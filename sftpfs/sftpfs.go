// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

// Package sftpfs implements an SFTP request handler that serves files from a
// container's root filesystem. All client paths are interpreted relative to a
// configured rootFS directory on the host and are confined to it, so an SFTP
// client cannot read or write outside the container's filesystem.
//
// Newly-created files, directories, and symlinks are chowned to a target
// uid/gid so that scp/sftp uploads land owned by the container user rather than
// by the (root) daemon process that actually performs the write.
package sftpfs

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/sftp"
)

// Handler implements the sftp.Handlers interfaces (FileReader, FileWriter,
// FileCmder, FileLister) for serving files from a container rootFS.
type Handler struct {
	rootFS  string // absolute path to container root
	homeDir string // user's home directory (container-relative, e.g. "/home/user")
	uid     int    // target user UID for newly-created files, or -1 to leave as-is
	gid     int    // target user GID for newly-created files, or -1 to leave as-is
}

// NewHandler returns a Handler that maps client paths through absRootFS (which
// must already be an absolute host path). homeDir is the container-relative
// start directory. uid/gid are applied to newly-created files; pass -1 for both
// to leave ownership untouched.
func NewHandler(absRootFS, homeDir string, uid, gid int) *Handler {
	return &Handler{rootFS: absRootFS, homeDir: homeDir, uid: uid, gid: gid}
}

// HomeDir returns the configured container-relative home directory, used by
// callers as the SFTP start directory.
func (h *Handler) HomeDir() string {
	return h.homeDir
}

// Handlers returns the sftp.Handlers set wired to this handler for all four
// SFTP request kinds.
func (h *Handler) Handlers() sftp.Handlers {
	return sftp.Handlers{
		FileGet:  h,
		FilePut:  h,
		FileCmd:  h,
		FileList: h,
	}
}

// chownNew sets ownership of a path newly created over SFTP to the target user.
// It is a no-op when no target uid/gid is configured (uid < 0).
func (h *Handler) chownNew(hostPath string) {
	if h.uid < 0 || h.gid < 0 {
		return
	}
	if err := os.Lchown(hostPath, h.uid, h.gid); err != nil {
		log.Printf("Warning: failed to chown %s to %d:%d: %v", hostPath, h.uid, h.gid, err)
	}
}

// toHostPath converts a container-relative path to an absolute host path.
// It ensures the resulting path is within the rootFS (no escaping via ..).
func (h *Handler) toHostPath(p string) (string, error) {
	// Clean the path to resolve . and ..
	cleaned := filepath.Clean("/" + p)

	// Join with rootFS
	hostPath := filepath.Join(h.rootFS, cleaned)

	// Verify the path is still within rootFS (prevent directory traversal attacks).
	if !strings.HasPrefix(hostPath, h.rootFS+"/") && hostPath != h.rootFS {
		return "", fmt.Errorf("path escapes container root: %s", p)
	}

	return hostPath, nil
}

// Fileread implements sftp.FileReader.
func (h *Handler) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	hostPath, err := h.toHostPath(r.Filepath)
	if err != nil {
		return nil, err
	}
	return os.Open(hostPath)
}

// Filewrite implements sftp.FileWriter.
func (h *Handler) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	hostPath, err := h.toHostPath(r.Filepath)
	if err != nil {
		return nil, err
	}

	// Determine flags from the SFTP request
	pflags := r.Pflags()
	flags := os.O_WRONLY
	if pflags.Creat {
		flags |= os.O_CREATE
	}
	if pflags.Trunc {
		flags |= os.O_TRUNC
	}
	if pflags.Append {
		flags |= os.O_APPEND
	}
	if pflags.Excl {
		flags |= os.O_EXCL
	}

	f, err := os.OpenFile(hostPath, flags, 0644)
	if err != nil {
		return nil, err
	}
	// New uploads should be owned by the target user, not by root (the
	// daemon process). Only chown when we actually created the file.
	if pflags.Creat {
		h.chownNew(hostPath)
	}
	return f, nil
}

// Filecmd implements sftp.FileCmder.
func (h *Handler) Filecmd(r *sftp.Request) error {
	hostPath, err := h.toHostPath(r.Filepath)
	if err != nil {
		return err
	}

	switch r.Method {
	case "Setstat":
		if r.AttrFlags().Size {
			if err := os.Truncate(hostPath, int64(r.Attributes().Size)); err != nil {
				return err
			}
		}
		if r.AttrFlags().Permissions {
			if err := os.Chmod(hostPath, r.Attributes().FileMode()); err != nil {
				return err
			}
		}
		return nil

	case "Rename":
		targetPath, err := h.toHostPath(r.Target)
		if err != nil {
			return err
		}
		return os.Rename(hostPath, targetPath)

	case "Rmdir":
		return os.Remove(hostPath)

	case "Remove":
		return os.Remove(hostPath)

	case "Mkdir":
		mode := os.FileMode(0755)
		if r.AttrFlags().Permissions {
			mode = r.Attributes().FileMode()
		}
		if err := os.Mkdir(hostPath, mode); err != nil {
			return err
		}
		h.chownNew(hostPath)
		return nil

	case "Symlink":
		// r.Filepath is the link name, r.Target is what it points to
		targetPath, err := h.toHostPath(r.Target)
		if err != nil {
			return err
		}
		if err := os.Symlink(targetPath, hostPath); err != nil {
			return err
		}
		h.chownNew(hostPath)
		return nil

	default:
		return fmt.Errorf("unsupported command: %s", r.Method)
	}
}

// Filelist implements sftp.FileLister.
func (h *Handler) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	hostPath, err := h.toHostPath(r.Filepath)
	if err != nil {
		return nil, err
	}

	switch r.Method {
	case "List":
		entries, err := os.ReadDir(hostPath)
		if err != nil {
			return nil, err
		}
		infos := make([]os.FileInfo, 0, len(entries))
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				continue // skip entries we can't stat
			}
			infos = append(infos, info)
		}
		return listerat(infos), nil

	case "Stat":
		info, err := os.Stat(hostPath)
		if err != nil {
			return nil, err
		}
		return listerat([]os.FileInfo{info}), nil

	case "Lstat":
		info, err := os.Lstat(hostPath)
		if err != nil {
			return nil, err
		}
		return listerat([]os.FileInfo{info}), nil

	case "Readlink":
		target, err := os.Readlink(hostPath)
		if err != nil {
			return nil, err
		}
		// Return a fake FileInfo with the link target as the name
		return listerat([]os.FileInfo{linkInfo{name: target}}), nil

	default:
		return nil, fmt.Errorf("unsupported list method: %s", r.Method)
	}
}

// listerat implements sftp.ListerAt for a slice of os.FileInfo.
type listerat []os.FileInfo

func (l listerat) ListAt(ls []os.FileInfo, offset int64) (int, error) {
	if offset >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(ls, l[offset:])
	if n < len(ls) {
		return n, io.EOF
	}
	return n, nil
}

// linkInfo is a minimal FileInfo for returning readlink results.
type linkInfo struct {
	name string
}

func (l linkInfo) Name() string       { return l.name }
func (l linkInfo) Size() int64        { return 0 }
func (l linkInfo) Mode() os.FileMode  { return os.ModeSymlink }
func (l linkInfo) ModTime() time.Time { return time.Time{} }
func (l linkInfo) IsDir() bool        { return false }
func (l linkInfo) Sys() any           { return nil }
