// Package frameid provides frame identity (UUID) generation and handling.
//
// Frame UUIDs use UUIDv7, which is time-ordered (sortable by creation time)
// while maintaining global uniqueness. This is useful for debugging and log
// analysis since frames created earlier will sort before frames created later.
package frameid

import (
	"fmt"

	"github.com/google/uuid"
)

// ID represents a frame identity as a UUIDv7.
type ID = uuid.UUID

// New generates a new frame ID using UUIDv7.
// UUIDv7 is time-ordered, so IDs sort chronologically by creation time.
func New() (ID, error) {
	return uuid.NewV7()
}

// MustNew generates a new frame ID, panicking on error.
// Use this only in contexts where errors are not expected (e.g., initialization).
func MustNew() ID {
	id, err := New()
	if err != nil {
		panic(fmt.Sprintf("frameid: failed to generate UUIDv7: %v", err))
	}
	return id
}

// Parse parses a frame ID from its string representation.
func Parse(s string) (ID, error) {
	return uuid.Parse(s)
}

// MustParse parses a frame ID, panicking on error.
func MustParse(s string) ID {
	return uuid.MustParse(s)
}

// IsZero reports whether id is the zero (nil) UUID.
func IsZero(id ID) bool {
	return id == uuid.Nil
}

// Nil is the zero UUID, used to represent "no frame".
var Nil = uuid.Nil
