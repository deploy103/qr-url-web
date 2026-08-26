// Package auth implements administrator password hashing, session tokens and
// CSRF tokens. Every random value comes from crypto/rand and every comparison
// of a secret is constant time.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// Argon2idParams are the cost parameters of a password hash.
type Argon2idParams struct {
	Memory      uint32 // in KiB
	Iterations  uint32
	Parallelism uint8
	SaltLength  uint32
	KeyLength   uint32
}

// DefaultParams targets roughly 64 MiB and a few tens of milliseconds on a
// small server, which is comfortably above the OWASP minimum for Argon2id.
func DefaultParams() Argon2idParams {
	threads := uint8(runtime.NumCPU())
	if threads < 1 {
		threads = 1
	}
	if threads > 4 {
		threads = 4
	}
	return Argon2idParams{
		Memory:      64 * 1024,
		Iterations:  3,
		Parallelism: threads,
		SaltLength:  16,
		KeyLength:   32,
	}
}

// Errors returned when parsing or verifying a stored hash.
var (
	ErrInvalidHash      = errors.New("auth: password hash is malformed")
	ErrIncompatibleHash = errors.New("auth: unsupported password hash algorithm or version")
	ErrPasswordTooShort = errors.New("auth: password must be at least 12 characters")
	ErrPasswordTooLong  = errors.New("auth: password must be at most 256 characters")
)

// MinPasswordLength is the shortest administrator password accepted.
const MinPasswordLength = 12

// MaxPasswordLength bounds the work an attacker can force on the hash.
const MaxPasswordLength = 256

// ValidatePassword checks the length policy of a new password.
func ValidatePassword(password string) error {
	switch {
	case len([]rune(password)) < MinPasswordLength:
		return ErrPasswordTooShort
	case len(password) > MaxPasswordLength:
		return ErrPasswordTooLong
	}
	return nil
}

// HashPassword derives an Argon2id hash in the standard PHC string format.
func HashPassword(password string, p Argon2idParams) (string, error) {
	if len(password) > MaxPasswordLength {
		return "", ErrPasswordTooLong
	}
	salt := make([]byte, p.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: read salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, p.Iterations, p.Memory, p.Parallelism, p.KeyLength)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Iterations, p.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword reports whether password matches the encoded Argon2id hash.
// It returns an error only when the stored hash cannot be parsed, so that a
// wrong password and a broken record are never confused.
func VerifyPassword(encoded, password string) (bool, error) {
	if len(password) > MaxPasswordLength {
		return false, nil
	}
	p, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, p.Iterations, p.Memory, p.Parallelism, p.KeyLength)
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

func decodeHash(encoded string) (Argon2idParams, []byte, []byte, error) {
	var p Argon2idParams
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" {
		return p, nil, nil, ErrInvalidHash
	}
	if parts[1] != "argon2id" {
		return p, nil, nil, ErrIncompatibleHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	if version != argon2.Version {
		return p, nil, nil, ErrIncompatibleHash
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Iterations, &p.Parallelism); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	if p.Memory == 0 || p.Iterations == 0 || p.Parallelism == 0 {
		return p, nil, nil, ErrInvalidHash
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return p, nil, nil, ErrInvalidHash
	}
	key, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return p, nil, nil, ErrInvalidHash
	}
	p.SaltLength = uint32(len(salt))
	p.KeyLength = uint32(len(key))
	return p, salt, key, nil
}

// dummyHash is a valid Argon2id record over a random password. Verifying
// against it costs the same as verifying a real one, which lets the login
// handler spend identical time whether or not an account exists.
var dummyHash = sync.OnceValue(func() string {
	password, err := NewPassword()
	if err != nil {
		password = "fallback-dummy-password"
	}
	h, err := HashPassword(password, DefaultParams())
	if err != nil {
		// Cannot happen with DefaultParams; fall back to a shape that still
		// parses so the caller performs a real derivation.
		return "$argon2id$v=19$m=65536,t=3,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	}
	return h
})

// DummyHash returns a throwaway hash for equalising login timing.
func DummyHash() string { return dummyHash() }
