package credential

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	minPasswordBytes = 12
	maxPasswordBytes = 1024
	memoryKiB        = 64 * 1024
	iterations       = 2
	parallelism      = 1
	saltBytes        = 16
	keyBytes         = 32
	maxEncodedBytes  = 128
	phcPrefix        = "$argon2id$v=19$m=65536,t=2,p=1$"
)

var (
	errPasswordLength = errors.New("password must contain between 12 and 1024 bytes")
	errInvalidHash    = errors.New("invalid or unsupported password hash")
)

// GeneratePassword returns a 32-byte cryptographically random base64url password.
func GeneratePassword() (string, error) {
	password := make([]byte, 32)
	if _, err := rand.Read(password); err != nil {
		return "", errors.New("could not generate a random password")
	}
	return base64.RawURLEncoding.EncodeToString(password), nil
}

// HashPassword preserves the password's exact bytes and uses a fresh random salt.
func HashPassword(password string) (string, error) {
	if len(password) < minPasswordBytes || len(password) > maxPasswordBytes {
		return "", errPasswordLength
	}
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", errors.New("could not generate a password salt")
	}
	key := argon2.IDKey([]byte(password), salt, iterations, memoryKiB, parallelism, keyBytes)
	return phcPrefix + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key), nil
}

// VerifyPassword accepts only this module's fixed profile. Resource costs are
// never taken from an external PHC string, and malformed inputs cannot run a KDF.
func VerifyPassword(password, encoded string) (bool, error) {
	if len(password) < minPasswordBytes || len(password) > maxPasswordBytes {
		return false, errPasswordLength
	}
	if len(encoded) > maxEncodedBytes || strings.ContainsAny(encoded, "\r\n") || !strings.HasPrefix(encoded, phcPrefix) {
		return false, errInvalidHash
	}
	fields := strings.Split(strings.TrimPrefix(encoded, phcPrefix), "$")
	if len(fields) != 2 || len(fields[0]) != base64.RawStdEncoding.EncodedLen(saltBytes) || len(fields[1]) != base64.RawStdEncoding.EncodedLen(keyBytes) {
		return false, errInvalidHash
	}
	encoding := base64.RawStdEncoding.Strict()
	salt, err := encoding.DecodeString(fields[0])
	if err != nil || len(salt) != saltBytes {
		return false, errInvalidHash
	}
	expected, err := encoding.DecodeString(fields[1])
	if err != nil || len(expected) != keyBytes {
		return false, errInvalidHash
	}
	actual := argon2.IDKey([]byte(password), salt, iterations, memoryKiB, parallelism, keyBytes)
	return subtle.ConstantTimeCompare(actual, expected) == 1, nil
}
