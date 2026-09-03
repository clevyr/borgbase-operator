// Package secrets generates the credentials the operator owns.
package secrets

import (
	"crypto/rand"
	"fmt"
)

// passwordLength of 128 characters
const passwordLength = 128

// alphabet deliberately excludes punctuation. The password is interpolated
// into shell environments and, for the REST credential, into a URL; keeping it
// alphanumeric means it can never need escaping in either place.
const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// GeneratePassword returns a new restic repository password.
//
// This is the encryption key for every snapshot in the repository. If it is
// lost the backups are unrecoverable, so callers must persist it before it is
// used and must never regenerate it for a repository that already has one.
func GeneratePassword() (string, error) {
	buf := make([]byte, passwordLength)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating password: %w", err)
	}
	// len(alphabet) is 62, which does not divide 256, so taking the raw
	// modulus would bias the first few letters. Rejection-sample instead.
	const maxUnbiased = 256 - (256 % len(alphabet))
	out := make([]byte, 0, passwordLength)
	for len(out) < passwordLength {
		for _, b := range buf {
			if int(b) >= maxUnbiased {
				continue
			}
			out = append(out, alphabet[int(b)%len(alphabet)])
			if len(out) == passwordLength {
				break
			}
		}
		if len(out) < passwordLength {
			if _, err := rand.Read(buf); err != nil {
				return "", fmt.Errorf("generating password: %w", err)
			}
		}
	}
	return string(out), nil
}
