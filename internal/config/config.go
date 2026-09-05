// Package config loads and validates process configuration.
package config

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/L-K-M/dl-tool/internal/secure"
)

const (
	environmentPrefix   = "DLTOOL_"
	envHTTPAddr         = "DLTOOL_HTTP_ADDR"
	envBasePath         = "DLTOOL_BASE_PATH"
	envAllowedHosts     = "DLTOOL_ALLOWED_HOSTS"
	envConfigLock       = "DLTOOL_CONFIG_LOCK"
	envConfigDir        = "DLTOOL_CONFIG_DIR"
	envDataRoots        = "DLTOOL_DATA_ROOTS"
	envDBPath           = "DLTOOL_DB_PATH"
	envLogLevel         = "DLTOOL_LOG_LEVEL"
	envLogFormat        = "DLTOOL_LOG_FORMAT"
	envTrustedProxies   = "DLTOOL_TRUSTED_PROXIES"
	envSessionTTL       = "DLTOOL_SESSION_TTL"
	envMetricsAddr      = "DLTOOL_METRICS_ADDR"
	envAria2URL         = "DLTOOL_ARIA2_URL"
	envAria2Secret      = "DLTOOL_ARIA2_SECRET"
	envQBittorrentURL   = "DLTOOL_QBITTORRENT_URL"
	envQBittorrentUser  = "DLTOOL_QBITTORRENT_USERNAME"
	envQBittorrentPass  = "DLTOOL_QBITTORRENT_PASSWORD"
	envYtdlpPath        = "DLTOOL_YTDLP_PATH"
	envJSRuntimePath    = "DLTOOL_JS_RUNTIME_PATH"
	envSevenzipPath     = "DLTOOL_SEVENZIP_PATH"
	envSSRFAllowPrivate = "DLTOOL_SSRF_ALLOW_PRIVATE"
	envWatchDir         = "DLTOOL_WATCH_DIR"
	envNotifyURL        = "DLTOOL_NOTIFY_URL"

	defaultHTTPAddr      = ":8080"
	defaultConfigDir     = "/config"
	defaultDataRoot      = "/data"
	defaultDBPath        = "/config/dl-tool.db"
	defaultLogLevel      = "info"
	defaultLogFormat     = "json"
	defaultMetricsAddr   = "127.0.0.1:9090"
	defaultYtdlpPath     = "/usr/local/bin/yt-dlp"
	defaultJSRuntimePath = "/usr/bin/node"
	defaultSevenzipPath  = "/usr/bin/7zz"
	defaultSessionTTL    = 720 * time.Hour

	logLevelDebug = "debug"
	logLevelWarn  = "warn"
	logLevelError = "error"
	logFormatText = "text"
	metricsOff    = "off"

	errorCodeMissing          = "config_missing"
	errorCodeConflict         = "config_conflict"
	errorCodeSecretUnreadable = "config_secret_unreadable"
	errorCodeMalformed        = "config_malformed"
	errorCodePathUnwritable   = "config_path_unwritable"
	errorCodeAttribute        = "err_code"

	warningDataRootNotWritable = "data_root_not_writable"
	warningBinaryMissing       = "binary_missing"
	warningJSRuntimeMissing    = "js_runtime_missing"
	warningOutOfRoot           = "config_out_of_root"
	eventSecretKeyRegenerated  = "secret_key_regenerated"
	eventAria2SecretRotated    = "aria2_secret_rotated"

	secretsFileName   = "secrets.env"
	aria2RPCSecretKey = "ARIA2_RPC_SECRET"
	secretKeyName     = "DLTOOL_SECRET_KEY"
	secretByteCount   = 32

	pathListSeparator    = ":"
	commaSeparator       = ","
	unixNewline          = "\n"
	windowsNewline       = "\r\n"
	keyValueSeparator    = "="
	maxDNSNameLength     = 253
	dnsNamePattern       = `^(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)*[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`
	directoryWriteBits   = 0o222
	secretReadBits       = 0o444
	privateDirectoryMode = 0o700
	secretFileMode       = 0o600
)

var dnsNameRegexp = regexp.MustCompile(dnsNamePattern)

// Config is the validated process configuration.
type Config struct {
	HTTPAddr         string
	BasePath         string
	AllowedHosts     []string
	ConfigLock       bool
	ConfigDir        string
	DataRoots        []string
	DBPath           string
	LogLevel         string
	LogFormat        string
	TrustedProxies   []netip.Prefix
	SessionTTL       time.Duration
	MetricsAddr      string
	Aria2URL         string
	Aria2Secret      secure.Secret
	QBittorrentURL   string
	QBittorrentUser  string
	QBittorrentPass  secure.Secret
	YtdlpPath        string
	JSRuntimePath    string
	SevenzipPath     string
	SSRFAllowPrivate bool
	WatchDir         string
	NotifyURL        string
	SecretKey        secure.Secret
}

// FatalError carries a boot-validation error code.
type FatalError struct {
	Code     string
	Variable string
	Detail   string
}

func (e *FatalError) Error() string {
	if e.Detail == "" {
		return e.Code + ": " + e.Variable
	}

	return e.Code + ": " + e.Variable + ": " + e.Detail
}

// Load reads the environment, validates it and generates missing boot secrets.
func Load(ctx context.Context) (*Config, error) {
	cfg, fatal := loadEnvironment()
	if fatal != nil {
		return nil, fmt.Errorf("load environment: %w", fatal)
	}

	if fatal = validateConfig(cfg); fatal != nil {
		return nil, fmt.Errorf("validate configuration: %w", fatal)
	}

	generated, fatal := writeSecrets(cfg.ConfigDir)
	if fatal != nil {
		return nil, fmt.Errorf("load boot secrets: %w", fatal)
	}
	cfg.SecretKey = generated.secretKey

	warnRegeneratedSecrets(ctx, generated)
	warnUnavailablePaths(ctx, cfg)

	return cfg, nil
}

func loadEnvironment() (*Config, *FatalError) {
	cfg := &Config{
		HTTPAddr:        envString(envHTTPAddr, defaultHTTPAddr),
		BasePath:        envString(envBasePath, ""),
		AllowedHosts:    envCommaList(envAllowedHosts),
		ConfigDir:       envString(envConfigDir, defaultConfigDir),
		DBPath:          envString(envDBPath, defaultDBPath),
		LogLevel:        envString(envLogLevel, defaultLogLevel),
		LogFormat:       envString(envLogFormat, defaultLogFormat),
		SessionTTL:      defaultSessionTTL,
		MetricsAddr:     envString(envMetricsAddr, defaultMetricsAddr),
		Aria2URL:        envString(envAria2URL, ""),
		QBittorrentURL:  envString(envQBittorrentURL, ""),
		QBittorrentUser: envString(envQBittorrentUser, ""),
		YtdlpPath:       envString(envYtdlpPath, defaultYtdlpPath),
		JSRuntimePath:   envString(envJSRuntimePath, defaultJSRuntimePath),
		SevenzipPath:    envString(envSevenzipPath, defaultSevenzipPath),
		WatchDir:        envString(envWatchDir, ""),
		NotifyURL:       envString(envNotifyURL, ""),
	}

	var fatal *FatalError
	if cfg.Aria2Secret, fatal = envSecret(envAria2Secret); fatal != nil {
		return nil, fatal
	}
	if cfg.QBittorrentPass, fatal = envSecret(envQBittorrentPass); fatal != nil {
		return nil, fatal
	}
	if cfg.DataRoots, fatal = envPathList(envDataRoots, []string{defaultDataRoot}); fatal != nil {
		return nil, fatal
	}
	if cfg.Aria2URL != "" && cfg.Aria2Secret.Reveal() == "" {
		return nil, missingVariable(envAria2Secret)
	}
	if cfg.QBittorrentURL != "" && cfg.QBittorrentUser == "" {
		return nil, missingVariable(envQBittorrentUser)
	}
	if cfg.QBittorrentURL != "" && cfg.QBittorrentPass.Reveal() == "" {
		return nil, missingVariable(envQBittorrentPass)
	}
	if cfg.ConfigLock, fatal = envBool(envConfigLock); fatal != nil {
		return nil, fatal
	}
	if cfg.TrustedProxies, fatal = envCIDRList(envTrustedProxies); fatal != nil {
		return nil, fatal
	}
	if cfg.SessionTTL, fatal = envDuration(envSessionTTL, defaultSessionTTL); fatal != nil {
		return nil, fatal
	}
	if cfg.SSRFAllowPrivate, fatal = envBool(envSSRFAllowPrivate); fatal != nil {
		return nil, fatal
	}

	return cfg, nil
}

func validateConfig(cfg *Config) *FatalError {
	checks := []struct {
		valid    bool
		variable string
		value    string
	}{
		{validLogLevel(cfg.LogLevel), envLogLevel, cfg.LogLevel},
		{cfg.LogFormat == defaultLogFormat || cfg.LogFormat == logFormatText, envLogFormat, cfg.LogFormat},
		{validListenAddress(cfg.HTTPAddr), envHTTPAddr, cfg.HTTPAddr},
		{cfg.MetricsAddr == metricsOff || validListenAddress(cfg.MetricsAddr), envMetricsAddr, cfg.MetricsAddr},
		{validBasePath(cfg.BasePath), envBasePath, cfg.BasePath},
	}
	for _, check := range checks {
		if !check.valid {
			return malformedValue(check.variable, check.value)
		}
	}

	allowedHosts, fatal := normalizeAllowedHosts(cfg.AllowedHosts)
	if fatal != nil {
		return fatal
	}
	cfg.AllowedHosts = allowedHosts

	paths := []struct{ variable, value string }{
		{envConfigDir, cfg.ConfigDir},
		{envDBPath, cfg.DBPath},
		{envWatchDir, cfg.WatchDir},
	}
	for _, root := range cfg.DataRoots {
		paths = append(paths, struct{ variable, value string }{envDataRoots, root})
	}
	for _, path := range paths {
		if path.value != "" && !filepath.IsAbs(path.value) {
			return malformedValue(path.variable, path.value)
		}
	}

	directories := []struct{ variable, path string }{
		{envConfigDir, cfg.ConfigDir},
		{envDBPath, filepath.Dir(cfg.DBPath)},
	}
	for _, directory := range directories {
		if err := os.MkdirAll(directory.path, privateDirectoryMode); err != nil {
			return pathUnwritable(directory.variable, fmt.Errorf("create directory: %w", err))
		}
		if err := probeWritableDirectory(directory.path); err != nil {
			return pathUnwritable(directory.variable, err)
		}
	}

	// The database may live elsewhere; its opener cannot secure the secret directory.
	if err := os.Chmod(cfg.ConfigDir, privateDirectoryMode); err != nil {
		return pathUnwritable(envConfigDir, fmt.Errorf("secure directory: %w", err))
	}
	return nil
}

// validBasePath excludes URL authorities and router patterns from a static mount.
func validBasePath(value string) bool {
	if value == "" {
		return true
	}
	if !strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.ContainsAny(value, `\{}*`) {
		return false
	}

	parsed, err := url.Parse(value)
	return err == nil && parsed.Host == "" && parsed.Path == value && path.Clean(value) == value
}

func validLogLevel(value string) bool {
	switch value {
	case logLevelDebug, defaultLogLevel, logLevelWarn, logLevelError:
		return true
	default:
		return false
	}
}

func validListenAddress(value string) bool {
	host, port, err := net.SplitHostPort(value)
	if err != nil || port == "" {
		return false
	}
	if _, err = strconv.ParseUint(port, 10, 16); err != nil {
		return false
	}

	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}

	host = strings.TrimSuffix(host, ".")
	return host == "" || validDNSName(host)
}

func normalizeAllowedHosts(hosts []string) ([]string, *FatalError) {
	normalized := make([]string, 0, len(hosts))
	received := strings.Join(hosts, commaSeparator)
	for _, host := range hosts {
		host = strings.TrimSpace(host)
		host = strings.TrimSuffix(host, ".")
		if _, err := netip.ParseAddr(host); err == nil {
			return nil, malformedValue(envAllowedHosts, received)
		}
		if !validDNSName(host) {
			return nil, malformedValue(envAllowedHosts, received)
		}

		normalized = append(normalized, strings.ToLower(host))
	}

	return normalized, nil
}

func validURL(value string) bool {
	parsed, err := url.ParseRequestURI(value)
	return err == nil && parsed.IsAbs() && parsed.Host != ""
}

func validDNSName(name string) bool {
	return name != "" && len(name) <= maxDNSNameLength && dnsNameRegexp.MatchString(name)
}

// probeWritableDirectory lets data-root checks detect missing mounts without creating them.
func probeWritableDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&directoryWriteBits == 0 {
		return errors.New("directory is not writable")
	}

	probe, err := os.CreateTemp(path, ".dl-tool-write-probe-")
	if err != nil {
		return fmt.Errorf("create write probe: %w", err)
	}

	probePath := probe.Name()
	if err = errors.Join(probe.Close(), os.Remove(probePath)); err != nil {
		return fmt.Errorf("finish write probe: %w", err)
	}

	return nil
}

type generatedSecrets struct {
	secretKey      secure.Secret
	fileExisted    bool
	aria2Generated bool
	keyGenerated   bool
}

func writeSecrets(configDir string) (generatedSecrets, *FatalError) {
	path := filepath.Join(configDir, secretsFileName)
	contents, fileExisted, fatal := readSecretsFile(path)
	if fatal != nil {
		return generatedSecrets{}, fatal
	}

	values := parseSecrets(contents)
	result := generatedSecrets{fileExisted: fileExisted}
	keys := []struct {
		name      string
		generated *bool
	}{
		{aria2RPCSecretKey, &result.aria2Generated},
		{secretKeyName, &result.keyGenerated},
	}
	for _, key := range keys {
		if values[key.name] != "" {
			continue
		}

		buffer := make([]byte, secretByteCount)
		if _, err := rand.Read(buffer); err != nil {
			return generatedSecrets{}, secretGenerationFailed(key.name, err)
		}
		value := base64.RawURLEncoding.EncodeToString(buffer)
		if len(contents) > 0 && contents[len(contents)-1] != '\n' {
			contents = append(contents, '\n')
		}
		contents = append(contents, key.name+keyValueSeparator+value+unixNewline...)
		values[key.name], *key.generated = value, true
	}

	if result.aria2Generated || result.keyGenerated {
		if err := writeSecretsFile(path, contents); err != nil {
			return generatedSecrets{}, pathUnwritable(envConfigDir, err)
		}
	} else if err := os.Chmod(path, secretFileMode); err != nil {
		return generatedSecrets{}, pathUnwritable(envConfigDir, fmt.Errorf("set secrets file mode: %w", err))
	}

	result.secretKey = secure.Secret(values[secretKeyName])

	return result, nil
}

func writeSecretsFile(path string, contents []byte) (resultErr error) {
	directoryPath := filepath.Dir(path)
	temporary, err := os.CreateTemp(directoryPath, "."+secretsFileName+"-*")
	if err != nil {
		return fmt.Errorf("create secrets temp file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		if err := temporary.Close(); err != nil && !errors.Is(err, fs.ErrClosed) {
			resultErr = errors.Join(resultErr, fmt.Errorf("close secrets temp file: %w", err))
		}
		if err := os.Remove(temporaryPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove secrets temp file: %w", err))
		}
	}()

	// Sync a complete replacement before exposing it at the durable path.
	if _, err := temporary.Write(contents); err != nil {
		return fmt.Errorf("write secrets temp file: %w", err)
	}
	if err := temporary.Chmod(secretFileMode); err != nil {
		return fmt.Errorf("set secrets temp file mode: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync secrets temp file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close secrets temp file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace secrets file: %w", err)
	}
	if err := syncDirectory(directoryPath); err != nil {
		return fmt.Errorf("persist secrets file replacement: %w", err)
	}

	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open directory: %w", err)
	}

	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		syncErr = fmt.Errorf("sync directory: %w", syncErr)
	}
	if closeErr != nil {
		closeErr = fmt.Errorf("close directory: %w", closeErr)
	}

	return errors.Join(syncErr, closeErr)
}

func readSecretsFile(path string) ([]byte, bool, *FatalError) {
	// Reject links before later mode changes can follow them.
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return nil, false, secretFileUnreadable()
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, false, secretFileUnreadable()
	}

	return contents, true, nil
}

func parseSecrets(contents []byte) map[string]string {
	values := make(map[string]string, 2)
	for _, line := range strings.Split(string(contents), unixNewline) {
		name, value, found := strings.Cut(strings.TrimSpace(line), keyValueSeparator)
		if !found || name != aria2RPCSecretKey && name != secretKeyName {
			continue
		}
		values[name] = value
	}

	return values
}

func warnRegeneratedSecrets(ctx context.Context, generated generatedSecrets) {
	if !generated.fileExisted {
		return
	}

	if generated.keyGenerated {
		slog.WarnContext(ctx, "at-rest secret key regenerated", "event_code", eventSecretKeyRegenerated)
	}

	if generated.aria2Generated {
		slog.WarnContext(ctx, "aria2 rpc secret rotated", "event_code", eventAria2SecretRotated)
	}
}

func warnUnavailablePaths(ctx context.Context, cfg *Config) {
	for _, root := range cfg.DataRoots {
		err := probeWritableDirectory(root)
		if err == nil {
			continue
		}

		warn(ctx, "data root is not writable", warningDataRootNotWritable, "root", root, "err", err)
	}

	warnMissingBinary(ctx, envYtdlpPath, cfg.YtdlpPath, warningBinaryMissing)
	warnMissingBinary(ctx, envJSRuntimePath, cfg.JSRuntimePath, warningJSRuntimeMissing)
	warnMissingBinary(ctx, envSevenzipPath, cfg.SevenzipPath, warningBinaryMissing)

	if cfg.NotifyURL != "" && !validURL(cfg.NotifyURL) {
		warn(ctx, "notification URL is malformed", errorCodeMalformed, "variable", envNotifyURL)
		cfg.NotifyURL = ""
	}

	if cfg.WatchDir == "" || withinAnyRoot(cfg.WatchDir, cfg.DataRoots) {
		return
	}

	warn(ctx, "watch directory is outside every data root", warningOutOfRoot, "variable", envWatchDir)
	cfg.WatchDir = ""
}

func warnMissingBinary(ctx context.Context, variable, path, code string) {
	_, err := exec.LookPath(path)
	if err == nil {
		return
	}

	warn(ctx, "configured binary is unavailable", code, "variable", variable, "path", path, "err", err)
}

func warn(ctx context.Context, message, code string, attributes ...any) {
	attributes = append([]any{errorCodeAttribute, code}, attributes...)
	slog.WarnContext(ctx, message, attributes...)
}

func withinAnyRoot(path string, roots []string) bool {
	for _, root := range roots {
		relative, err := filepath.Rel(root, path)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
	}

	return false
}

func missingVariable(variable string) *FatalError {
	return newFatal(errorCodeMissing, variable, "required variable is unset")
}

func pathUnwritable(variable string, cause error) *FatalError {
	return newFatal(errorCodePathUnwritable, variable, cause.Error())
}

func secretFileUnreadable() *FatalError {
	return newFatal(errorCodeSecretUnreadable, secretKeyName, "secrets file is unreadable")
}

func secretGenerationFailed(variable string, cause error) *FatalError {
	return newFatal(errorCodeSecretUnreadable, variable, cause.Error())
}

func newFatal(code, variable, detail string) *FatalError {
	return &FatalError{Code: code, Variable: variable, Detail: detail}
}
