// trivial-httpd serves a static directory over HTTP with full support for range requests.
// Only serves regular files and symlinks. For symlinks, returns the readlink() result.
// Opens files with O_NOFOLLOW and O_NONBLOCK for safety.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

var (
	addr = flag.String("addr", ":8080", "address to listen on")
	dir  = flag.String("dir", ".", "directory to serve")
)

func main() {
	flag.Parse()

	absDir, err := filepath.Abs(*dir)
	if err != nil {
		log.Fatalf("failed to resolve directory: %v", err)
	}

	handler := &fileServer{root: absDir}

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	fmt.Printf("Serving %s on http://%s\n", absDir, listener.Addr())

	server := &http.Server{Handler: handler}
	if err := server.Serve(listener); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// fileServer serves files from a directory with range request support.
type fileServer struct {
	root string
}

func (fs *fileServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Clean the path to prevent directory traversal
	cleanPath := filepath.Clean(r.URL.Path)
	if strings.HasPrefix(cleanPath, "..") {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	fullPath := filepath.Join(fs.root, cleanPath)

	// Ensure the path is within root
	if !strings.HasPrefix(fullPath, fs.root) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	// Use Lstat to check file type without following symlinks
	stat, err := os.Lstat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "not found", http.StatusNotFound)
		} else {
			http.Error(w, "error stating file", http.StatusInternalServerError)
		}
		return
	}

	mode := stat.Mode()

	// Only allow regular files and symlinks
	if !mode.IsRegular() && mode&os.ModeSymlink == 0 {
		http.Error(w, "not a regular file or symlink", http.StatusForbidden)
		return
	}

	// Handle symlinks: return the readlink() result as content
	if mode&os.ModeSymlink != 0 {
		target, err := os.Readlink(fullPath)
		if err != nil {
			http.Error(w, "error reading symlink", http.StatusInternalServerError)
			return
		}

		content := []byte(target)
		fileSize := int64(len(content))

		rangeHeader := r.Header.Get("Range")
		if rangeHeader == "" {
			w.Header().Set("Content-Length", strconv.FormatInt(fileSize, 10))
			w.Header().Set("Accept-Ranges", "bytes")
			w.WriteHeader(http.StatusOK)
			w.Write(content)
			return
		}

		// Handle range request for symlink content
		start, end, err := parseRangeHeader(rangeHeader, fileSize)
		if err != nil {
			http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
			return
		}

		contentLength := end - start + 1
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, fileSize))
		w.Header().Set("Content-Length", strconv.FormatInt(contentLength, 10))
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusPartialContent)
		w.Write(content[start : end+1])
		return
	}

	// Regular file: open with O_NOFOLLOW and O_NONBLOCK
	fd, err := syscall.Open(fullPath, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if err == syscall.ELOOP {
			http.Error(w, "unexpected symlink", http.StatusForbidden)
		} else if err == syscall.ENOENT {
			http.Error(w, "not found", http.StatusNotFound)
		} else {
			http.Error(w, "error opening file", http.StatusInternalServerError)
		}
		return
	}
	f := os.NewFile(uintptr(fd), fullPath)
	defer f.Close()

	fileSize := stat.Size()

	// Check for Range header
	rangeHeader := r.Header.Get("Range")
	if rangeHeader == "" {
		// No range request - serve entire file
		w.Header().Set("Content-Length", strconv.FormatInt(fileSize, 10))
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusOK)
		io.Copy(w, f)
		return
	}

	// Parse Range header
	start, end, err := parseRangeHeader(rangeHeader, fileSize)
	if err != nil {
		http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
		return
	}

	// Seek to start position
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		http.Error(w, "seek error", http.StatusInternalServerError)
		return
	}

	// Calculate content length
	contentLength := end - start + 1

	// Set headers for partial content
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, fileSize))
	w.Header().Set("Content-Length", strconv.FormatInt(contentLength, 10))
	w.Header().Set("Accept-Ranges", "bytes")
	w.WriteHeader(http.StatusPartialContent)

	// Copy the requested range
	io.CopyN(w, f, contentLength)
}

// parseRangeHeader parses a Range header like "bytes=0-99" and returns start and end positions.
func parseRangeHeader(header string, fileSize int64) (start, end int64, err error) {
	// Must start with "bytes="
	if !strings.HasPrefix(header, "bytes=") {
		return 0, 0, fmt.Errorf("invalid range prefix")
	}

	rangeSpec := strings.TrimPrefix(header, "bytes=")

	// Split on comma for multiple ranges (we only support single range)
	ranges := strings.Split(rangeSpec, ",")
	if len(ranges) != 1 {
		return 0, 0, fmt.Errorf("multiple ranges not supported")
	}

	// Parse the range
	parts := strings.Split(strings.TrimSpace(ranges[0]), "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid range format")
	}

	startStr := strings.TrimSpace(parts[0])
	endStr := strings.TrimSpace(parts[1])

	if startStr == "" {
		// Suffix range: -500 means last 500 bytes
		if endStr == "" {
			return 0, 0, fmt.Errorf("invalid range: empty")
		}
		suffixLen, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid suffix length")
		}
		start = fileSize - suffixLen
		if start < 0 {
			start = 0
		}
		end = fileSize - 1
	} else {
		// Regular range: start-end
		start, err = strconv.ParseInt(startStr, 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid start")
		}

		if endStr == "" {
			// Open-ended range: start-
			end = fileSize - 1
		} else {
			end, err = strconv.ParseInt(endStr, 10, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("invalid end")
			}
		}
	}

	// Validate range
	if start < 0 || start >= fileSize {
		return 0, 0, fmt.Errorf("start out of range")
	}
	if end >= fileSize {
		end = fileSize - 1
	}
	if start > end {
		return 0, 0, fmt.Errorf("start > end")
	}

	return start, end, nil
}
