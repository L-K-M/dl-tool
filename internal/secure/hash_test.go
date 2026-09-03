package secure

import (
	"encoding/base64"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

// phcPrefix is the constant head of every hash HashPassword stores; doc 12
// section 6.3 fixes it byte for byte.
const phcPrefix = "$argon2id$v=19$m=19456,t=2,p=1$"

// phcSplit decodes a PHC string into its salt and hash lengths.
func phcSplit(t *testing.T, hash string) (int, int) {
	t.Helper()

	fields := strings.Split(hash, "$")
	if len(fields) != phcFieldCount {
		t.Fatalf("PHC string %q has %d fields, want %d", hash, len(fields), phcFieldCount)
	}

	salt := decodePHCPart(t, fields[4])
	stored := decodePHCPart(t, fields[5])

	return len(salt), len(stored)
}

func decodePHCPart(t *testing.T, part string) []byte {
	t.Helper()

	decoded, err := base64.RawStdEncoding.DecodeString(part)
	if err != nil {
		t.Fatalf("decode PHC part %q: %v", part, err)
	}

	return decoded
}

// TestHashPasswordPHCString pins the stored format: the fixed parameter
// prefix and the salt and key lengths of the constants.
func TestHashPasswordPHCString(t *testing.T) {
	hash, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	if !strings.HasPrefix(hash, phcPrefix) {
		t.Errorf("hash %q lacks the prefix %q", hash, phcPrefix)
	}

	saltLen, keyLen := phcSplit(t, hash)
	if saltLen != argonSaltLen {
		t.Errorf("salt length = %d, want %d", saltLen, argonSaltLen)
	}
	if keyLen != argonKeyLen {
		t.Errorf("key length = %d, want %d", keyLen, argonKeyLen)
	}
}

// TestHashPasswordSaltsDiffer pins that two hashes of the same password are
// distinct: every call draws a fresh random salt.
func TestHashPasswordSaltsDiffer(t *testing.T) {
	first, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	second, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	if first == second {
		t.Error("two hashes of one password are identical; the salt is not random")
	}
}

func TestVerifyPasswordRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	ok, needsRehash, err := VerifyPassword(hash, "correct horse battery")
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Error("ok = false for the right password, want true")
	}
	if needsRehash {
		t.Error("needsRehash = true for current parameters, want false")
	}

	ok, needsRehash, err = VerifyPassword(hash, "wrong horse battery")
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if ok {
		t.Error("ok = true for a wrong password, want false")
	}
	if needsRehash {
		t.Error("needsRehash = true for current parameters, want false")
	}
}

// TestVerifyPasswordWeakerParametersNeedRehash feeds a hash minted with
// weaker parameters and asserts it still verifies but reports needsRehash.
func TestVerifyPasswordWeakerParametersNeedRehash(t *testing.T) {
	salt := make([]byte, argonSaltLen)
	weak := phcString(8192, 1, 1, salt,
		argon2.IDKey([]byte("correct horse battery"), salt, 1, 8192, 1, argonKeyLen))

	ok, needsRehash, err := VerifyPassword(weak, "correct horse battery")
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Error("ok = false for the right password under weaker parameters, want true")
	}
	if !needsRehash {
		t.Error("needsRehash = false for weaker parameters, want true")
	}

	// A wrong password never asks for a rehash, so a caller cannot store a
	// hash of the attacker's password.
	ok, needsRehash, err = VerifyPassword(weak, "wrong horse battery")
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if ok {
		t.Error("ok = true for a wrong password under weaker parameters, want false")
	}
	if needsRehash {
		t.Error("needsRehash = true for a wrong password, want false")
	}
}

// TestVerifyPasswordMalformedStringsRejectsEveryVariant asserts that no
// malformed input is accepted; every error path fails closed.
func TestVerifyPasswordMalformedStringsRejectsEveryVariant(t *testing.T) {
	for name, hash := range map[string]string{
		"empty":             "",
		"not a PHC string":  "argon2id$v=19",
		"wrong algorithm":   "$argon2i$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA",
		"wrong version":     "$argon2id$v=16$m=19456,t=2,p=1$c2FsdA$aGFzaA",
		"missing parameter": "$argon2id$v=19$m=19456,p=1$c2FsdA$aGFzaA",
		"zero parameter":    "$argon2id$v=19$m=0,t=2,p=1$c2FsdA$aGFzaA",
		"bad base64":        "$argon2id$v=19$m=19456,t=2,p=1$!!!$???",
		"empty salt":        "$argon2id$v=19$m=19456,t=2,p=1$$aGFzaA",
		"threads overflow":  "$argon2id$v=19$m=19456,t=2,p=256$c2FsdA$aGFzaA",
		"huge memory":       "$argon2id$v=19$m=2147483648,t=2,p=1$c2FsdA$aGFzaA",
		"huge time":         "$argon2id$v=19$m=19456,t=1000000,p=1$c2FsdA$aGFzaA",
		"memory below 8p":   "$argon2id$v=19$m=16,t=2,p=4$c2FsdA$aGFzaA",
		"oversized hash":    "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$" + strings.Repeat("a", 200),
	} {
		t.Run(name, func(t *testing.T) {
			ok, _, err := VerifyPassword(hash, "correct horse battery")
			if err == nil {
				t.Error("err = nil for a malformed PHC string, want an error")
			}
			if ok {
				t.Error("ok = true for a malformed PHC string, want false")
			}
		})
	}
}

// TestVerifyPasswordShortStoredHashNeedsRehash pins the re-derivation rule:
// the key is rebuilt with the length the stored PHC string carries, so a
// short hash from an older policy still verifies and flags needsRehash.
func TestVerifyPasswordShortStoredHashNeedsRehash(t *testing.T) {
	salt := make([]byte, argonSaltLen)
	shortKey := phcString(argonMemoryKiB, argonTime, argonThreads, salt,
		argon2.IDKey([]byte("correct horse battery"), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen-1))

	ok, needsRehash, err := VerifyPassword(shortKey, "correct horse battery")
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Error("ok = false against a short-but-correct stored hash, want true")
	}
	if !needsRehash {
		t.Error("needsRehash = false for a short stored hash, want true")
	}
}

// TestVerifyPasswordTamperedHashFails exercises the constant-time comparison
// in every byte position: flipping any stored byte must reject.
func TestVerifyPasswordTamperedHashFails(t *testing.T) {
	hash, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	fields := strings.Split(hash, "$")
	stored, err := base64.RawStdEncoding.DecodeString(fields[5])
	if err != nil {
		t.Fatalf("decode stored hash: %v", err)
	}

	for position := range stored {
		tampered := slices.Clone(stored)
		tampered[position] ^= 0xff

		fields[5] = base64.RawStdEncoding.EncodeToString(tampered)
		ok, _, err := VerifyPassword(strings.Join(fields, "$"), "correct horse battery")
		if err != nil {
			t.Fatalf("VerifyPassword with tampered byte %d: %v", position, err)
		}
		if ok {
			t.Errorf("ok = true with stored byte %d tampered, want false", position)
		}
	}
}
