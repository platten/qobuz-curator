package configfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pawel/qobuz-curator/internal/config"
)

func TestSaveWritesValidProtectedConfiguration(t *testing.T) {
	cfg, err := config.Initial()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	cfg.QobuzUserToken = "qobuz-test-secret"
	cfg.OpenAIAPIKey = "openai-test-secret"
	path := filepath.Join(t.TempDir(), "nested", "desktop.yaml")
	if err = Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "operating-system vault") || strings.Contains(string(raw), "qobuz-test-secret") || strings.Contains(string(raw), "openai-test-secret") {
		t.Fatalf("unexpected output: %s", raw)
	}
	t.Setenv("QOBUZ_CURATOR_DATA_DIR", "/environment-must-not-win")
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DataDir != cfg.DataDir || loaded.SessionSecret != cfg.SessionSecret {
		t.Fatalf("round trip mismatch: %#v", loaded)
	}
}

func TestLoadRejectsInvalidConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "desktop.yaml")
	if err := os.WriteFile(path, []byte("match_threshold: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestSaveRejectsInvalidConfiguration(t *testing.T) {
	if err := Save(filepath.Join(t.TempDir(), "desktop.yaml"), config.Config{}); err == nil {
		t.Fatal("expected validation error")
	}
}
