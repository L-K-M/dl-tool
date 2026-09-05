package config

import (
	"context"
	"encoding"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	readOnlyMode os.FileMode = 0o555
	redactedText             = "[REDACTED]"
)

func TestEnvironmentDefaults(t *testing.T) {
	clearApplicationEnvironment(t)
	cfg, fatal := loadEnvironment()
	if fatal != nil {
		t.Fatalf("load defaults: %v", fatal)
	}
	want := &Config{
		HTTPAddr:       defaultHTTPAddr,
		AllowedHosts:   []string{},
		ConfigDir:      defaultConfigDir,
		DataRoots:      []string{defaultDataRoot},
		DBPath:         defaultDBPath,
		LogLevel:       defaultLogLevel,
		LogFormat:      defaultLogFormat,
		TrustedProxies: []netip.Prefix{},
		SessionTTL:     defaultSessionTTL,
		MetricsAddr:    defaultMetricsAddr,
		YtdlpPath:      defaultYtdlpPath,
		JSRuntimePath:  defaultJSRuntimePath,
		SevenzipPath:   defaultSevenzipPath,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("defaults mismatch:\n got: %#v\nwant: %#v", cfg, want)
	}
}

func TestSecretInputs(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		configureLoad(t, t.TempDir())
		path := filepath.Join(t.TempDir(), "aria2-secret")
		requireNoError(t, os.WriteFile(path, []byte("from-file\n"), secretFileMode))
		t.Setenv(envAria2URL, "http://aria2.invalid")
		t.Setenv(envAria2Secret+secretFileSuffix, path)
		if got := mustLoad(t).Aria2Secret.Reveal(); got != "from-file" {
			t.Fatalf("secret = %q", got)
		}
	})
	t.Run("conflict", func(t *testing.T) {
		clearApplicationEnvironment(t)
		t.Setenv(envAria2Secret, "inline-secret")
		t.Setenv(envAria2Secret+secretFileSuffix, "secret-file")
		_, fatal := loadEnvironment()
		requireFatal(t, fatal, errorCodeConflict, envAria2Secret)
		if strings.Contains(fatal.Error(), "inline-secret") {
			t.Fatal("fatal error disclosed a secret")
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		clearApplicationEnvironment(t)
		t.Setenv(envAria2Secret+secretFileSuffix, filepath.Join(t.TempDir(), "missing"))
		_, fatal := loadEnvironment()
		requireFatal(t, fatal, errorCodeSecretUnreadable, envAria2Secret)
	})
}

func TestSecretFileStripsOneTrailingNewline(t *testing.T) {
	tests := []struct {
		name, contents, want string
	}{
		{"line feed", "secret\n", "secret"},
		{"windows newline", "secret\r\n", "secret"},
		{"two line feeds", "secret\n\n", "secret\n"},
		{"line feed then windows newline", "secret\n\r\n", "secret\n"},
		{"bare carriage return", "secret\r", "secret\r"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearApplicationEnvironment(t)
			path := filepath.Join(t.TempDir(), "secret")
			requireNoError(t, os.WriteFile(path, []byte(test.contents), secretFileMode))
			t.Setenv(envAria2Secret+secretFileSuffix, path)

			got, fatal := envSecret(envAria2Secret)
			if fatal != nil || got.Reveal() != test.want {
				t.Fatalf("secret = %q, fatal = %v", got.Reveal(), fatal)
			}
		})
	}
}

func TestFatalValidation(t *testing.T) {
	type environment map[string]string
	type testCase struct {
		name, variable, code string
		values               environment
	}
	blocked := filepath.Join(t.TempDir(), "file")
	requireNoError(t, os.WriteFile(blocked, nil, secretFileMode))
	tests := []testCase{
		{"aria2 secret missing", envAria2Secret, errorCodeMissing, environment{envAria2URL: "http://aria2.invalid"}},
		{"qbt username missing", envQBittorrentUser, errorCodeMissing, environment{envQBittorrentURL: "http://qbt.invalid", envQBittorrentPass: "pass"}},
		{"qbt password missing", envQBittorrentPass, errorCodeMissing, environment{envQBittorrentURL: "http://qbt.invalid", envQBittorrentUser: "user"}},
		{"boolean malformed", envConfigLock, errorCodeMalformed, environment{envConfigLock: "sometimes"}},
		{"duration malformed", envSessionTTL, errorCodeMalformed, environment{envSessionTTL: "a while"}},
		{"CIDR malformed", envTrustedProxies, errorCodeMalformed, environment{envTrustedProxies: "invalid"}},
		{"log level malformed", envLogLevel, errorCodeMalformed, environment{envLogLevel: "verbose"}},
		{"log format malformed", envLogFormat, errorCodeMalformed, environment{envLogFormat: "xml"}},
		{"HTTP address malformed", envHTTPAddr, errorCodeMalformed, environment{envHTTPAddr: "localhost"}},
		{"metrics address malformed", envMetricsAddr, errorCodeMalformed, environment{envMetricsAddr: "localhost:http"}},
		{"base path malformed", envBasePath, errorCodeMalformed, environment{envBasePath: "tools"}},
		{"allowed host malformed", envAllowedHosts, errorCodeMalformed, environment{envAllowedHosts: "*.example.com"}},
		{"path malformed", envDataRoots, errorCodeMalformed, environment{envDataRoots: "relative"}},
		{"config path unwritable", envConfigDir, errorCodePathUnwritable, environment{envConfigDir: blocked}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configureLoad(t, t.TempDir())
			for name, value := range test.values {
				t.Setenv(name, value)
			}
			_, err := Load(context.Background())
			requireFatal(t, err, test.code, test.variable)
		})
	}
}

func TestBasePathIsAStaticLocalMount(t *testing.T) {
	for _, value := range []string{"//other.invalid/app", "/app?query", "/app#fragment", "/app/../other", "/app//nested", "/app%2fother", "/app\\other", "/{prefix}", "/app/*"} {
		t.Run(value, func(t *testing.T) {
			configureLoad(t, t.TempDir())
			t.Setenv(envBasePath, value)
			_, err := Load(t.Context())
			requireFatal(t, err, errorCodeMalformed, envBasePath)
		})
	}
	for _, value := range []string{"", "/dl-tool", "/nested/app", "/ümlaut"} {
		t.Run("valid="+value, func(t *testing.T) {
			configureLoad(t, t.TempDir())
			t.Setenv(envBasePath, value)
			if got := mustLoad(t).BasePath; got != value {
				t.Fatalf("base path = %q, want %q", got, value)
			}
		})
	}
}

func TestConfigDirectoryStaysPrivateWithExternalDatabase(t *testing.T) {
	directory := configureLoad(t, t.TempDir())
	requireNoError(t, os.Chmod(directory, 0o777))
	t.Setenv(envDBPath, filepath.Join(t.TempDir(), "state.db"))
	mustLoad(t)

	info, err := os.Stat(directory)
	requireNoError(t, err)
	if got := info.Mode().Perm(); got != privateDirectoryMode {
		t.Fatalf("config directory mode = %o, want %o", got, privateDirectoryMode)
	}
}

func TestSecretsAreDistinctPerInstance(t *testing.T) {
	var keys [2]string
	for index := range keys {
		dir := configureLoad(t, t.TempDir())
		cfg := mustLoad(t)
		contents, err := os.ReadFile(filepath.Join(dir, secretsFileName))
		if err != nil {
			t.Fatal(err)
		}
		values := parseSecrets(contents)
		for _, name := range []string{aria2RPCSecretKey, secretKeyName} {
			decoded, err := base64.RawURLEncoding.DecodeString(values[name])
			if err != nil || len(decoded) != secretByteCount {
				t.Fatalf("%s is not a %d-byte secret", name, secretByteCount)
			}
		}
		info, err := os.Stat(filepath.Join(dir, secretsFileName))
		if err != nil || info.Mode().Perm() != secretFileMode {
			t.Fatalf("secrets file mode: %v", err)
		}
		keys[index] = cfg.SecretKey.Reveal()
	}
	if keys[0] == keys[1] {
		t.Fatal("fresh instances received the same secret key")
	}
}

func TestCompleteSecretsFileIsNotOverwritten(t *testing.T) {
	dir := configureLoad(t, t.TempDir())
	path := filepath.Join(dir, secretsFileName)
	contents := "# keep\nARIA2_RPC_SECRET=existing-aria\nDLTOOL_SECRET_KEY=existing-key\n"
	requireNoError(t, os.WriteFile(path, []byte(contents), secretFileMode))
	oldTime := time.Unix(1_700_000_000, 0)
	requireNoError(t, os.Chtimes(path, oldTime, oldTime))
	if got := mustLoad(t).SecretKey.Reveal(); got != "existing-key" {
		t.Fatalf("secret key = %q", got)
	}
	after, readErr := os.ReadFile(path)
	info, statErr := os.Stat(path)
	if readErr != nil || statErr != nil || string(after) != contents || !info.ModTime().Equal(oldTime) {
		t.Fatal("secrets file was rewritten")
	}
}

func TestSecretsFileReplacementIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), secretsFileName)
	requireNoError(t, os.WriteFile(path, []byte("old"), secretFileMode))
	before, err := os.Stat(path)
	requireNoError(t, err)

	replacement := []byte("new")
	requireNoError(t, writeSecretsFile(path, replacement))
	after, err := os.Stat(path)
	requireNoError(t, err)
	contents, err := os.ReadFile(path)
	requireNoError(t, err)

	if os.SameFile(before, after) || !reflect.DeepEqual(contents, replacement) {
		t.Fatal("secrets file was rewritten in place")
	}
}

func TestSecretsFileSymlinkIsRejected(t *testing.T) {
	dir := configureLoad(t, t.TempDir())
	target := filepath.Join(t.TempDir(), "target")
	contents := "ARIA2_RPC_SECRET=existing-aria\nDLTOOL_SECRET_KEY=existing-key\n"
	requireNoError(t, os.WriteFile(target, []byte(contents), secretFileMode))
	requireNoError(t, os.Symlink(target, filepath.Join(dir, secretsFileName)))

	_, err := Load(context.Background())
	requireFatal(t, err, errorCodeSecretUnreadable, secretKeyName)
}

func TestSecretNeverPrints(t *testing.T) {
	configureLoad(t, t.TempDir())
	t.Setenv(envAria2Secret, "do-not-disclose")
	secret := mustLoad(t).Aria2Secret
	encoded, err := json.Marshal(secret)
	if err != nil {
		t.Fatal(err)
	}
	textMarshaler := encoding.TextMarshaler(secret)
	text, err := textMarshaler.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	got := []string{fmt.Sprintf("%v", secret), fmt.Sprintf("%s", secret), string(encoded), string(text)}
	want := []string{redactedText, redactedText, `"[REDACTED]"`, redactedText}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("secret rendered as %#v", got)
	}
}

func TestUnwritableDataRootIsNotFatal(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "missing")
		configureLoad(t, root)
		logs := captureLogs(t)
		mustLoad(t)
		if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing root was created: %v", err)
		}
		requireWarning(t, logs.String(), warningDataRootNotWritable, root)
	})
	t.Run("read-only", func(t *testing.T) {
		root := t.TempDir()
		requireNoError(t, os.Chmod(root, readOnlyMode))
		t.Cleanup(func() { requireNoError(t, os.Chmod(root, privateDirectoryMode)) })
		configureLoad(t, root)
		logs := captureLogs(t)
		mustLoad(t)
		info, err := os.Stat(root)
		if err != nil || info.Mode().Perm() != readOnlyMode {
			t.Fatalf("data root mode changed: %v", err)
		}
		requireWarning(t, logs.String(), warningDataRootNotWritable, root)
	})
}

func TestWarningRows(t *testing.T) {
	configureLoad(t, t.TempDir())
	missing := t.TempDir()
	t.Setenv(envYtdlpPath, filepath.Join(missing, "yt-dlp"))
	t.Setenv(envJSRuntimePath, filepath.Join(missing, "node"))
	t.Setenv(envSevenzipPath, filepath.Join(missing, "7zz"))
	t.Setenv(envWatchDir, t.TempDir())
	t.Setenv(envNotifyURL, "not a URL")
	logs := captureLogs(t)
	cfg := mustLoad(t)
	checks := [][2]string{
		{warningBinaryMissing, envYtdlpPath}, {warningBinaryMissing, envSevenzipPath},
		{warningJSRuntimeMissing, envJSRuntimePath}, {warningOutOfRoot, envWatchDir},
		{errorCodeMalformed, envNotifyURL},
	}
	for _, check := range checks {
		requireWarning(t, logs.String(), check[0], check[1])
	}
	if cfg.WatchDir != "" || cfg.NotifyURL != "" {
		t.Fatal("invalid preference seed was retained")
	}
}

func configureLoad(t *testing.T, dataRoot string) string {
	t.Helper()
	clearApplicationEnvironment(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	values := map[string]string{
		envConfigDir: dir, envDBPath: filepath.Join(dir, "dl-tool.db"), envDataRoots: dataRoot,
		envYtdlpPath: executable, envJSRuntimePath: executable, envSevenzipPath: executable,
	}
	for name, value := range values {
		t.Setenv(name, value)
	}
	return dir
}

func clearApplicationEnvironment(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, keyValueSeparator)
		if strings.HasPrefix(name, environmentPrefix) {
			t.Setenv(name, "")
		}
	}
}

func mustLoad(t *testing.T) *Config {
	t.Helper()
	cfg, err := Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func requireFatal(t *testing.T, err error, code, variable string) {
	t.Helper()
	var fatal *FatalError
	if !errors.As(err, &fatal) {
		t.Fatalf("error does not wrap FatalError: %v", err)
	}
	if fatal.Code != code || fatal.Variable != variable {
		t.Fatalf("fatal = %q/%q, want %q/%q", fatal.Code, fatal.Variable, code, variable)
	}
}

func captureLogs(t *testing.T) *strings.Builder {
	t.Helper()
	previous := slog.Default()
	logs := &strings.Builder{}
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}

func requireWarning(t *testing.T, logs, code, attribute string) {
	t.Helper()
	for _, record := range strings.Split(logs, "\n") {
		if strings.Contains(record, `"level":"WARN"`) &&
			strings.Contains(record, `"err_code":"`+code+`"`) && strings.Contains(record, attribute) {
			return
		}
	}
	t.Fatalf("warning %q/%q missing:\n%s", code, attribute, logs)
}
