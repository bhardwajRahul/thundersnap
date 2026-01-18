//go:build linux

package bupdate

import (
	"fmt"
	"os"
	"syscall"
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
