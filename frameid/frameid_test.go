package frameid

import (
	"testing"
	"time"
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
		"12345678-1234-1234-1234-12345678901",  // too short
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
