// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package frameid

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNew(t *testing.T) {
	id, err := New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	// Should not be the nil UUID.
	if IsZero(id) {
		t.Error("New() returned nil UUID")
	}

	// Should be a valid UUID string (36 chars with hyphens).
	s := id.String()
	if len(s) != 36 {
		t.Errorf("UUID string length = %d, want 36", len(s))
	}
}

func TestMustNew(t *testing.T) {
	// MustNew should not panic under normal conditions.
	id := MustNew()
	if IsZero(id) {
		t.Error("MustNew() returned nil UUID")
	}
}

func TestParse(t *testing.T) {
	id := MustNew()
	s := id.String()

	parsed, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q) error: %v", s, err)
	}
	if parsed != id {
		t.Errorf("Parse round-trip failed: got %v, want %v", parsed, id)
	}
}

func TestParseInvalid(t *testing.T) {
	invalidUUIDs := []string{
		"",
		"not-a-uuid",
		"12345678-1234-1234-1234-12345678901",   // too short
		"12345678-1234-1234-1234-1234567890123", // too long
	}

	for _, s := range invalidUUIDs {
		_, err := Parse(s)
		if err == nil {
			t.Errorf("Parse(%q) should have failed", s)
		}
	}
}

func TestMustParse(t *testing.T) {
	// Valid UUID should not panic.
	id := MustNew()
	parsed := MustParse(id.String())
	if parsed != id {
		t.Error("MustParse round-trip failed")
	}
}

func TestMustParsePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("MustParse with invalid UUID should panic")
		}
	}()
	MustParse("invalid")
}

func TestIsZero(t *testing.T) {
	if !IsZero(Nil) {
		t.Error("IsZero(Nil) should be true")
	}

	id := MustNew()
	if IsZero(id) {
		t.Error("IsZero(new ID) should be false")
	}
}

func TestNewDistinct(t *testing.T) {
	// Two consecutive New() calls must produce distinct IDs even with no
	// delay between them (UUIDv7 mixes in random bits beyond the timestamp).
	a := MustNew()
	b := MustNew()
	if a == b {
		t.Errorf("consecutive New() calls produced identical IDs: %v", a)
	}
}

func TestParseAcceptsNonV7(t *testing.T) {
	// Parse delegates to uuid.Parse, which accepts any UUID version. Despite
	// the package being documented around UUIDv7, Parse does NOT enforce the
	// version; this test pins that intentional leniency so a future change to
	// reject non-v7 input is a conscious decision.
	v4 := uuid.New() // random (version 4)
	if got := v4.Version(); got != 4 {
		t.Fatalf("uuid.New() version = %d, want 4", got)
	}
	parsed, err := Parse(v4.String())
	if err != nil {
		t.Errorf("Parse of a v4 UUID should succeed, got error: %v", err)
	}
	if parsed != v4 {
		t.Errorf("Parse(%q) = %v, want %v", v4.String(), parsed, v4)
	}
}

func TestParseNilRoundTrip(t *testing.T) {
	// Parsing the all-zero UUID yields a value for which IsZero is true.
	const nilStr = "00000000-0000-0000-0000-000000000000"
	parsed, err := Parse(nilStr)
	if err != nil {
		t.Fatalf("Parse(%q) error: %v", nilStr, err)
	}
	if !IsZero(parsed) {
		t.Errorf("IsZero(Parse(%q)) = false, want true", nilStr)
	}
	if parsed != Nil {
		t.Errorf("Parse(%q) = %v, want Nil", nilStr, parsed)
	}
}

func TestTimeOrdering(t *testing.T) {
	// UUIDv7 should be time-ordered. Generate several UUIDs and verify
	// their string representations sort chronologically.
	var ids []ID
	for i := 0; i < 10; i++ {
		id := MustNew()
		ids = append(ids, id)
		// Small delay to ensure different timestamps.
		time.Sleep(time.Millisecond)
	}

	for i := 1; i < len(ids); i++ {
		// UUIDv7 strings should sort in creation order due to time prefix.
		if ids[i-1].String() >= ids[i].String() {
			t.Errorf("UUIDs not in order: %s >= %s", ids[i-1], ids[i])
		}
	}
}
