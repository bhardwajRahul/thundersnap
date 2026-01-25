//go:build linux

package bupdate

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// FICLONE is the ioctl number for btrfs/reflink clone
const FICLONE = 0x40049409

// CloneFile creates a COW clone of src at dst using FICLONE ioctl.
// Both files must be on the same btrfs/xfs/etc filesystem that supports reflinks.
// Returns nil on success, or an error if cloning is not supported or fails.
func CloneFile(dst, src string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening source: %w", err)
	}
	defer srcFile.Close()

	// Remove destination if it exists (to avoid following symlinks)
	os.Remove(dst)

	dstFile, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("creating destination: %w", err)
	}
	defer dstFile.Close()

	// Perform FICLONE ioctl - the third argument is the source fd directly (not a pointer)
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, dstFile.Fd(), FICLONE, srcFile.Fd())
	if errno != 0 {
		os.Remove(dst)
		return fmt.Errorf("FICLONE ioctl: %w", errno)
	}

	return nil
}

// CopyFile copies src to dst using copy_file_range() for efficient kernel-level copying.
// This is faster than read/write loops since data doesn't pass through userspace.
// Works on NOCOW files where FICLONE fails.
func CopyFile(dst, src string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening source: %w", err)
	}
	defer srcFile.Close()

	srcInfo, err := srcFile.Stat()
	if err != nil {
		return fmt.Errorf("stat source: %w", err)
	}

	// Remove destination if it exists (to avoid following symlinks)
	os.Remove(dst)

	dstFile, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("creating destination: %w", err)
	}
	defer dstFile.Close()

	// Pre-allocate the destination file
	size := srcInfo.Size()
	if err := dstFile.Truncate(size); err != nil {
		os.Remove(dst)
		return fmt.Errorf("truncate: %w", err)
	}

	// Use copy_file_range to copy in chunks (max 1GB per call is a reasonable limit)
	const maxCopySize = 1 << 30 // 1GB
	var srcOff, dstOff int64

	for srcOff < size {
		toWrite := size - srcOff
		if toWrite > maxCopySize {
			toWrite = maxCopySize
		}

		n, err := unix.CopyFileRange(int(srcFile.Fd()), &srcOff, int(dstFile.Fd()), &dstOff, int(toWrite), 0)
		if err != nil {
			os.Remove(dst)
			return fmt.Errorf("copy_file_range: %w", err)
		}
		if n == 0 {
			break // EOF
		}
	}

	return nil
}

// CloneOrCopyFile tries FICLONE first for zero-copy COW clone, and falls back
// to copy_file_range() if FICLONE fails (e.g., for NOCOW files).
// Returns whether a COW clone was used (true) or a copy (false).
func CloneOrCopyFile(dst, src string) (cloned bool, err error) {
	// Try FICLONE first
	if err := CloneFile(dst, src); err == nil {
		return true, nil
	}

	// Fall back to copy_file_range
	if err := CopyFile(dst, src); err != nil {
		return false, err
	}
	return false, nil
}
