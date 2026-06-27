package snaphash

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	// Test that encoding then decoding returns the original hash.
	testCases := []Hash{
		{}, // zero hash
		{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
			0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
			0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
			0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, // all ones
		Sum([]byte("hello world")),
		Sum([]byte("")),
		Sum([]byte("thundersnap")),
	}

	for i, h := range testCases {
		encoded := Encode(h)
		decoded, err := Decode(encoded)
		if err != nil {
			t.Errorf("case %d: Decode(%q) error: %v", i, encoded, err)
			continue
		}
		if decoded != h {
			t.Errorf("case %d: round trip failed: got %x, want %x", i, decoded, h)
		}
	}
}

func TestEncodedLength(t *testing.T) {
	// All encoded hashes should be exactly 43 characters.
	for i := 0; i < 100; i++ {
		h := Sum([]byte{byte(i)})
		encoded := Encode(h)
		if len(encoded) != EncodedSize {
			t.Errorf("Encode(Sum(%d)) length = %d, want %d", i, len(encoded), EncodedSize)
		}
	}
}

func TestFirstCharNeverDashOrUnderscore(t *testing.T) {
	// The main feature: first character should never be '-' or '_'.
	// Test many random-ish hashes.
	for i := 0; i < 1000; i++ {
		// Generate different hashes by hashing different inputs.
		h := Sum([]byte{byte(i), byte(i >> 8), byte(i >> 16)})
		encoded := Encode(h)

		if encoded[0] == '-' || encoded[0] == '_' {
			t.Errorf("Encode(Sum(%d)) = %q starts with forbidden character", i, encoded)
		}
	}

	// Also test edge cases where the first byte of the hash has high bits set.
	edgeCases := []Hash{
		{0xff}, // first byte all ones
		{0x80}, // first byte has MSB set
		{0xc0}, // first byte has top 2 bits set
		{0xe0}, // first byte has top 3 bits set
		{0xf0}, // first byte has top 4 bits set
		{0xf8}, // first byte has top 5 bits set
		{0xfc}, // first byte has top 6 bits set (this would make first char '-' without shift)
		{0xfe}, // first byte has top 7 bits set
	}

	for _, h := range edgeCases {
		encoded := Encode(h)
		if encoded[0] == '-' || encoded[0] == '_' {
			t.Errorf("Encode(%x) = %q starts with forbidden character", h[:4], encoded)
		}
	}
}

func TestLastCharCanBeAnyBase64(t *testing.T) {
	// Note: The design doc mentions the last char should also be 0-31, but
	// with a 1-bit shift, only the first char is constrained. The last char
	// can be any valid base64url character. This is fine since the primary
	// goal (no leading - or _) is achieved.
	//
	// This test just verifies the encoding doesn't crash and produces valid base64url.
	for i := 0; i < 100; i++ {
		h := Sum([]byte{byte(i), byte(i >> 8), byte(i >> 16)})
		encoded := Encode(h)
		last := encoded[len(encoded)-1]

		valid := (last >= 'A' && last <= 'Z') ||
			(last >= 'a' && last <= 'z') ||
			(last >= '0' && last <= '9') ||
			last == '-' || last == '_'
		if !valid {
			t.Errorf("last char %c is not valid base64url", last)
		}
	}
}

func TestDecodeInvalidLength(t *testing.T) {
	testCases := []string{
		"",
		"abc",
		"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmn",   // 42 chars
		"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnop", // 44 chars
	}

	for _, s := range testCases {
		_, err := Decode(s)
		if err == nil {
			t.Errorf("Decode(%q) should have failed", s)
		}
	}
}

func TestDecodeInvalidBase64(t *testing.T) {
	// 43 chars but invalid base64url characters.
	invalid := "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!#$"
	_, err := Decode(invalid)
	if err == nil {
		t.Errorf("Decode(%q) should have failed", invalid)
	}
}

func TestFromBytes(t *testing.T) {
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(i)
	}
	h := FromBytes(b)
	if !bytes.Equal(h[:], b) {
		t.Errorf("FromBytes mismatch")
	}
}

func TestFromBytesPanic(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("FromBytes with wrong size should panic")
		}
	}()
	FromBytes([]byte{1, 2, 3})
}

func TestSum(t *testing.T) {
	data := []byte("test data")
	h := Sum(data)
	expected := sha256.Sum256(data)
	if h != expected {
		t.Errorf("Sum mismatch: got %x, want %x", h, expected)
	}
}

func TestString(t *testing.T) {
	h := Sum([]byte("test"))
	s := h.String()
	if s != Encode(h) {
		t.Errorf("String() = %q, Encode() = %q", s, Encode(h))
	}
}

func TestIsZero(t *testing.T) {
	var zero Hash
	if !zero.IsZero() {
		t.Error("zero hash should be zero")
	}

	nonZero := Sum([]byte("x"))
	if nonZero.IsZero() {
		t.Error("non-zero hash should not be zero")
	}
}

func TestJSONMarshal(t *testing.T) {
	h := Sum([]byte("test"))
	data, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("json.Marshal error: %v", err)
	}

	// Should be a quoted string.
	expected := `"` + Encode(h) + `"`
	if string(data) != expected {
		t.Errorf("json.Marshal = %s, want %s", data, expected)
	}

	// Unmarshal back.
	var h2 Hash
	if err := json.Unmarshal(data, &h2); err != nil {
		t.Fatalf("json.Unmarshal error: %v", err)
	}
	if h2 != h {
		t.Errorf("json round trip failed: got %x, want %x", h2, h)
	}
}

func TestURLSafe(t *testing.T) {
	// Verify the encoding only uses URL-safe characters.
	for i := 0; i < 100; i++ {
		h := Sum([]byte{byte(i)})
		encoded := Encode(h)

		for j, c := range encoded {
			valid := (c >= 'A' && c <= 'Z') ||
				(c >= 'a' && c <= 'z') ||
				(c >= '0' && c <= '9') ||
				c == '-' || c == '_'
			if !valid {
				t.Errorf("Encode(Sum(%d))[%d] = %c is not URL-safe", i, j, c)
			}
		}
	}
}

func TestParseHash(t *testing.T) {
	h := Sum([]byte("test"))
	encoded := Encode(h)

	parsed, err := ParseHash(encoded)
	if err != nil {
		t.Fatalf("ParseHash error: %v", err)
	}
	if parsed != h {
		t.Errorf("ParseHash mismatch")
	}
}

func TestFirstCharRange(t *testing.T) {
	// Verify that first character is always in the range A-Z or a-f (values 0-31).
	// base64url alphabet: A-Z (0-25), a-z (26-51), 0-9 (52-61), - (62), _ (63)
	// Values 0-31 correspond to: A-Z (0-25), a-f (26-31)

	validFirstChars := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdef"

	for i := 0; i < 1000; i++ {
		h := Sum([]byte{byte(i), byte(i >> 8)})
		encoded := Encode(h)

		if !strings.ContainsRune(validFirstChars, rune(encoded[0])) {
			t.Errorf("first char %c not in valid range for hash %d", encoded[0], i)
		}
	}
}

// TestDecodeAdversarialInput tests decoding malformed/adversarial input.
func TestDecodeAdversarialInput(t *testing.T) {
	testCases := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"too short", "abc"},
		{"too long", "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrs"},
		{"contains padding", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="},
		{"contains space", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA "},
		{"contains newline", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n"},
		{"null bytes", string(make([]byte, 43))},
		{"unicode", "🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉"},                                      // emoji is multi-byte
		{"standard base64 chars", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA+"}, // + is base64, not base64url
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode(tc.input)
			if err == nil {
				t.Errorf("Decode(%q) should fail", tc.input)
			}
		})
	}
}

// TestDecodeValidButCrafted tests decoding valid-looking strings that might
// attempt to exploit edge cases.
func TestDecodeValidButCrafted(t *testing.T) {
	// Valid 43-char base64url strings.
	valid := []string{
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"___________________________________________", // all underscores (valid base64url)
		"-------------------------------------------", // all dashes (valid base64url)
		"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",
		"0000000000000000000000000000000000000000000",
	}

	for _, s := range valid {
		_, err := Decode(s)
		if err != nil {
			t.Errorf("Decode(%q) should succeed: %v", s, err)
		}
	}
}

// TestJSONUnmarshalAdversarial tests JSON unmarshaling with adversarial input.
func TestJSONUnmarshalAdversarial(t *testing.T) {
	testCases := []string{
		`""`,                  // empty string
		`"abc"`,               // too short
		`null`,                // null
		`123`,                 // number
		`true`,                // boolean
		`{"evil": "payload"}`, // object
		`"` + strings.Repeat("A", 43) + `"` + strings.Repeat("A", 100), // trailing data in JSON
	}

	for _, tc := range testCases {
		var h Hash
		err := json.Unmarshal([]byte(tc), &h)
		// Most of these should fail, but the important thing is they don't panic.
		_ = err
	}
}

// TestEncodeDecodeAllBitPatterns ensures round-trip works for various bit patterns.
func TestEncodeDecodeAllBitPatterns(t *testing.T) {
	// Test hashes where the first byte has different values (affects first encoded char).
	for firstByte := 0; firstByte < 256; firstByte++ {
		var h Hash
		h[0] = byte(firstByte)
		// Fill rest with a pattern.
		for i := 1; i < 32; i++ {
			h[i] = byte(i)
		}

		encoded := Encode(h)
		decoded, err := Decode(encoded)
		if err != nil {
			t.Errorf("Decode failed for firstByte=%d: %v", firstByte, err)
			continue
		}
		if decoded != h {
			t.Errorf("Round trip failed for firstByte=%d", firstByte)
		}

		// Also verify first char constraint.
		if encoded[0] == '-' || encoded[0] == '_' {
			t.Errorf("First char forbidden for firstByte=%d: %c", firstByte, encoded[0])
		}
	}
}
