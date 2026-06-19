package infiniblock

import (
	"os"

	"golang.org/x/sys/unix"
)

// Backend is the interface for block storage backends.
type Backend interface {
	// ReadAt reads len(p) bytes starting at offset.
	ReadAt(p []byte, off int64) (int, error)
	// WriteAt writes p starting at offset.
	WriteAt(p []byte, off int64) (int, error)
	// Trim punches a hole in the range [off, off+length).
	Trim(off int64, length int64) error
	// Sync flushes data to stable storage.
	Sync() error
	// Close closes the backend.
	Close() error
	// AllocatedSize returns the actual allocated size on disk.
	AllocatedSize() (int64, error)
	// Size returns the current file size (logical size).
	Size() (int64, error)
}

// SparseFile implements Backend using a sparse file.
type SparseFile struct {
	f *os.File
}

// OpenSparseFile opens or creates a sparse file at path.
func OpenSparseFile(path string) (*SparseFile, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, err
	}
	return &SparseFile{f: f}, nil
}

// ReadAt reads len(p) bytes starting at offset.
// Reading from a hole returns zeros (kernel/filesystem handles this).
func (s *SparseFile) ReadAt(p []byte, off int64) (int, error) {
	return s.f.ReadAt(p, off)
}

// WriteAt writes p starting at offset.
// Writing past EOF automatically extends the file (sparse).
func (s *SparseFile) WriteAt(p []byte, off int64) (int, error) {
	return s.f.WriteAt(p, off)
}

// Trim punches a hole in the range [off, off+length).
// This deallocates the underlying disk blocks.
func (s *SparseFile) Trim(off int64, length int64) error {
	return unix.Fallocate(int(s.f.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, off, length)
}

// Sync flushes data to stable storage.
func (s *SparseFile) Sync() error {
	return s.f.Sync()
}

// Close closes the file.
func (s *SparseFile) Close() error {
	return s.f.Close()
}

// AllocatedSize returns the actual allocated size on disk (excluding holes).
func (s *SparseFile) AllocatedSize() (int64, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(s.f.Fd()), &stat); err != nil {
		return 0, err
	}
	// stat.Blocks is in 512-byte units
	return stat.Blocks * 512, nil
}

// Size returns the current file size (logical size including holes).
func (s *SparseFile) Size() (int64, error) {
	fi, err := s.f.Stat()
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// File returns the underlying os.File (for testing).
func (s *SparseFile) File() *os.File {
	return s.f
}
