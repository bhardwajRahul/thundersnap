// Package snaphash provides encoding and decoding of snap hashes.
//
// Snap hashes are 256-bit SHA256 values. Rather than hex encoding (64 chars),
// we use base64url (RFC 4648) with a 1-bit shift for better ergonomics.
//
// The 1-bit shift ensures snap IDs never start with '-' or '_' (which could
// be confused with command-line flags). We prepend a 0 bit before encoding:
//
//	Encoding: hash (256 bits) → prepend 0 bit → 257 bits → base64url → 43 chars
//	Decoding: 43 chars → base64url → 257 bits → drop first bit → 256 bits → hash
//
// The first base64url character encodes 6 bits. With the prepended 0 bit, the
// first char encodes `0` + bits 1-5 of the original hash. This means the first
// char can only have values 0-31 in the base64url alphabet (A-Z, a-f), never
// '-' (62) or '_' (63).
package snaphash

import (
	"crypto/sha256"
	"errors"
	"fmt"
)

// Size is the size of a snap hash in bytes (256 bits).
const Size = sha256.Size // 32 bytes

// EncodedSize is the size of an encoded snap hash string.
const EncodedSize = 43

// Hash represents a 256-bit snap hash.
type Hash [Size]byte

var (
	// ErrInvalidLength is returned when the encoded string has wrong length.
	ErrInvalidLength = errors.New("snaphash: invalid encoded length")
	// ErrInvalidEncoding is returned when base64url decoding fails.
	ErrInvalidEncoding = errors.New("snaphash: invalid base64url encoding")
)

// Encode encodes a 256-bit hash to a 43-character base64url string.
// The encoding prepends a 0 bit to ensure the result never starts with '-' or '_'.
func Encode(h Hash) string {
	// We need to encode 256 bits with a prepended 0 bit = 257 bits.
	// 257 bits / 6 bits per char = 42.83, rounds up to 43 chars.
	// 43 chars * 6 bits = 258 bits, so we have 1 bit of padding at the end.
	//
	// We'll encode directly to base64url characters rather than going through
	// bytes, to avoid the extra padding issues.

	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

	var result [EncodedSize]byte

	// We have 257 bits to encode: 1 zero bit + 256 hash bits.
	// Each output char encodes 6 bits.
	// Char 0: bits 0-5 of the 257-bit value = 0 + bits 0-4 of hash
	// Char 1: bits 6-11 = bits 5-10 of hash
	// ... and so on

	// Treat the hash as a big-endian bit stream.
	// For char i, we need bits [i*6-1, i*6+4] of the original hash (with -1 meaning the prepended 0).

	for i := 0; i < EncodedSize; i++ {
		// Bit position in the 257-bit value (0 is the prepended zero, 1-256 are the hash bits).
		startBit := i * 6

		// Extract 6 bits starting at startBit.
		var val byte
		for b := 0; b < 6; b++ {
			bitPos := startBit + b
			if bitPos == 0 {
				// The prepended zero bit - contributes 0.
				continue
			}
			// bitPos 1-256 corresponds to hash bit 0-255.
			hashBit := bitPos - 1
			if hashBit < 256 {
				byteIdx := hashBit / 8
				bitInByte := 7 - (hashBit % 8) // MSB first
				if (h[byteIdx]>>bitInByte)&1 == 1 {
					val |= 1 << (5 - b)
				}
			}
			// bitPos >= 257 contributes 0 (padding).
		}
		result[i] = alphabet[val]
	}

	return string(result[:])
}

// Decode decodes a 43-character base64url string to a 256-bit hash.
func Decode(s string) (Hash, error) {
	var h Hash

	if len(s) != EncodedSize {
		return h, fmt.Errorf("%w: got %d, want %d", ErrInvalidLength, len(s), EncodedSize)
	}

	// Decode each base64url character to its 6-bit value.
	var vals [EncodedSize]byte
	for i := 0; i < EncodedSize; i++ {
		c := s[i]
		var v byte
		switch {
		case c >= 'A' && c <= 'Z':
			v = c - 'A'
		case c >= 'a' && c <= 'z':
			v = c - 'a' + 26
		case c >= '0' && c <= '9':
			v = c - '0' + 52
		case c == '-':
			v = 62
		case c == '_':
			v = 63
		default:
			return h, fmt.Errorf("%w: invalid character %q at position %d", ErrInvalidEncoding, c, i)
		}
		vals[i] = v
	}

	// We have 43 chars * 6 bits = 258 bits.
	// Bit 0 is the prepended zero (should be 0).
	// Bits 1-256 are the hash.
	// Bits 257 is padding (ignored).

	// Extract hash bits 0-255 from position 1-256 of the encoded value.
	for hashBit := 0; hashBit < 256; hashBit++ {
		// This corresponds to bit position hashBit+1 in the 258-bit encoded value.
		encBit := hashBit + 1
		charIdx := encBit / 6
		bitInChar := encBit % 6
		if (vals[charIdx]>>(5-bitInChar))&1 == 1 {
			byteIdx := hashBit / 8
			bitInByte := 7 - (hashBit % 8)
			h[byteIdx] |= 1 << bitInByte
		}
	}

	return h, nil
}

// FromBytes creates a Hash from a byte slice.
// Panics if the slice is not exactly 32 bytes.
func FromBytes(b []byte) Hash {
	if len(b) != Size {
		panic(fmt.Sprintf("snaphash: FromBytes requires %d bytes, got %d", Size, len(b)))
	}
	var h Hash
	copy(h[:], b)
	return h
}

// Sum computes the SHA256 hash of data and returns it as a Hash.
func Sum(data []byte) Hash {
	return sha256.Sum256(data)
}

// String returns the encoded string representation of the hash.
func (h Hash) String() string {
	return Encode(h)
}

// IsZero reports whether h is the zero hash.
func (h Hash) IsZero() bool {
	return h == Hash{}
}

// MarshalText implements encoding.TextMarshaler.
func (h Hash) MarshalText() ([]byte, error) {
	return []byte(Encode(h)), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (h *Hash) UnmarshalText(text []byte) error {
	decoded, err := Decode(string(text))
	if err != nil {
		return err
	}
	*h = decoded
	return nil
}

// ParseHash parses an encoded snap hash string.
func ParseHash(s string) (Hash, error) {
	return Decode(s)
}
