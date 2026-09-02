package application

import (
	"path/filepath"
	"testing"

	"github.com/pawel/qobuz-curator/internal/config"
)

func TestOpenAndClose(t *testing.T) {
	cfg, err := config.Initial()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = t.TempDir()
	runtime, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Handler() == nil || runtime.Config.DatabasePath() != filepath.Join(cfg.DataDir, cfg.DatabaseName) {
		t.Fatal("runtime was not assembled")
	}
	if err = runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if err = runtime.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRequiresQobuzCredentials(t *testing.T) {
	cfg, err := config.Initial()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Provider = "qobuz"
	if _, err = Open(cfg); err == nil {
		t.Fatal("expected missing credential error")
	}
}
