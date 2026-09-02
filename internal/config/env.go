package config

import (
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/L-K-M/dl-tool/internal/secure"
)

const secretFileSuffix = "_FILE"

func envString(name, fallback string) string {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}

	return value
}

func envBool(name string) (bool, *FatalError) {
	return envParsed(name, false, strconv.ParseBool)
}

func envDuration(name string, fallback time.Duration) (time.Duration, *FatalError) {
	return envParsed(name, fallback, time.ParseDuration)
}

func envParsed[T any](name string, fallback T, parse func(string) (T, error)) (T, *FatalError) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}

	parsed, err := parse(value)
	if err != nil {
		return fallback, malformedValue(name, value)
	}

	return parsed, nil
}

func envPathList(name string, fallback []string) ([]string, *FatalError) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}

	paths := strings.Split(value, pathListSeparator)
	for _, path := range paths {
		if path == "" {
			return nil, malformedValue(name, value)
		}
	}

	return paths, nil
}

func envCommaList(name string) []string {
	value := os.Getenv(name)
	if value == "" {
		return []string{}
	}

	return strings.Split(value, commaSeparator)
}

func envCIDRList(name string) ([]netip.Prefix, *FatalError) {
	value := os.Getenv(name)
	if value == "" {
		return []netip.Prefix{}, nil
	}

	parts := strings.Split(value, commaSeparator)
	prefixes := make([]netip.Prefix, 0, len(parts))
	for _, part := range parts {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(part))
		if err != nil {
			return nil, malformedValue(name, value)
		}

		prefixes = append(prefixes, prefix)
	}

	return prefixes, nil
}

func envSecret(name string) (secure.Secret, *FatalError) {
	inlineValue := os.Getenv(name)
	filePath := os.Getenv(name + secretFileSuffix)
	if inlineValue != "" && filePath != "" {
		return "", newFatal(errorCodeConflict, name, "inline and file forms are both set")
	}

	if filePath == "" {
		return secure.Secret(inlineValue), nil
	}

	info, err := os.Stat(filePath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&secretReadBits == 0 {
		return "", newFatal(errorCodeSecretUnreadable, name, "secret file is unreadable")
	}

	contents, err := os.ReadFile(filePath)
	if err != nil {
		return "", newFatal(errorCodeSecretUnreadable, name, "secret file is unreadable")
	}

	value := stripTrailingNewline(string(contents))
	return secure.Secret(value), nil
}

func stripTrailingNewline(value string) string {
	if strings.HasSuffix(value, windowsNewline) {
		return strings.TrimSuffix(value, windowsNewline)
	}

	return strings.TrimSuffix(value, unixNewline)
}

func malformedValue(name, value string) *FatalError {
	return newFatal(errorCodeMalformed, name, "invalid value "+strconv.Quote(value))
}
