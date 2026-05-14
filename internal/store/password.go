package store

import (
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// HashPassword returns a bcrypt hash of the plaintext password, in the
// form CreateAccount expects to be handed. Hashing is deliberately slow,
// so call this at account-creation time, not on every login.
func HashPassword(plaintext string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(plaintext), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("store: hash password: %w", err)
	}
	return string(h), nil
}

// VerifyPassword reports whether plaintext matches a bcrypt hash
// produced by HashPassword. The comparison is constant-time with
// respect to the hash, so it does not leak the password by timing.
func VerifyPassword(hash, plaintext string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plaintext)) == nil
}
