package aggregator

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Sha256Hex returns the lowercase hex encoding of SHA-256(b).
func Sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// EqualHash compares two hex hash strings case-insensitively after trimming space.
func EqualHash(actual, expected string) bool {
	return strings.EqualFold(strings.TrimSpace(actual), strings.TrimSpace(expected))
}
