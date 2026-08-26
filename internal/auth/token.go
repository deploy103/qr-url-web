package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
)

// TokenBytes is the entropy of session and CSRF tokens.
const TokenBytes = 32

// NewToken returns a URL safe random token backed by crypto/rand.
func NewToken() (string, error) {
	buf := make([]byte, TokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: read random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashToken returns the hex encoded SHA-256 of a token. Session cookies are
// only ever stored in hashed form.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", sum)
}

// ConstantTimeEqual compares two tokens without leaking their contents through
// timing.
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// NewPassword generates a random password suitable for first time setup.
func NewPassword() (string, error) {
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: read random password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
