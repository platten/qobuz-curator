package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

// clearApplicationEnvironment keeps configuration tests independent of the
// developer machine and, importantly, prevents a failed assertion from
// rendering ambient credentials as part of a Config value.
func clearApplicationEnvironment(t *testing.T) {
	t.Helper()
	for _, setting := range os.Environ() {
		key, _, _ := strings.Cut(setting, "=")
		if strings.HasPrefix(key, "QOBUZ_CURATOR_") {
			t.Setenv(key, "")
		}
	}
}

func isolateDefaultConfig(t *testing.T) string {
	t.Helper()
	original := localConfigDir
	directory := t.TempDir()
	localConfigDir = func(...string) string { return directory }
	t.Cleanup(func() { localConfigDir = original })
	return directory
}

func TestLoadPrecedence(t *testing.T) {
	clearApplicationEnvironment(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if e := os.WriteFile(path, []byte("port: 8000\nprovider: fake\ndata_dir: from-file\n"), 0600); e != nil {
		t.Fatal(e)
	}
	t.Setenv("QOBUZ_CURATOR_PORT", "8001")
	flags := pflag.NewFlagSet("x", pflag.ContinueOnError)
	flags.Int("port", 0, "")
	_ = flags.Set("port", "8002")
	cfg, v, e := Load(path, flags)
	if e != nil {
		t.Fatal(e)
	}
	if cfg.Port != 8002 || cfg.DataDir != "from-file" || v.ConfigFileUsed() != path {
		t.Fatalf("unexpected precedence: port=%d data_dir=%q config=%q", cfg.Port, cfg.DataDir, v.ConfigFileUsed())
	}
}
func TestDefaultsAndValidation(t *testing.T) {
	clearApplicationEnvironment(t)
	isolateDefaultConfig(t)
	cfg, _, e := Load("", nil)
	if e != nil {
		t.Fatal(e)
	}
	if cfg.Port != 0 || cfg.Provider != "fake" {
		t.Fatalf("unexpected defaults: port=%d provider=%q", cfg.Port, cfg.Provider)
	}
	cases := []Config{{Port: 0, MatchThreshold: .5, PreviewTTLHours: 1, Provider: "fake", AuthDisabled: true}, {Port: 1, MatchThreshold: 2, PreviewTTLHours: 1, Provider: "fake", AuthDisabled: true}, {Port: 1, MatchThreshold: .5, PreviewTTLHours: 0, Provider: "fake", AuthDisabled: true}, {Port: 1, MatchThreshold: .5, PreviewTTLHours: 1, Provider: "bad", AuthDisabled: true}, {Port: 1, MatchThreshold: .5, PreviewTTLHours: 1, Provider: "fake", AuthDisabled: false}}
	for i, c := range cases {
		if c.Validate() == nil {
			t.Errorf("case %d", i)
		}
	}
}

func TestInitialConfiguration(t *testing.T) {
	clearApplicationEnvironment(t)
	cfg, err := Initial()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "fake" || cfg.Host != "127.0.0.1" || len(cfg.SessionSecret) < 32 {
		t.Fatalf("unexpected initial configuration: provider=%q host=%q secret_length=%d", cfg.Provider, cfg.Host, len(cfg.SessionSecret))
	}
	second, err := Initial()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SessionSecret == second.SessionSecret {
		t.Fatal("initial configurations reused a session secret")
	}
}

func TestPlatformDefaultConfigurationPath(t *testing.T) {
	clearApplicationEnvironment(t)
	directory := isolateDefaultConfig(t)
	if got, want := DefaultDir(), directory; got != want {
		t.Fatalf("DefaultDir()=%q, want %q", got, want)
	}
	path := DefaultPath()
	if want := filepath.Join(directory, "qobuz-curator.yaml"); path != want {
		t.Fatalf("DefaultPath()=%q, want %q", path, want)
	}
	if err := os.WriteFile(path, []byte("provider: fake\nport: 43210\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, v, err := Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 43210 || v.ConfigFileUsed() != path {
		t.Fatalf("default configuration was not loaded: port=%d path=%q", cfg.Port, v.ConfigFileUsed())
	}
}

func validConfig() Config {
	return Config{
		DataDir: ".", DatabaseName: "db.sqlite", Host: "127.0.0.1", Port: 8100,
		MatchThreshold: .72, PreviewTTLHours: 72, SessionTTLHours: 24,
		Provider: "fake", QobuzAPIBase: "https://www.qobuz.com/api.json/0.2",
		OpenAIModel: "test", OpenAIAPIBase: "https://api.openai.com/v1", SessionSecret: strings.Repeat("s", 32),
		AuthDisabled: true, LogLevel: "info", LogFormat: "console",
	}
}

func TestValidationBranches(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	mutations := []func(*Config){
		func(c *Config) { c.DataDir = " " },
		func(c *Config) { c.DatabaseName = "../db" },
		func(c *Config) { c.Port = 70_000 },
		func(c *Config) { c.AllowedHosts = []string{"bad/path"} },
		func(c *Config) { c.AllowedHosts = []string{"example.test:8443"} },
		func(c *Config) { c.AllowedHosts = []string{"*.example.test"} },
		func(c *Config) { c.MatchThreshold = -1 },
		func(c *Config) { c.PreviewTTLHours = 721 },
		func(c *Config) { c.SessionTTLHours = 0 },
		func(c *Config) { c.Provider = "other" },
		func(c *Config) { c.OpenAIModel = " " },
		func(c *Config) { c.QobuzAPIBase = "not-a-url" },
		func(c *Config) { c.OpenAIAPIBase = "http://example.test/v1" },
		func(c *Config) { c.OpenAIAPIBase = "https://user:pass@example.test/v1" },
		func(c *Config) { c.OpenAIAPIBase = "https://example.test/v1?q=x" },
		func(c *Config) { c.SessionSecret = "short" },
		func(c *Config) { c.LogLevel = "verbose" },
		func(c *Config) { c.LogFormat = "xml" },
	}
	for i, mutate := range mutations {
		cfg := validConfig()
		mutate(&cfg)
		if cfg.Validate() == nil {
			t.Errorf("mutation %d should fail: %#v", i, cfg)
		}
	}
	if err := func() error {
		cfg := validConfig()
		cfg.OpenAIAPIBase = "http://localhost:9999/v1"
		return cfg.Validate()
	}(); err != nil {
		t.Fatal(err)
	}
	if got := validConfig().DatabasePath(); got != filepath.Join(".", "db.sqlite") {
		t.Fatal(got)
	}
}

func TestAuthenticatedConfiguration(t *testing.T) {
	clearApplicationEnvironment(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("auth_disabled: false\npassword_hash: invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path, nil); err == nil || !strings.Contains(err.Error(), "session_secret") {
		t.Fatal(err)
	}
	cfg := validConfig()
	cfg.AuthDisabled = false
	cfg.PasswordHash = "invalid"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "password_hash") {
		t.Fatal(err)
	}
	cfg.PasswordHash = "scrypt$131072$8$1$MDEyMzQ1Njc4OWFiY2RlZg$MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY"
	cfg.SessionSecret = "change-me-in-production-with-a-long-value"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "placeholder") {
		t.Fatal(err)
	}
}
func TestMissingExplicitConfig(t *testing.T) {
	if _, _, e := Load(filepath.Join(t.TempDir(), "missing.yaml"), nil); e == nil {
		t.Fatal("expected error")
	}
}
