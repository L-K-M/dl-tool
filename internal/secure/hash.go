package secure

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// The OWASP mid-point argon2id parameters of
// docs/12-security-and-threat-model.md section 6.3. Do not lower them.
const (
	argonMemoryKiB = 19456 // m=19456 (19 MiB)
	argonTime      = 2     // t=2
	argonThreads   = 1     // p=1
	argonSaltLen   = 16
	argonKeyLen    = 32

	// PHC string layout: $argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>
	// (https://github.com/P-H-C/phc-string-format).
	argonAlgorithm = "argon2id"
	argonVersion   = 19

	// Splitting a well-formed PHC string on "$" yields six fields:
	// ["", "argon2id", "v=19", "m=..,t=..,p=..", "<salt>", "<hash>"].
	phcFieldCount = 6

	// Verification ceilings for the parameters a stored PHC string may carry
	// (doc 12 section 6.3 sits far below all of them).
	maxVerifyMemoryKiB = 1 << 21 // 2 GiB
	maxVerifyTime      = 32
	maxVerifyThreads   = 32
	maxVerifyKeyLen    = 128
)

// HashPassword returns the full PHC string
// $argon2id$v=19$m=19456,t=2,p=1$<b64salt>$<b64hash>.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("secure: hash password: %w", err)
	}

	return phcString(
		argonMemoryKiB, argonTime, argonThreads,
		salt, argon2.IDKey([]byte(password), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen),
	), nil
}

// VerifyPassword parses the PHC string, re-derives the key with the
// parameters it carries and compares in constant time. It returns
// needsRehash — and only after a successful verification — when the stored
// parameters are weaker than the constants above, so a login can upgrade
// the hash after the policy is raised.
func VerifyPassword(phc, password string) (ok bool, needsRehash bool, err error) {
	fields := strings.Split(phc, "$")
	if len(fields) != phcFieldCount {
		return false, false, fmt.Errorf("secure: malformed PHC string: expected %d fields, got %d", phcFieldCount, len(fields))
	}
	if fields[1] != argonAlgorithm {
		return false, false, fmt.Errorf("secure: unsupported PHC algorithm %q", fields[1])
	}

	version, err := parsePHCIntField(fields[2], "v")
	if err != nil {
		return false, false, err
	}
	if version != argonVersion {
		return false, false, fmt.Errorf("secure: unsupported PHC version %d", version)
	}

	memoryKiB, err := parsePHCIntField(fields[3], "m")
	if err != nil {
		return false, false, err
	}
	timeParam, err := parsePHCIntField(fields[3], "t")
	if err != nil {
		return false, false, err
	}
	threads, err := parsePHCIntField(fields[3], "p")
	if err != nil {
		return false, false, err
	}

	// RawStdEncoding is the PHC convention: standard alphabet, no padding.
	salt, err := base64.RawStdEncoding.DecodeString(fields[4])
	if err != nil {
		return false, false, fmt.Errorf("secure: decode PHC salt: %w", err)
	}
	hash, err := base64.RawStdEncoding.DecodeString(fields[5])
	if err != nil {
		return false, false, fmt.Errorf("secure: decode PHC hash: %w", err)
	}
	if len(salt) == 0 || len(hash) == 0 {
		return false, false, fmt.Errorf("secure: empty PHC salt or hash")
	}

	// A tampered or corrupted row must not turn a login into a crash or a
	// CPU/memory hog: bound the parameters before deriving. The ceilings sit
	// far above any sane policy; argon2id also requires m >= 8*p.
	if memoryKiB > maxVerifyMemoryKiB || timeParam > maxVerifyTime || threads > maxVerifyThreads ||
		memoryKiB < 8*threads || len(hash) > maxVerifyKeyLen {
		return false, false, fmt.Errorf(
			"secure: PHC parameters out of supported range (m=%d, t=%d, p=%d, keyLen=%d)",
			memoryKiB, timeParam, threads, len(hash),
		)
	}

	derived := argon2.IDKey([]byte(password), salt, uint32(timeParam), uint32(memoryKiB), uint8(threads), uint32(len(hash)))
	ok = subtle.ConstantTimeCompare(derived, hash) == 1
	// Only a verified password may ask for a rehash: a caller rehashing on a
	// failed attempt would store a hash of the attacker's wrong password.
	needsRehash = ok && (memoryKiB < argonMemoryKiB ||
		timeParam < argonTime ||
		threads < argonThreads ||
		len(salt) < argonSaltLen ||
		len(hash) < argonKeyLen)

	return ok, needsRehash, nil
}

// phcString renders the PHC string for the given parameters and derived key.
func phcString(memoryKiB, timeParam, threads int, salt, hash []byte) string {
	return "$" + argonAlgorithm +
		"$v=" + strconv.Itoa(argonVersion) +
		"$m=" + strconv.Itoa(memoryKiB) +
		",t=" + strconv.Itoa(timeParam) +
		",p=" + strconv.Itoa(threads) +
		"$" + base64.RawStdEncoding.EncodeToString(salt) +
		"$" + base64.RawStdEncoding.EncodeToString(hash)
}

// parsePHCIntField reads one integer assignment (v, m, t or p) from the
// field it appears in.
func parsePHCIntField(field, name string) (int, error) {
	for _, assignment := range strings.Split(field, ",") {
		key, value, found := strings.Cut(assignment, "=")
		if !found || key != name {
			continue
		}

		parsed, err := strconv.Atoi(value)
		if err != nil {
			return 0, fmt.Errorf("secure: parse PHC %s parameter: %w", name, err)
		}
		if parsed <= 0 {
			return 0, fmt.Errorf("secure: PHC %s parameter must be positive, got %d", name, parsed)
		}

		return parsed, nil
	}

	return 0, fmt.Errorf("secure: PHC string lacks the %s parameter", name)
}
