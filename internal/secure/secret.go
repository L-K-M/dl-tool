// Package secure provides types that prevent accidental secret disclosure.
package secure

import (
	"fmt"
	"io"
)

const (
	redacted     = "[REDACTED]"
	redactedJSON = `"[REDACTED]"`
)

// Secret is a string whose value never reaches a log, an error or an API response.
type Secret string

func (s Secret) String() string { return redacted }

func (s Secret) Format(f fmt.State, _ rune) {
	if _, err := io.WriteString(f, redacted); err != nil {
		return
	}
}

func (s Secret) MarshalJSON() ([]byte, error) { return []byte(redactedJSON), nil }

func (s Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

func (s Secret) Reveal() string { return string(s) }
