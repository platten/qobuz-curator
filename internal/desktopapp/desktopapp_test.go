package desktopapp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/pawel/qobuz-curator/internal/config"
	"github.com/pawel/qobuz-curator/internal/credentials"
	"github.com/pawel/qobuz-curator/internal/qobuzauth"
	"go.yaml.in/yaml/v3"
)

type memoryVault struct {
	mu     sync.Mutex
	values map[string]string
	err    error
}

func (v *memoryVault) Get(name string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.err != nil {
		return "", v.err
	}
	value, ok := v.values[name]
	if !ok {
		return "", credentials.ErrNotFound
	}
	return value, nil
}
func (v *memoryVault) Set(name, value string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.err != nil {
		return v.err
	}
	v.values[name] = value
	return nil
}
func (v *memoryVault) Delete(name string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.err != nil {
		return v.err
	}
	delete(v.values, name)
	return nil
}

type fakeAuth struct{ err error }

func (f fakeAuth) DiscoverAppID(context.Context) (string, error) { return "123456", f.err }
func (f fakeAuth) ReceiveCode(context.Context, string, int) (string, string, error) {
	return "code", "https://login.example", f.err
}
func (f fakeAuth) Exchange(context.Context, string, string, string) (qobuzauth.Credentials, error) {
	return qobuzauth.Credentials{AppID: "123456", Token: "qobuz-secret", DisplayName: "Listener"}, f.err
}

func newDesktopTestApp(t *testing.T, vault *memoryVault) *App {
	t.Helper()
	app, err := New(Options{
		ConfigPath: filepath.Join(t.TempDir(), "config", desktopConfigName),
		Vault:      vault, Auth: fakeAuth{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	return app
}

func postForm(app *App, path string, values url.Values) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "http://wails.localhost"+path, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, request)
	return recorder
}

func TestGraphicalSetupConnectsAndStoresSecretsOnlyInVault(t *testing.T) {
	vault := &memoryVault{values: map[string]string{}}
	app := newDesktopTestApp(t, vault)
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://wails.localhost/", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "Connect Qobuz") {
		t.Fatalf("setup response: %d %s", recorder.Code, recorder.Body.String())
	}
	recorder = postForm(app, "/desktop/qobuz", url.Values{"csrf": {app.csrf}})
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "Connected to Qobuz as Listener") {
		t.Fatalf("connect response: %d %s", recorder.Code, recorder.Body.String())
	}
	if vault.values[credentials.QobuzToken] != "qobuz-secret" || app.runtime == nil {
		t.Fatal("credential or runtime was not installed")
	}
	raw, err := os.ReadFile(app.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "qobuz-secret") {
		t.Fatal("desktop configuration contains a secret")
	}
	if !strings.Contains(string(raw), "provider: qobuz") {
		t.Fatalf("configuration was not updated: %s", raw)
	}

	recorder = postForm(app, "/desktop/openai", url.Values{"csrf": {app.csrf}, "openai_api_key": {"openai-secret"}})
	if recorder.Code != http.StatusOK || vault.values[credentials.OpenAIAPIKey] != "openai-secret" {
		t.Fatalf("OpenAI save failed: %d %s", recorder.Code, recorder.Body.String())
	}
	raw, _ = os.ReadFile(app.configPath)
	if strings.Contains(string(raw), "openai-secret") {
		t.Fatal("configuration contains OpenAI secret")
	}

	recorder = postForm(app, "/desktop/qobuz/disconnect", url.Values{"csrf": {app.csrf}})
	if recorder.Code != http.StatusOK || app.runtime != nil {
		t.Fatalf("disconnect failed: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestSetupCSRFPreferencesAndStaticAssets(t *testing.T) {
	app := newDesktopTestApp(t, &memoryVault{values: map[string]string{}})
	recorder := postForm(app, "/desktop/preferences", url.Values{"csrf": {"wrong"}})
	if recorder.Code != http.StatusForbidden {
		t.Fatal(recorder.Code)
	}
	recorder = postForm(app, "/desktop/preferences", url.Values{
		"csrf": {app.csrf}, "openai_model": {"gpt-test"}, "match_threshold": {"0.81"}, "log_level": {"warn"},
	})
	if recorder.Code != http.StatusOK || app.config.OpenAIModel != "gpt-test" || app.config.MatchThreshold != 0.81 || app.config.LogLevel != "warn" {
		t.Fatalf("preferences failed: %d %s", recorder.Code, recorder.Body.String())
	}
	recorder = postForm(app, "/desktop/preferences", url.Values{
		"csrf": {app.csrf}, "openai_model": {"gpt-test"}, "match_threshold": {"not-a-number"}, "log_level": {"warn"},
	})
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatal(recorder.Code)
	}
	recorder = httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://wails.localhost/desktop/static/setup.css", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "--paper") {
		t.Fatalf("static response: %d %s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://wails.localhost/missing", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatal(recorder.Code)
	}
}

func TestVaultFailureIsGraphicalAndFailsClosed(t *testing.T) {
	vault := &memoryVault{values: map[string]string{}, err: errors.New("keyring locked")}
	app := newDesktopTestApp(t, vault)
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://wails.localhost/", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "keyring locked") || !strings.Contains(recorder.Body.String(), "will not save credentials in a file") {
		t.Fatalf("vault diagnostic missing: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestLegacyCredentialsAreCopiedToVaultWithoutChangingLegacyFile(t *testing.T) {
	directory := t.TempDir()
	legacyPath := filepath.Join(directory, "qobuz-curator.yaml")
	cfg, err := config.Initial()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = filepath.Join(directory, "shared-data")
	cfg.Provider = "qobuz"
	cfg.QobuzAppID = "123456"
	cfg.QobuzUserToken = "legacy-qobuz-secret"
	cfg.OpenAIAPIKey = "legacy-openai-secret"
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(legacyPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	vault := &memoryVault{values: map[string]string{}}
	configPath := filepath.Join(directory, desktopConfigName)
	app, err := New(Options{ConfigPath: configPath, LegacyConfigPath: legacyPath, Vault: vault, Auth: fakeAuth{}})
	if err != nil {
		t.Fatal(err)
	}
	if app.runtime == nil || vault.values[credentials.QobuzToken] != "legacy-qobuz-secret" || vault.values[credentials.OpenAIAPIKey] != "legacy-openai-secret" {
		t.Fatal("legacy settings were not migrated")
	}
	desktopRaw, _ := os.ReadFile(configPath)
	legacyRaw, _ := os.ReadFile(legacyPath)
	if strings.Contains(string(desktopRaw), "legacy-qobuz-secret") || !strings.Contains(string(legacyRaw), "legacy-qobuz-secret") {
		t.Fatal("desktop leaked a secret or changed the additive CLI configuration")
	}
	if err = app.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := New(Options{ConfigPath: configPath, LegacyConfigPath: legacyPath, Vault: vault, Auth: fakeAuth{}})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.runtime == nil {
		t.Fatal("existing desktop configuration did not reopen")
	}
	recorder := postForm(reopened, "/desktop/openai", url.Values{"csrf": {reopened.csrf}, "remove": {"true"}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("remove OpenAI key: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestQobuzAuthenticationFailureIsRendered(t *testing.T) {
	app, err := New(Options{
		ConfigPath: filepath.Join(t.TempDir(), desktopConfigName),
		Vault:      &memoryVault{values: map[string]string{}},
		Auth:       fakeAuth{err: errors.New("Qobuz unavailable")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	recorder := postForm(app, "/desktop/qobuz", url.Values{"csrf": {app.csrf}})
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "Qobuz unavailable") {
		t.Fatalf("authentication error missing: %d %s", recorder.Code, recorder.Body.String())
	}
}
