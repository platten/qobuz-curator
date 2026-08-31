package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fatih/color"
	"github.com/pawel/qobuz-curator/internal/config"
	"github.com/pawel/qobuz-curator/internal/qobuzauth"
)

type synchronizedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *synchronizedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(value)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func configFile(t *testing.T, extra string) string {
	t.Helper()
	// Configuration precedence intentionally puts environment variables above
	// files. Clear ambient application settings so a developer's real provider
	// credentials can neither change these tests nor appear in a failure dump.
	for _, setting := range os.Environ() {
		key, _, _ := strings.Cut(setting, "=")
		if strings.HasPrefix(key, "QOBUZ_CURATOR_") {
			t.Setenv(key, "")
		}
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	providerLine := "provider: fake\n"
	if strings.Contains(extra, "provider:") {
		providerLine = ""
	}
	content := providerLine + "auth_disabled: true\nsession_secret: \"12345678901234567890123456789012\"\ndata_dir: \"" + strings.ReplaceAll(t.TempDir(), "\\", "/") + "\"\n" + extra
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAuthWriteAndHealthcheck(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/callback", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"token":"token"}`)) })
	mux.HandleFunc("/user/get", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"display_name":"Listener"}`)) })
	api := httptest.NewServer(mux)
	defer api.Close()
	old := newAuthClient
	defer func() { newAuthClient = old }()
	newAuthClient = func() *qobuzauth.Client {
		client := &qobuzauth.Client{HTTP: api.Client(), APIBase: api.URL, OAuthURL: api.URL + "/authorize"}
		client.OpenBrowser = func(address string) error {
			parsed, _ := url.Parse(address)
			callback := parsed.Query().Get("redirect_url")
			go func() {
				time.Sleep(10 * time.Millisecond)
				_, _ = http.Get(callback + "?code=abc")
			}()
			return nil
		}
		return client
	}
	path := configFile(t, "")
	cmd := NewRoot("x")
	var out synchronizedBuffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"auth", "--app-id", "123456", "--callback-port", "0", "--json", "--write-config", "--config", path})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "token") {
		t.Fatal(out.String())
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "qobuz_app_id") {
		t.Fatal(string(raw))
	}
	yamlCommand := NewRoot("x")
	var yamlOutput bytes.Buffer
	yamlCommand.SetOut(&yamlOutput)
	yamlCommand.SetErr(io.Discard)
	yamlCommand.SetArgs([]string{"auth", "--app-id", "123456", "--callback-port", "0", "--config", path})
	if err := yamlCommand.Execute(); err != nil || !strings.Contains(yamlOutput.String(), "qobuz_user_auth_token") {
		t.Fatal(yamlOutput.String(), err)
	}
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer health.Close()
	u, _ := url.Parse(health.URL)
	port, _ := net.LookupPort("tcp", u.Port())
	_ = port
	_, portText, _ := net.SplitHostPort(u.Host)
	healthPath := configFile(t, "port: "+portText+"\n")
	healthCmd := NewRoot("x")
	healthCmd.SetArgs([]string{"healthcheck", "--config", healthPath})
	if err := healthCmd.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestVersionAndAuthID(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"auth", "--app-id", "123456", "--app-id-only", "--config", configFile(t, "")}} {
		cmd := NewRoot("1.2.3")
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatal(args, err)
		}
		want := "1.2.3"
		if args[0] == "auth" {
			want = "123456"
		}
		if !strings.Contains(out.String(), want) {
			t.Fatal(out.String())
		}
	}
}

func TestServeAndValidation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := NewRoot("test")
	cmd.SetContext(ctx)
	var out synchronizedBuffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"serve", "--config", configFile(t, fmt.Sprintf("port: %d\n", port)), "--no-browser"})
	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(out.String(), "listening") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	bad := NewRoot("x")
	bad.SetArgs([]string{"serve", "--config", configFile(t, "provider: qobuz\nport: 8100\n")})
	if err := bad.Execute(); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatal(err)
	}
}

func TestPasswordCommand(t *testing.T) {
	original := readTerminalPassword
	defer func() { readTerminalPassword = original }()
	run := func(values [][]byte, terminalErr error) error {
		index := 0
		readTerminalPassword = func(int) ([]byte, error) {
			if terminalErr != nil {
				return nil, terminalErr
			}
			value := values[index]
			index++
			return value, nil
		}
		cmd := passwordCommand()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		return cmd.Execute()
	}
	if err := run([][]byte{[]byte("secret"), []byte("secret")}, nil); err != nil {
		t.Fatal(err)
	}
	if err := run([][]byte{[]byte("secret"), []byte("different")}, nil); err == nil {
		t.Fatal("mismatch")
	}
	if err := run(nil, errors.New("terminal")); err == nil {
		t.Fatal("terminal error")
	}
	call := 0
	readTerminalPassword = func(int) ([]byte, error) {
		call++
		if call == 2 {
			return nil, errors.New("confirm")
		}
		return []byte("secret"), nil
	}
	cmd := passwordCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil {
		t.Fatal("confirm read error")
	}
}

func TestExecuteAndWriteCredentialErrors(t *testing.T) {
	originalArgs := os.Args
	defer func() { os.Args = originalArgs }()
	os.Args = []string{"qobuz-curator", "version"}
	if err := Execute("test"); err != nil {
		t.Fatal(err)
	}
	if err := ExecuteContext(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	malformed := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(malformed, []byte("["), 0600); err != nil {
		t.Fatal(err)
	}
	creds := qobuzauth.Credentials{AppID: "app", Token: "token"}
	if err := writeCredentials(malformed, creds); err == nil {
		t.Fatal("malformed YAML")
	}
	parent := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(parent, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeCredentials(filepath.Join(parent, "config.yaml"), creds); err == nil {
		t.Fatal("invalid parent")
	}
	target := filepath.Join(t.TempDir(), "new", "config.yaml")
	if err := writeCredentials(target, creds); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(target)
	if !strings.Contains(string(raw), "token") {
		t.Fatal(string(raw))
	}
}

func TestHealthAndCommandErrors(t *testing.T) {
	statusServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", http.StatusServiceUnavailable) }))
	u, _ := url.Parse(statusServer.URL)
	_, port, _ := net.SplitHostPort(u.Host)
	cmd := NewRoot("x")
	cmd.SetArgs([]string{"healthcheck", "--config", configFile(t, "port: "+port+"\n")})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatal(err)
	}
	statusServer.Close()
	cmd = NewRoot("x")
	cmd.SetArgs([]string{"healthcheck", "--config", configFile(t, "port: "+port+"\n")})
	if err := cmd.Execute(); err == nil {
		t.Fatal("closed health endpoint")
	}
	cmd = NewRoot("x")
	cmd.SetArgs([]string{"serve", "--config", filepath.Join(t.TempDir(), "missing.yaml")})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "qobuz-curator init --interactive") {
		t.Fatalf("missing config guidance: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	occupiedPort := listener.Addr().(*net.TCPAddr).Port
	cmd = NewRoot("x")
	cmd.SetArgs([]string{"serve", "--no-browser", "--config", configFile(t, fmt.Sprintf("port: %d\n", occupiedPort))})
	if err := cmd.Execute(); err == nil {
		t.Fatal("occupied listener")
	}
	dataParent := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(dataParent, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	badDataConfig := filepath.Join(t.TempDir(), "bad-data.yaml")
	content := "provider: fake\nauth_disabled: true\ndata_dir: \"" + strings.ReplaceAll(filepath.Join(dataParent, "child"), "\\", "/") + "\"\n"
	if err := os.WriteFile(badDataConfig, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cmd = NewRoot("x")
	cmd.SetArgs([]string{"serve", "--config", badDataConfig})
	if err := cmd.Execute(); err == nil {
		t.Fatal("invalid data directory")
	}
}

func TestAuthDiscovery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			_, _ = w.Write([]byte(`<script src="/resources/app.bundle.js"></script>`))
			return
		}
		_, _ = w.Write([]byte(`production:{api:{appId:"987654321"`))
	}))
	defer server.Close()
	original := newAuthClient
	defer func() { newAuthClient = original }()
	newAuthClient = func() *qobuzauth.Client {
		return &qobuzauth.Client{HTTP: server.Client(), WebBase: server.URL}
	}
	cmd := NewRoot("x")
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"auth", "--app-id-only", "--config", configFile(t, "")})
	if err := cmd.Execute(); err != nil || !strings.Contains(output.String(), "987654321") {
		t.Fatal(output.String(), err)
	}
}

func TestServeBrowserWarning(t *testing.T) {
	original := openBrowser
	browserCalled := make(chan struct{}, 1)
	openBrowser = func(string) error {
		browserCalled <- struct{}{}
		return errors.New("browser unavailable")
	}
	defer func() { openBrowser = original }()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := NewRoot("x")
	cmd.SetContext(ctx)
	var output synchronizedBuffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"serve", "--config", configFile(t, fmt.Sprintf("port: %d\n", port))})
	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()
	select {
	case <-browserCalled:
	case <-time.After(3 * time.Second):
		t.Fatal("browser opener was not called")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(output.String(), err)
	}
}

func TestAutomaticPortAndUtilityCommands(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := NewRoot("x")
	cmd.SetContext(ctx)
	var output synchronizedBuffer
	cmd.SetOut(&output)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"serve", "--no-browser", "--config", configFile(t, "port: 0\n")})
	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(output.String(), "http://127.0.0.1:") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(output.String(), "http://127.0.0.1:") {
		t.Fatal(output.String())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	health := NewRoot("x")
	health.SetArgs([]string{"healthcheck", "--config", configFile(t, "port: 0\n")})
	if err := health.Execute(); err == nil || !strings.Contains(err.Error(), "fixed") {
		t.Fatal(err)
	}

	pathCommand := NewRoot("x")
	var pathOutput bytes.Buffer
	pathCommand.SetOut(&pathOutput)
	pathCommand.SetArgs([]string{"config-path"})
	if err := pathCommand.Execute(); err != nil || !strings.Contains(pathOutput.String(), "qobuz-curator.yaml") {
		t.Fatal(pathOutput.String(), err)
	}

	tooManyArgs := NewRoot("x")
	tooManyArgs.SetArgs([]string{"version", "extra"})
	if err := tooManyArgs.Execute(); err == nil {
		t.Fatal("unexpected positional argument accepted")
	}
}

func TestColorLoggingAndSpinnerHelpers(t *testing.T) {
	if value, err := useColor("always", io.Discard); err != nil || !value {
		t.Fatal(value, err)
	}
	if value, err := useColor("never", os.Stdout); err != nil || value {
		t.Fatal(value, err)
	}
	if _, err := useColor("sometimes", os.Stdout); err == nil {
		t.Fatal("invalid color mode accepted")
	}
	t.Setenv("NO_COLOR", "1")
	if value, err := useColor("auto", os.Stdout); err != nil || value {
		t.Fatal(value, err)
	}
	if output := paint(true, color.FgGreen)("ready"); !strings.Contains(output, "\x1b[") {
		t.Fatal(output)
	}
	if got := configSource("explicit.yaml"); got != "explicit.yaml" {
		t.Fatal(got)
	}
	if got := configSource(""); got != config.DefaultPath() {
		t.Fatal(got)
	}

	originalTerminal := isTerminal
	isTerminal = func(int) bool { return true }
	t.Cleanup(func() { isTerminal = originalTerminal })
	file, err := os.CreateTemp(t.TempDir(), "spinner-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	stop := startSpinner(file, "Waiting", false)
	time.Sleep(20 * time.Millisecond)
	stop(true)
	stop = startSpinner(file, "Failing", true)
	time.Sleep(20 * time.Millisecond)
	stop(false)
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	if cleanup, err := configureLogger(config.Config{LogLevel: "verbose", LogFormat: "console"}); err == nil {
		cleanup()
		t.Fatal("invalid logging configuration accepted")
	}
}
