package main

import (
	"crypto/rand"
	"encoding/hex"
)

// newUserID matches the auth service's user id format (usr_ + 12 random bytes).
func newUserID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "usr_" + hex.EncodeToString(b), nil
}
