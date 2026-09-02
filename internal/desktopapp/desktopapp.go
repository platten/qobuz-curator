// Package desktopapp adapts the existing web application to a private Wails window.
package desktopapp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pawel/qobuz-curator/internal/application"
	"github.com/pawel/qobuz-curator/internal/config"
	"github.com/pawel/qobuz-curator/internal/configfile"
	"github.com/pawel/qobuz-curator/internal/credentials"
	"github.com/pawel/qobuz-curator/internal/qobuzauth"
)

//go:embed assets
var setupAssets embed.FS

const desktopConfigName = "qobuz-curator-desktop.yaml"

type authClient interface {
	DiscoverAppID(context.Context) (string, error)
	ReceiveCode(context.Context, string, int) (string, string, error)
	Exchange(context.Context, string, string, string) (qobuzauth.Credentials, error)
}

// Options supplies replaceable desktop boundaries for production and tests.
type Options struct {
	ConfigPath       string
	LegacyConfigPath string
	Vault            credentials.Store
	Auth             authClient
}

// SystemOptions selects native paths, OS credential storage, and live Qobuz authentication.
func SystemOptions() Options {
	return Options{
		ConfigPath:       filepath.Join(config.DefaultDir(), desktopConfigName),
		LegacyConfigPath: config.DefaultPath(),
		Vault:            credentials.SystemStore{},
		Auth:             qobuzauth.New(),
	}
}

// App owns graphical setup and swaps in the shared runtime after authentication.
type App struct {
	mu            sync.RWMutex
	actions       sync.Mutex
	configPath    string
	legacyPath    string
	vault         credentials.Store
	auth          authClient
	config        config.Config
	runtime       *application.Runtime
	appHandler    http.Handler
	csrf          string
	vaultError    string
	templates     *template.Template
	staticHandler http.Handler
}

type setupPage struct {
	CSRF, Error, Success, VaultError, VaultName, ConfigPath, DataDir string
	OpenAIModel, MatchThreshold, LogLevel                            string
	Ready, QobuzConnected, OpenAIConfigured                          bool
}

// New initializes non-secret desktop configuration and attempts to unlock the OS vault.
func New(options Options) (*App, error) {
	if options.ConfigPath == "" || options.Vault == nil || options.Auth == nil {
		return nil, fmt.Errorf("desktop configuration path, credential store, and Qobuz authenticator are required")
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return nil, fmt.Errorf("generate desktop CSRF token: %w", err)
	}
	templates, err := template.ParseFS(setupAssets, "assets/setup.html")
	if err != nil {
		return nil, err
	}
	static, err := fs.Sub(setupAssets, "assets")
	if err != nil {
		return nil, err
	}
	app := &App{
		configPath: options.ConfigPath, legacyPath: options.LegacyConfigPath,
		vault: options.Vault, auth: options.Auth, csrf: base64.RawURLEncoding.EncodeToString(random),
		templates: templates, staticHandler: http.StripPrefix("/desktop/static/", http.FileServer(http.FS(static))),
	}
	if err = app.loadOrCreateConfiguration(); err != nil {
		return nil, err
	}
	if err = app.reload(); err != nil {
		app.vaultError = err.Error()
	}
	return app, nil
}

func (a *App) loadOrCreateConfiguration() error {
	if _, err := os.Stat(a.configPath); err == nil {
		cfg, loadErr := configfile.Load(a.configPath)
		if loadErr != nil {
			return loadErr
		}
		cfg.QobuzUserToken, cfg.OpenAIAPIKey = "", ""
		a.config = cfg
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect desktop configuration: %w", err)
	}
	cfg, err := config.Initial()
	if err != nil {
		return err
	}
	cfg.DataDir = filepath.Join(filepath.Dir(a.configPath), "data")
	cfg.Provider = "fake"
	cfg.AuthDisabled = true
	cfg.SecureCookies = false
	cfg.BrowserDisabled = true
	cfg.LogColor = false
	if a.legacyPath != "" {
		if _, statErr := os.Stat(a.legacyPath); statErr == nil {
			legacy, loadErr := configfile.Load(a.legacyPath)
			if loadErr == nil {
				cfg.MatchThreshold, cfg.PreviewTTLHours = legacy.MatchThreshold, legacy.PreviewTTLHours
				cfg.OpenAIModel, cfg.LogLevel, cfg.LogFormat = legacy.OpenAIModel, legacy.LogLevel, legacy.LogFormat
				cfg.QobuzAPIBase, cfg.OpenAIAPIBase = legacy.QobuzAPIBase, legacy.OpenAIAPIBase
				if filepath.IsAbs(legacy.DataDir) {
					cfg.DataDir = legacy.DataDir
				}
				if legacy.QobuzAppID != "" {
					cfg.QobuzAppID = legacy.QobuzAppID
				}
				if legacy.QobuzUserToken != "" {
					if setErr := a.vault.Set(credentials.QobuzToken, legacy.QobuzUserToken); setErr != nil {
						a.vaultError = setErr.Error()
					} else {
						cfg.Provider = "qobuz"
					}
				}
				if legacy.OpenAIAPIKey != "" {
					if setErr := a.vault.Set(credentials.OpenAIAPIKey, legacy.OpenAIAPIKey); setErr != nil {
						a.vaultError = setErr.Error()
					}
				}
			}
		}
	}
	cfg.QobuzUserToken, cfg.OpenAIAPIKey = "", ""
	a.config = cfg
	return configfile.Save(a.configPath, cfg)
}

func (a *App) effectiveConfig() (config.Config, error) {
	cfg := a.config
	cfg.QobuzUserToken, cfg.OpenAIAPIKey = "", ""
	token, err := a.vault.Get(credentials.QobuzToken)
	if err == nil {
		cfg.QobuzUserToken = token
	} else if !errors.Is(err, credentials.ErrNotFound) {
		return cfg, err
	}
	key, err := a.vault.Get(credentials.OpenAIAPIKey)
	if err == nil {
		cfg.OpenAIAPIKey = key
	} else if !errors.Is(err, credentials.ErrNotFound) {
		return cfg, err
	}
	return cfg, nil
}

func (a *App) reload() error {
	cfg, err := a.effectiveConfig()
	if err != nil {
		return err
	}
	if cfg.Provider != "qobuz" || cfg.QobuzAppID == "" || cfg.QobuzUserToken == "" {
		a.replaceRuntime(nil)
		return nil
	}
	desktopCfg := cfg
	desktopCfg.Host = "127.0.0.1"
	desktopCfg.AllowedHosts = append(append([]string{}, desktopCfg.AllowedHosts...), "wails.localhost")
	next, err := application.Open(desktopCfg)
	if err != nil {
		return err
	}
	next.Web.SettingsURL = "/desktop/settings"
	a.mu.Lock()
	previous := a.runtime
	a.runtime = next
	a.appHandler = RedirectCompatible(next.Handler())
	a.mu.Unlock()
	if previous != nil {
		_ = previous.Close()
	}
	return nil
}

func (a *App) replaceRuntime(next *application.Runtime) {
	a.mu.Lock()
	previous := a.runtime
	a.runtime, a.appHandler = next, nil
	a.mu.Unlock()
	if previous != nil {
		_ = previous.Close()
	}
}

// Close cleanly shuts down the active application runtime.
func (a *App) Close() error {
	a.actions.Lock()
	defer a.actions.Unlock()
	a.mu.Lock()
	active := a.runtime
	a.runtime, a.appHandler = nil, nil
	a.mu.Unlock()
	if active != nil {
		return active.Close()
	}
	return nil
}

// ServeHTTP serves graphical setup routes or delegates to the existing web UI.
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.securityHeaders(w)
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/desktop/static/setup.css":
		a.staticHandler.ServeHTTP(w, r)
		return
	case r.Method == http.MethodGet && r.URL.Path == "/desktop/static/setup.js":
		a.staticHandler.ServeHTTP(w, r)
		return
	case r.Method == http.MethodGet && r.URL.Path == "/desktop/settings":
		a.actions.Lock()
		defer a.actions.Unlock()
		a.renderSetup(w, http.StatusOK, "", "")
		return
	case r.Method == http.MethodPost && r.URL.Path == "/desktop/qobuz":
		a.connectQobuz(w, r)
		return
	case r.Method == http.MethodPost && r.URL.Path == "/desktop/qobuz/disconnect":
		a.disconnectQobuz(w, r)
		return
	case r.Method == http.MethodPost && r.URL.Path == "/desktop/openai":
		a.saveOpenAI(w, r)
		return
	case r.Method == http.MethodPost && r.URL.Path == "/desktop/preferences":
		a.savePreferences(w, r)
		return
	}
	a.mu.RLock()
	handler := a.appHandler
	if handler == nil {
		a.mu.RUnlock()
		if r.Method != http.MethodGet || r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		a.actions.Lock()
		defer a.actions.Unlock()
		a.renderSetup(w, http.StatusOK, "", "")
		return
	}
	clone := r.Clone(r.Context())
	clone.Host = "wails.localhost"
	handler.ServeHTTP(w, clone)
	a.mu.RUnlock()
}

func (a *App) connectQobuz(w http.ResponseWriter, r *http.Request) {
	a.actions.Lock()
	defer a.actions.Unlock()
	if !a.validForm(w, r) {
		return
	}
	if a.vaultError != "" {
		a.renderSetup(w, http.StatusServiceUnavailable, a.vaultError, "")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	appID, err := a.auth.DiscoverAppID(ctx)
	if err != nil {
		a.renderSetup(w, http.StatusBadGateway, fmt.Sprintf("Discover the Qobuz application ID: %v", err), "")
		return
	}
	code, _, err := a.auth.ReceiveCode(ctx, appID, 0)
	if err != nil {
		a.renderSetup(w, http.StatusBadGateway, err.Error(), "")
		return
	}
	creds, err := a.auth.Exchange(ctx, appID, code, qobuzauth.DefaultPrivateKey)
	if err != nil {
		a.renderSetup(w, http.StatusBadGateway, fmt.Sprintf("Complete Qobuz authorization: %v", err), "")
		return
	}
	if err = a.vault.Set(credentials.QobuzToken, creds.Token); err != nil {
		a.renderSetup(w, http.StatusServiceUnavailable, err.Error(), "")
		return
	}
	a.config.QobuzAppID, a.config.Provider = creds.AppID, "qobuz"
	if err = a.saveConfigAndReload(); err != nil {
		a.renderSetup(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	a.renderSetup(w, http.StatusOK, "", "Connected to Qobuz as "+creds.DisplayName+".")
}

func (a *App) disconnectQobuz(w http.ResponseWriter, r *http.Request) {
	a.actions.Lock()
	defer a.actions.Unlock()
	if !a.validForm(w, r) {
		return
	}
	if err := a.vault.Delete(credentials.QobuzToken); err != nil {
		a.renderSetup(w, http.StatusServiceUnavailable, err.Error(), "")
		return
	}
	a.config.Provider, a.config.QobuzAppID = "fake", ""
	if err := a.saveConfigAndReload(); err != nil {
		a.renderSetup(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	a.renderSetup(w, http.StatusOK, "", "Qobuz has been disconnected.")
}

func (a *App) saveOpenAI(w http.ResponseWriter, r *http.Request) {
	a.actions.Lock()
	defer a.actions.Unlock()
	if !a.validForm(w, r) {
		return
	}
	var err error
	key := strings.TrimSpace(r.FormValue("openai_api_key"))
	if r.FormValue("remove") == "true" {
		err = a.vault.Delete(credentials.OpenAIAPIKey)
	} else if key != "" {
		err = a.vault.Set(credentials.OpenAIAPIKey, key)
	}
	if err != nil {
		a.renderSetup(w, http.StatusServiceUnavailable, err.Error(), "")
		return
	}
	if err = a.reload(); err != nil {
		a.renderSetup(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	a.renderSetup(w, http.StatusOK, "", "OpenAI settings saved.")
}

func (a *App) savePreferences(w http.ResponseWriter, r *http.Request) {
	a.actions.Lock()
	defer a.actions.Unlock()
	if !a.validForm(w, r) {
		return
	}
	threshold, err := strconv.ParseFloat(r.FormValue("match_threshold"), 64)
	if err != nil {
		a.renderSetup(w, http.StatusUnprocessableEntity, "Match confidence must be a number between zero and one.", "")
		return
	}
	a.config.OpenAIModel = strings.TrimSpace(r.FormValue("openai_model"))
	a.config.MatchThreshold = threshold
	a.config.LogLevel = strings.ToLower(strings.TrimSpace(r.FormValue("log_level")))
	if err = a.saveConfigAndReload(); err != nil {
		a.renderSetup(w, http.StatusUnprocessableEntity, err.Error(), "")
		return
	}
	a.renderSetup(w, http.StatusOK, "", "Preferences saved.")
}

func (a *App) saveConfigAndReload() error {
	a.config.QobuzUserToken, a.config.OpenAIAPIKey = "", ""
	if err := configfile.Save(a.configPath, a.config); err != nil {
		return err
	}
	return a.reload()
}

func (a *App) validForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form submission", http.StatusUnprocessableEntity)
		return false
	}
	want, got := []byte(a.csrf), []byte(r.FormValue("csrf"))
	if len(want) != len(got) || subtle.ConstantTimeCompare(want, got) != 1 {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return false
	}
	return true
}

func (a *App) renderSetup(w http.ResponseWriter, status int, message, success string) {
	cfg, err := a.effectiveConfig()
	if err != nil && a.vaultError == "" {
		a.vaultError = err.Error()
	}
	page := setupPage{
		CSRF: a.csrf, Error: message, Success: success, VaultError: a.vaultError,
		VaultName: vaultName(), ConfigPath: a.configPath, DataDir: a.config.DataDir,
		OpenAIModel: a.config.OpenAIModel, MatchThreshold: strconv.FormatFloat(a.config.MatchThreshold, 'f', 2, 64), LogLevel: a.config.LogLevel,
		QobuzConnected: cfg.QobuzAppID != "" && cfg.QobuzUserToken != "", OpenAIConfigured: cfg.OpenAIAPIKey != "",
	}
	a.mu.RLock()
	page.Ready = a.runtime != nil
	a.mu.RUnlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if executeErr := a.templates.ExecuteTemplate(w, "setup", page); executeErr != nil {
		http.Error(w, "render desktop setup", http.StatusInternalServerError)
	}
}

func (a *App) securityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; script-src 'self'; img-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Permissions-Policy", "camera=(), geolocation=(), microphone=()")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Cache-Control", "no-store")
}

func vaultName() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS Keychain"
	case "windows":
		return "Windows Credential Manager"
	default:
		return "the Linux Secret Service"
	}
}
