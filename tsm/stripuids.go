package tsm

// Simplified UID model: all non-root users in a container map to a single
// shared UID. Root keeps UID 0; everyone else (postgres, www-data, etc.)
// resolves via /etc/passwd to one common UID. See strip-all-uids-design.md.
//
// This file provides helpers to:
//   1. Rewrite /etc/passwd so every non-root entry uses the shared UID.
//   2. Rewrite /etc/group similarly so every non-root entry uses the shared GID.
//   3. Walk a rootfs and chown every non-root file to the shared UID/GID,
//      preserving the setuid/setgid/sticky bits.
//
// We intentionally keep the operation idempotent: applying it twice
// produces the same result as applying it once.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// DefaultSharedUID is the UID used for all non-root accounts inside a
// container after stripping. We pick 1000 because that matches the
// conventional first user UID on Debian/Ubuntu, so existing files owned
// by uid=1000 in the snapshot continue to work without touching them.
const DefaultSharedUID = 1000

// DefaultSharedGID mirrors the above for groups.
const DefaultSharedGID = 1000

// StripOptions configures how UID stripping is applied.
type StripOptions struct {
	// SharedUID is the UID assigned to all non-root accounts.
	// If 0 (the zero value), DefaultSharedUID is used.
	SharedUID uint32

	// SharedGID is the GID assigned to all non-root groups.
	// If 0, DefaultSharedGID is used.
	SharedGID uint32

	// ChownFiles, if true, walks the rootfs and chowns every file
	// owned by a non-root UID/GID to the shared UID/GID. This is a
	// one-time fix-up done at rootfs creation. Skipped on the snapshots
	// dir because those subvolumes are read-only.
	ChownFiles bool
}

func (o StripOptions) uid() uint32 {
	if o.SharedUID == 0 {
		return DefaultSharedUID
	}
	return o.SharedUID
}

func (o StripOptions) gid() uint32 {
	if o.SharedGID == 0 {
		return DefaultSharedGID
	}
	return o.SharedGID
}

// StripPasswdFile rewrites the /etc/passwd file at path so every entry
// other than root (UID 0) is rewritten to use sharedUID/sharedGID.
// The home directory and shell fields are left untouched.
//
// Returns nil if the file does not exist.
func StripPasswdFile(path string, opts StripOptions) error {
	uid := opts.uid()
	gid := opts.gid()

	in, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	var out strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(string(in)))
	// The default Scanner buffer is 64KB which can be too small for very
	// large passwd files; bump it.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		// Preserve comments / blank lines verbatim.
		if line == "" || strings.HasPrefix(line, "#") {
			out.WriteString(line)
			out.WriteByte('\n')
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 7 {
			// Malformed line; preserve as-is.
			out.WriteString(line)
			out.WriteByte('\n')
			continue
		}
		// Skip root (UID 0).
		curUID, err := strconv.ParseUint(fields[2], 10, 32)
		if err == nil && curUID == 0 {
			out.WriteString(line)
			out.WriteByte('\n')
			continue
		}
		fields[2] = strconv.FormatUint(uint64(uid), 10)
		fields[3] = strconv.FormatUint(uint64(gid), 10)
		out.WriteString(strings.Join(fields, ":"))
		out.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan passwd: %w", err)
	}
	return atomicWriteFile(path, []byte(out.String()), 0644)
}

// StripGroupFile rewrites /etc/group so every group with non-zero GID uses
// sharedGID. The membership list (fourth field) is preserved.
func StripGroupFile(path string, opts StripOptions) error {
	gid := opts.gid()

	in, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	var out strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(string(in)))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			out.WriteString(line)
			out.WriteByte('\n')
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 4 {
			out.WriteString(line)
			out.WriteByte('\n')
			continue
		}
		curGID, err := strconv.ParseUint(fields[2], 10, 32)
		if err == nil && curGID == 0 {
			out.WriteString(line)
			out.WriteByte('\n')
			continue
		}
		fields[2] = strconv.FormatUint(uint64(gid), 10)
		out.WriteString(strings.Join(fields, ":"))
		out.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan group: %w", err)
	}
	return atomicWriteFile(path, []byte(out.String()), 0644)
}

// ChownNonRootFiles walks rootfs and re-owns every entry not already owned
// by uid 0 / gid 0 to the shared UID/GID. Symlinks are chowned (lchown).
// Errors on individual files are logged via logf (if non-nil) but do not
// abort the walk.
func ChownNonRootFiles(rootfs string, opts StripOptions, logf func(format string, args ...any)) error {
	uid := opts.uid()
	gid := opts.gid()

	if logf == nil {
		logf = func(string, ...any) {}
	}

	return filepath.Walk(rootfs, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// Don't abort: a missing file or unreadable directory inside
			// the snapshot shouldn't prevent the rest from being chowned.
			logf("walk error at %s: %v", path, err)
			if info != nil && info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil
		}
		// Decide new UID/GID: keep root, otherwise rewrite.
		newUID := stat.Uid
		newGID := stat.Gid
		if newUID != 0 {
			newUID = uid
		}
		if newGID != 0 {
			newGID = gid
		}
		if newUID == stat.Uid && newGID == stat.Gid {
			return nil
		}
		// Use Lchown so we don't follow symlinks.
		if err := os.Lchown(path, int(newUID), int(newGID)); err != nil {
			logf("lchown %s: %v", path, err)
		}
		return nil
	})
}

// StripRootfs is the top-level convenience function: rewrite passwd and
// group, and (if opts.ChownFiles) walk the rootfs to align ownership.
// Operations missing a file are skipped silently.
func StripRootfs(rootfs string, opts StripOptions) error {
	if err := StripPasswdFile(filepath.Join(rootfs, "etc", "passwd"), opts); err != nil {
		return fmt.Errorf("strip passwd: %w", err)
	}
	if err := StripGroupFile(filepath.Join(rootfs, "etc", "group"), opts); err != nil {
		return fmt.Errorf("strip group: %w", err)
	}
	// /etc/shadow and /etc/gshadow contain no UID/GID columns, so they
	// don't need modification — usernames remain the same, just with
	// different UIDs in passwd.
	if opts.ChownFiles {
		if err := ChownNonRootFiles(rootfs, opts, nil); err != nil {
			return fmt.Errorf("chown rootfs: %w", err)
		}
	}
	return nil
}

// atomicWriteFile writes data to path via a temp file + rename, preserving
// the existing file's mode if it exists; otherwise uses defaultMode.
func atomicWriteFile(path string, data []byte, defaultMode os.FileMode) error {
	mode := defaultMode
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".strip-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		// Best effort cleanup if rename didn't happen.
		os.Remove(tmpName)
	}()
	if _, err := io.Copy(tmp, strings.NewReader(string(data))); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
