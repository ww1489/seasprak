package agent

import (
	"crypto/rand"
	"encoding/hex"
)

// NewID returns an opaque 128-bit identifier.
func NewID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
