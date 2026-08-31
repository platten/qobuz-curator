package cli

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pawel/qobuz-curator/internal/config"
)

func TestInitCreatesAndProtectsConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "qobuz-curator.yaml")
	cmd := NewRoot("test")
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"init", "--config", path, "--color", "never"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, key := range []string{"provider: fake", "session_secret:", "openai_api_key:", "qobuz_user_auth_token:"} {
		if !strings.Contains(text, key) {
			t.Fatalf("generated configuration is missing %q:\n%s", key, text)
		}
	}
	if !strings.Contains(output.String(), "Configuration created") {
		t.Fatal(output.String())
	}

	again := NewRoot("test")
	again.SetArgs([]string{"init", "--config", path})
	if err = again.Execute(); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("existing file was not protected: %v", err)
	}

	forced := NewRoot("test")
	forced.SetOut(&output)
	forced.SetArgs([]string{"init", "--force", "--config", path, "--color", "never"})
	if err = forced.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestInteractiveInit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "interactive.yaml")
	answers := []string{
		t.TempDir(), // data directory
		"",          // database name
		"",          // host
		"localhost", // allowed hosts
		"0",         // port
		"qobuz",
		"app-id",
		"qobuz-secret-token",
		"", // Qobuz API base
		"openai-secret-key",
		"", // OpenAI model
		"", // OpenAI API base
		"0.8",
		"48",
		"yes", // authentication disabled
		"12",
		"no",
		"yes", // no browser
		"debug",
		"json",
		"no", // log color
	}
	cmd := NewRoot("test")
	var output bytes.Buffer
	cmd.SetIn(strings.NewReader(strings.Join(answers, "\n") + "\n"))
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"init", "--interactive", "--config", path, "--color", "never"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "qobuz-secret-token") || strings.Contains(output.String(), "openai-secret-key") {
		t.Fatal("interactive output disclosed a secret")
	}
	cfg, _, err := config.Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "qobuz" || cfg.QobuzAppID != "app-id" || cfg.QobuzUserToken != "qobuz-secret-token" || cfg.OpenAIAPIKey != "openai-secret-key" {
		t.Fatal("interactive values were not saved")
	}
	if cfg.MatchThreshold != 0.8 || cfg.PreviewTTLHours != 48 || cfg.SessionTTLHours != 12 || !cfg.BrowserDisabled || cfg.LogFormat != "json" || cfg.LogColor {
		t.Fatal("interactive non-secret values were not saved")
	}
}

func TestInteractivePromptValidation(t *testing.T) {
	prompter := &configPrompter{reader: bufio.NewReader(bytes.NewBufferString("not-a-number\n")), input: bytes.NewBuffer(nil), output: &bytes.Buffer{}}
	if _, err := prompter.integer("Port", 0); err == nil {
		t.Fatal("invalid integer accepted")
	}
	prompter = &configPrompter{reader: bufio.NewReader(bytes.NewBufferString("maybe\n")), input: bytes.NewBuffer(nil), output: &bytes.Buffer{}}
	if _, err := prompter.boolean("Enabled", false); err == nil {
		t.Fatal("invalid boolean accepted")
	}
	if got := splitCommaSeparated(" one, ,two "); len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("unexpected list: %#v", got)
	}
}
