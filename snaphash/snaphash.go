// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

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
	// ErrNonCanonical is returned when an otherwise-valid encoded string is not
	// the canonical encoding of its hash: its prepended bit or trailing padding
	// bit is set. Such a string would decode to the same hash as the canonical
	// form, so accepting it would make Decode non-injective.
	ErrNonCanonical = errors.New("snaphash: non-canonical encoding")
)

// getHashBit reports whether bit hashBit (0 = MSB of byte 0) of h is set.
func getHashBit(h *Hash, hashBit int) bool {
	byteIdx := hashBit / 8
	bitInByte := 7 - (hashBit % 8) // big-endian: bit 0 is the MSB
	return (h[byteIdx]>>bitInByte)&1 == 1
}

// setHashBit sets bit hashBit (0 = MSB of byte 0) of h.
func setHashBit(h *Hash, hashBit int) {
	byteIdx := hashBit / 8
	bitInByte := 7 - (hashBit % 8)
	h[byteIdx] |= 1 << bitInByte
}

// alphabet is the base64url (RFC 4648) alphabet. Index = 6-bit value.
const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// The encoded value is a 258-bit stream packed 6 bits per char (43 chars):
//
//	bit 0        : prepended 0 (keeps the first char in 0-31, never '-'/'_')
//	bits 1..256  : the 256 hash bits, big-endian
//	bit 257      : trailing padding (0)
//
// encBitToHashBit maps an encoded-stream bit position to a hash bit index, or
// returns ok=false for the prepended bit (0) and the padding bit (257).
func encBitToHashBit(encBit int) (hashBit int, ok bool) {
	if encBit == 0 || encBit >= 257 {
		return 0, false
	}
	return encBit - 1, true
}

// Encode encodes a 256-bit hash to a 43-character base64url string.
// The encoding prepends a 0 bit to ensure the result never starts with '-' or '_'.
func Encode(h Hash) string {
	var result [EncodedSize]byte
	for i := 0; i < EncodedSize; i++ {
		var val byte
		for b := 0; b < 6; b++ {
			encBit := i*6 + b
			if hashBit, ok := encBitToHashBit(encBit); ok && getHashBit(&h, hashBit) {
				val |= 1 << (5 - b)
			}
			// The prepended and padding bits contribute 0.
		}
		result[i] = alphabet[val]
	}
	return string(result[:])
}

// decodeChar maps a base64url character to its 6-bit value.
func decodeChar(c byte) (byte, bool) {
	switch {
	case c >= 'A' && c <= 'Z':
		return c - 'A', true
	case c >= 'a' && c <= 'z':
		return c - 'a' + 26, true
	case c >= '0' && c <= '9':
		return c - '0' + 52, true
	case c == '-':
		return 62, true
	case c == '_':
		return 63, true
	default:
		return 0, false
	}
}

// Decode decodes a 43-character base64url string to a 256-bit hash.
//
// Decode is canonical: it rejects strings whose prepended bit or trailing
// padding bit is set (ErrNonCanonical). Without that check, four distinct
// 43-char strings would decode to the same hash, making the round trip
// Encode(Decode(s)) lossy. With it, Decode is injective and Encode/Decode are
// exact inverses on the set of valid snap hashes.
func Decode(s string) (Hash, error) {
	var h Hash

	if len(s) != EncodedSize {
		return h, fmt.Errorf("%w: got %d, want %d", ErrInvalidLength, len(s), EncodedSize)
	}

	var vals [EncodedSize]byte
	for i := 0; i < EncodedSize; i++ {
		v, ok := decodeChar(s[i])
		if !ok {
			return h, fmt.Errorf("%w: invalid character %q at position %d", ErrInvalidEncoding, s[i], i)
		}
		vals[i] = v
	}

	encBitSet := func(encBit int) bool {
		return (vals[encBit/6]>>(5-encBit%6))&1 == 1
	}

	// The prepended bit (0) and padding bit (257) must both be 0 for a
	// canonical encoding.
	if encBitSet(0) {
		return h, fmt.Errorf("%w: prepended bit is set", ErrNonCanonical)
	}
	if encBitSet(257) {
		return h, fmt.Errorf("%w: padding bit is set", ErrNonCanonical)
	}

	for encBit := 1; encBit <= 256; encBit++ {
		if encBitSet(encBit) {
			hashBit, _ := encBitToHashBit(encBit)
			setHashBit(&h, hashBit)
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
