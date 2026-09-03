package secure

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// tokenByteCount is the entropy behind a minted session or setup token:
// 256 bits, double the 128-bit floor of docs/12-security-and-threat-model.md
// section 6.1.
const tokenByteCount = 32

// NewToken returns 32 bytes from crypto/rand, base64url-encoded without padding.
func NewToken() (string, error) {
	buffer := make([]byte, tokenByteCount)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("secure: mint token: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

// HashToken returns the lowercase SHA-256 hex of a session cookie value or a
// bearer token. Only the hash is stored server-side; the value itself never is.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))

	return hex.EncodeToString(sum[:])
}

// EqualToken compares two tokens in constant time.
func EqualToken(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
