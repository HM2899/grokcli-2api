package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
)

const passwordIterations = 120_000

func NewPassword(password string) (hashHex, saltHex string, err error) {
	if len(password) < 4 {
		return "", "", errors.New("password must contain at least 4 characters")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", "", err
	}
	derived := pbkdf2SHA256([]byte(password), []byte(hex.EncodeToString(salt)), passwordIterations, 32)
	return hex.EncodeToString(derived), hex.EncodeToString(salt), nil
}

// VerifyPassword reproduces settings_store.py's PBKDF2-HMAC-SHA256 format:
// the random salt is hex-encoded first, then that ASCII string is the PBKDF2 salt.
func VerifyPassword(password, hashHex, saltHex string) bool {
	expected, err := hex.DecodeString(hashHex)
	if err != nil || len(expected) == 0 || saltHex == "" {
		return false
	}
	actual := pbkdf2SHA256([]byte(password), []byte(saltHex), passwordIterations, len(expected))
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func pbkdf2SHA256(password, salt []byte, iterations, keyLength int) []byte {
	result := make([]byte, 0, keyLength)
	for block := uint32(1); len(result) < keyLength; block++ {
		mac := hmac.New(sha256.New, password)
		_, _ = mac.Write(salt)
		_, _ = mac.Write([]byte{byte(block >> 24), byte(block >> 16), byte(block >> 8), byte(block)})
		u := mac.Sum(nil)
		t := append([]byte(nil), u...)
		for i := 1; i < iterations; i++ {
			mac = hmac.New(sha256.New, password)
			_, _ = mac.Write(u)
			u = mac.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		remaining := keyLength - len(result)
		if remaining < len(t) {
			t = t[:remaining]
		}
		result = append(result, t...)
	}
	return result
}
