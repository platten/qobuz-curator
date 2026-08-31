// Package config loads and validates the application's runtime settings.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/kirsle/configdir"
	"github.com/pawel/qobuz-curator/internal/security"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// Config contains the fully merged and validated runtime configuration.
type Config struct {
	DataDir         string   `mapstructure:"data_dir" yaml:"data_dir"`
	DatabaseName    string   `mapstructure:"database_name" yaml:"database_name"`
	Host            string   `mapstructure:"host" yaml:"host"`
	AllowedHosts    []string `mapstructure:"allowed_hosts" yaml:"allowed_hosts"`
	Port            int      `mapstructure:"port" yaml:"port"`
	MatchThreshold  float64  `mapstructure:"match_threshold" yaml:"match_threshold"`
	PreviewTTLHours int      `mapstructure:"preview_ttl_hours" yaml:"preview_ttl_hours"`
	Provider        string   `mapstructure:"provider" yaml:"provider"`
	QobuzAppID      string   `mapstructure:"qobuz_app_id" yaml:"qobuz_app_id"`
	QobuzUserToken  string   `mapstructure:"qobuz_user_auth_token" yaml:"qobuz_user_auth_token"`
	QobuzAPIBase    string   `mapstructure:"qobuz_api_base" yaml:"qobuz_api_base"`
	OpenAIAPIKey    string   `mapstructure:"openai_api_key" yaml:"openai_api_key"`
	OpenAIModel     string   `mapstructure:"openai_model" yaml:"openai_model"`
	OpenAIAPIBase   string   `mapstructure:"openai_api_base" yaml:"openai_api_base"`
	SessionSecret   string   `mapstructure:"session_secret" yaml:"session_secret"`
	SessionTTLHours int      `mapstructure:"session_ttl_hours" yaml:"session_ttl_hours"`
	PasswordHash    string   `mapstructure:"password_hash" yaml:"password_hash"`
	AuthDisabled    bool     `mapstructure:"auth_disabled" yaml:"auth_disabled"`
	SecureCookies   bool     `mapstructure:"secure_cookies" yaml:"secure_cookies"`
	BrowserDisabled bool     `mapstructure:"no_browser" yaml:"no_browser"`
	LogLevel        string   `mapstructure:"log_level" yaml:"log_level"`
	LogFormat       string   `mapstructure:"log_format" yaml:"log_format"`
	LogColor        bool     `mapstructure:"log_color" yaml:"log_color"`
}

var localConfigDir = configdir.LocalConfig

// DefaultDir returns the platform-native per-user configuration directory.
// It follows XDG_CONFIG_HOME on Unix, Library/Application Support on macOS,
// and AppData on Windows through kirsle/configdir.
func DefaultDir() string { return localConfigDir("qobuz-curator") }

// DefaultPath returns the default YAML configuration file location.
func DefaultPath() string { return filepath.Join(DefaultDir(), "qobuz-curator.yaml") }

func defaults(v *viper.Viper) {
	v.SetDefault("data_dir", "./data")
	v.SetDefault("database_name", "qobuz_curator.sqlite3")
	v.SetDefault("host", "127.0.0.1")
	// Port zero asks the operating system to reserve a currently unused
	// ephemeral port. This avoids both common-port collisions and the
	// check-then-bind race inherent in probing a port before listening.
	v.SetDefault("port", 0)
	v.SetDefault("match_threshold", 0.72)
	v.SetDefault("preview_ttl_hours", 72)
	v.SetDefault("provider", "fake")
	v.SetDefault("qobuz_api_base", "https://www.qobuz.com/api.json/0.2")
	v.SetDefault("openai_model", "gpt-5.6-luna")
	v.SetDefault("openai_api_base", "https://api.openai.com/v1")
	v.SetDefault("session_ttl_hours", 24)
	v.SetDefault("auth_disabled", true)
	v.SetDefault("log_level", "info")
	v.SetDefault("log_format", "console")
	v.SetDefault("log_color", true)
}

// Initial returns a complete, validated configuration suitable for writing to
// a new per-user configuration file. Unlike runtime defaults, it persists a
// cryptographically random session secret so authentication can later be
// enabled without reusing an ephemeral secret.
func Initial() (Config, error) {
	v := viper.New()
	defaults(v)
	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode initial configuration: %w", err)
	}
	secret, err := newSessionSecret()
	if err != nil {
		return Config{}, err
	}
	cfg.SessionSecret = secret
	return cfg, cfg.Validate()
}

func newSessionSecret() (string, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("generate session secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(secret), nil
}

// Load applies defaults, YAML, environment variables, and changed flags in
// increasing precedence order.
func Load(path string, flags *pflag.FlagSet) (Config, *viper.Viper, error) {
	v := viper.New()
	defaults(v)
	v.SetEnvPrefix("QOBUZ_CURATOR")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	if flags != nil {
		if err := v.BindPFlags(flags); err != nil {
			return Config{}, v, err
		}
	}
	if path != "" {
		v.SetConfigFile(path)
	} else {
		v.SetConfigFile(DefaultPath())
	}
	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if path != "" || (!errors.As(err, &notFound) && !errors.Is(err, os.ErrNotExist)) {
			return Config{}, v, fmt.Errorf("read configuration: %w", err)
		}
	}
	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return Config{}, v, fmt.Errorf("decode configuration: %w", err)
	}
	if cfg.SessionSecret == "" && !cfg.AuthDisabled {
		return Config{}, v, fmt.Errorf("session_secret must be set explicitly when authentication is enabled")
	}
	if cfg.SessionSecret == "" {
		secret, err := newSessionSecret()
		if err != nil {
			return Config{}, v, fmt.Errorf("generate ephemeral session secret: %w", err)
		}
		cfg.SessionSecret = secret
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, v, err
	}
	return cfg, v, nil
}

// Validate rejects unsafe or internally inconsistent settings.
func (c Config) Validate() error {
	if strings.TrimSpace(c.DataDir) == "" {
		return fmt.Errorf("data_dir must not be blank")
	}
	if c.DatabaseName == "" || c.DatabaseName != filepath.Base(c.DatabaseName) || c.DatabaseName == "." {
		return fmt.Errorf("database_name must be a file name, not a path")
	}
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("port must be zero (automatic) or between 1 and 65535")
	}
	for _, host := range c.AllowedHosts {
		host = strings.TrimSpace(host)
		normalized := strings.Trim(host, "[]")
		if host == "" || strings.ContainsAny(host, "/?#@*") ||
			(net.ParseIP(normalized) == nil && strings.Contains(normalized, ":")) {
			return fmt.Errorf("allowed_hosts entries must be host names or IP addresses")
		}
	}
	if c.MatchThreshold < 0 || c.MatchThreshold > 1 {
		return fmt.Errorf("match_threshold must be between 0 and 1")
	}
	if c.PreviewTTLHours < 1 || c.PreviewTTLHours > 720 {
		return fmt.Errorf("preview_ttl_hours must be between 1 and 720")
	}
	if c.SessionTTLHours < 1 || c.SessionTTLHours > 720 {
		return fmt.Errorf("session_ttl_hours must be between 1 and 720")
	}
	if c.Provider != "fake" && c.Provider != "qobuz" {
		return fmt.Errorf("provider must be fake or qobuz")
	}
	if strings.TrimSpace(c.OpenAIModel) == "" {
		return fmt.Errorf("openai_model must not be blank")
	}
	switch strings.ToLower(strings.TrimSpace(c.LogLevel)) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level must be debug, info, warn, or error")
	}
	switch strings.ToLower(strings.TrimSpace(c.LogFormat)) {
	case "console", "json":
	default:
		return fmt.Errorf("log_format must be console or json")
	}
	for name, value := range map[string]string{
		"qobuz_api_base":  c.QobuzAPIBase,
		"openai_api_base": c.OpenAIAPIBase,
	} {
		if err := validateAPIURL(name, value); err != nil {
			return err
		}
	}
	if len(c.SessionSecret) < 32 {
		return fmt.Errorf("session_secret must contain at least 32 characters")
	}
	if !c.AuthDisabled {
		if !security.PasswordHashValid(c.PasswordHash) {
			return fmt.Errorf("password_hash must be generated by the password-hash command when authentication is enabled")
		}
		secret := strings.ToLower(c.SessionSecret)
		if strings.Contains(secret, "change-me") || strings.Contains(secret, "replace-me") || strings.Contains(secret, "example") {
			return fmt.Errorf("session_secret contains a known placeholder; generate a random secret")
		}
	}
	return nil
}

func (c Config) DatabasePath() string { return filepath.Join(c.DataDir, c.DatabaseName) }

// validateAPIURL prevents credentials from being sent over cleartext networks.
// HTTP remains available for loopback endpoints used by local development and tests.
func validateAPIURL(name, value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("%s must be an absolute HTTP(S) URL", name)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s must not contain credentials, a query, or a fragment", name)
	}
	if u.Scheme == "http" {
		host := u.Hostname()
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return fmt.Errorf("%s must use HTTPS unless it points to loopback", name)
		}
	}
	return nil
}
