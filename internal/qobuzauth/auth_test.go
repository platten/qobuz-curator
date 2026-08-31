package qobuzauth

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestParseCode(t *testing.T) {
	for input, want := range map[string]string{"plain": "plain", "http://x/callback?code=abc": "abc", "http://x/?code_autorisation=def": "def"} {
		got, e := ParseCode(input)
		if e != nil || got != want {
			t.Fatal(input, got, e)
		}
	}
	for _, input := range []string{"", "http://x/", "http://x/?error=denied"} {
		if _, e := ParseCode(input); e == nil {
			t.Fatal(input)
		}
	}
}
func TestDiscoverExchangeAndReceive(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<script src="/resources/app.bundle.js"></script>`))
	})
	mux.HandleFunc("/resources/app.bundle.js", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`production:{api:{appId:"123456789"`)) })
	mux.HandleFunc("/oauth/callback", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"token":"tok"}`)) })
	mux.HandleFunc("/user/get", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"display_name":"Listener"}`)) })
	server := httptest.NewServer(mux)
	defer server.Close()
	c := &Client{HTTP: server.Client(), WebBase: server.URL, APIBase: server.URL, OAuthURL: server.URL + "/authorize"}
	id, e := c.DiscoverAppID(context.Background())
	if e != nil || id != "123456789" {
		t.Fatal(id, e)
	}
	c.OpenBrowser = func(address string) error {
		u, _ := url.Parse(address)
		redirect := u.Query().Get("redirect_url")
		go func() {
			time.Sleep(10 * time.Millisecond)
			callback, _ := url.Parse(redirect)
			_, _ = http.Get(callback.Scheme + "://" + callback.Host + "/wrong")
			_, _ = http.Get(redirect + "?code=code123")
		}()
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	code, login, e := c.ReceiveCode(ctx, id, 0)
	if e != nil || code != "code123" || !strings.Contains(login, "ext_app_id") {
		t.Fatal(code, login, e)
	}
	creds, e := c.Exchange(context.Background(), id, code, "key")
	if e != nil || creds.Token != "tok" || creds.DisplayName != "Listener" {
		t.Fatal(creds, e)
	}
}
func TestAuthErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			w.Write([]byte("no bundle"))
			return
		}
		http.Error(w, "bad", 500)
	}))
	defer server.Close()
	c := &Client{HTTP: server.Client(), WebBase: server.URL, APIBase: server.URL, OAuthURL: server.URL, OpenBrowser: func(string) error { return nil }}
	if _, e := c.DiscoverAppID(context.Background()); e == nil {
		t.Fatal("discover")
	}
	if _, e := c.Exchange(context.Background(), "a", "c", "k"); e == nil {
		t.Fatal("exchange")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, e := c.ReceiveCode(ctx, "a", 0); e == nil {
		t.Fatal("timeout")
	}
	c.OpenBrowser = func(string) error { return errors.New("browser failed") }
	if _, _, e := c.ReceiveCode(context.Background(), "a", 0); e == nil || !strings.Contains(e.Error(), "browser failed") {
		t.Fatal(e)
	}
}

func TestNewAndResponseBounds(t *testing.T) {
	if c := New(); c.HTTP == nil || c.OpenBrowser == nil || c.APIBase == "" {
		t.Fatal(c)
	}
	if _, err := readBounded(errorReader{}, 10); err == nil {
		t.Fatal("read error")
	}
	if _, err := readBounded(strings.NewReader(strings.Repeat("x", 11)), 10); err == nil {
		t.Fatal("oversize")
	}
	c := &Client{HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network")
	})}}
	if _, err := c.get(context.Background(), "http://example.test", nil); err == nil {
		t.Fatal("network error")
	}
	if _, err := c.get(context.Background(), ":", nil); err == nil {
		t.Fatal("request error")
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestExchangeVariants(t *testing.T) {
	profileInvalid := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/callback":
			code := r.URL.Query().Get("code")
			switch code {
			case "fallback":
				_, _ = w.Write([]byte(`{"user_auth_token":"fallback-token"}`))
			case "missing":
				_, _ = w.Write([]byte(`{}`))
			case "bad":
				_, _ = w.Write([]byte(`{`))
			default:
				_, _ = w.Write([]byte(`{"token":"token"}`))
			}
		case "/user/get":
			if profileInvalid {
				_, _ = w.Write([]byte(`{`))
			} else {
				_, _ = w.Write([]byte(`{"firstname":"First"}`))
			}
		}
	}))
	defer server.Close()
	c := &Client{HTTP: server.Client(), APIBase: server.URL}
	creds, err := c.Exchange(context.Background(), "app", "fallback", "key")
	if err != nil || creds.Token != "fallback-token" || creds.DisplayName != "First" {
		t.Fatal(creds, err)
	}
	if _, err = c.Exchange(context.Background(), "app", "missing", "key"); err == nil {
		t.Fatal("missing token")
	}
	if _, err = c.Exchange(context.Background(), "app", "bad", "key"); err == nil {
		t.Fatal("bad token response")
	}
	profileInvalid = true
	if _, err = c.Exchange(context.Background(), "app", "ok", "key"); err == nil {
		t.Fatal("bad profile")
	}
	if _, err = ParseCode("http://[::1"); err == nil {
		t.Fatal("bad callback URL")
	}
}

func TestReceiveCodeSetupFailures(t *testing.T) {
	originalListen, originalRandom := listenLoopback, readAuthRandom
	defer func() { listenLoopback, readAuthRandom = originalListen, originalRandom }()
	listenLoopback = func(string, string) (net.Listener, error) { return nil, errors.New("listen") }
	c := &Client{OpenBrowser: func(string) error { return nil }}
	if _, _, err := c.ReceiveCode(context.Background(), "app", 0); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatal(err)
	}
	listenLoopback = net.Listen
	readAuthRandom = func([]byte) (int, error) { return 0, errors.New("entropy") }
	if _, _, err := c.ReceiveCode(context.Background(), "app", 0); err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatal(err)
	}
}
