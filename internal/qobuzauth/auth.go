// Package qobuzauth implements the loopback browser flow for Qobuz credentials.
package qobuzauth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/pawel/qobuz-curator/internal/browser"
	"github.com/pawel/qobuz-curator/internal/httpretry"
	"go.uber.org/zap"
)

const WebBase = "https://play.qobuz.com"
const APIBase = "https://www.qobuz.com/api.json/0.2"
const OAuthURL = "https://www.qobuz.com/signin/oauth"
const DefaultPrivateKey = "6lz8C03UDIC7"

// Credentials contains the two values needed by the Qobuz provider plus a
// display-only account name.
type Credentials struct {
	AppID       string `json:"qobuz_app_id"`
	Token       string `json:"qobuz_user_auth_token"`
	DisplayName string `json:"display_name"`
}

// Client implements the browser-based Qobuz web authorization flow.
type Client struct {
	HTTP                       *http.Client
	WebBase, APIBase, OAuthURL string
	OpenBrowser                func(string) error
}

var listenLoopback = net.Listen
var readAuthRandom = rand.Read

// New returns a Qobuz authorization client with production endpoints.
func New() *Client {
	return &Client{HTTP: &http.Client{Timeout: 30 * time.Second}, WebBase: WebBase, APIBase: APIBase, OAuthURL: OAuthURL, OpenBrowser: browser.Open}
}
func (c *Client) get(ctx context.Context, address string, headers map[string]string) ([]byte, error) {
	return c.getWithRetry(ctx, address, headers, true)
}

func (c *Client) getWithRetry(ctx context.Context, address string, headers map[string]string, retryable bool) ([]byte, error) {
	makeRequest := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "QobuzCurator-CredentialHelper/1.0")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		return req, nil
	}
	resp, err := httpretry.Do(ctx, c.HTTP, makeRequest, retryable, zap.L(), "Qobuz authorization", httpretry.DefaultPolicy())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := readBounded(resp.Body, 8<<20)
	if err != nil {
		return nil, fmt.Errorf("read Qobuz response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("qobuz returned HTTP %d", resp.StatusCode)
	}
	return raw, nil
}

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return raw, nil
}
func (c *Client) DiscoverAppID(ctx context.Context) (string, error) {
	zap.L().Info("discovering the Qobuz application ID")
	html, e := c.get(ctx, c.WebBase+"/login", nil)
	if e != nil {
		return "", e
	}
	patterns := []*regexp.Regexp{regexp.MustCompile(`["']([^"']*?/resources/[^"']*?bundle\.js)["']`), regexp.MustCompile(`["']([^"']*?bundle[^"']*?\.js)["']`)}
	var path string
	for _, p := range patterns {
		m := p.FindAllSubmatch(html, -1)
		if len(m) > 0 {
			path = string(m[len(m)-1][1])
			break
		}
	}
	if path == "" {
		return "", fmt.Errorf("could not find Qobuz web-player bundle")
	}
	base, _ := url.Parse(c.WebBase + "/")
	relative, _ := url.Parse(path)
	bundle, e := c.get(ctx, base.ResolveReference(relative).String(), nil)
	if e != nil {
		return "", e
	}
	for _, p := range []*regexp.Regexp{regexp.MustCompile(`production\s*:\s*\{\s*api\s*:\s*\{\s*appId\s*:\s*["'](\d{6,12})`), regexp.MustCompile(`["']production["']\s*:\s*\{.*?["']appId["']\s*:\s*["'](\d{6,12})`)} {
		if m := p.FindSubmatch(bundle); len(m) > 1 {
			zap.L().Info("discovered the Qobuz application ID")
			return string(m[1]), nil
		}
	}
	return "", fmt.Errorf("could not extract Qobuz app ID")
}

// ParseCode accepts either a raw code or a callback URL.
func ParseCode(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("authorization callback was empty")
	}
	if !strings.Contains(value, "://") {
		return value, nil
	}
	u, e := url.Parse(value)
	if e != nil {
		return "", e
	}
	for _, key := range []string{"code_autorisation", "code", "authorization_code"} {
		if code := u.Query().Get(key); code != "" {
			return code, nil
		}
	}
	if failure := u.Query().Get("error"); failure != "" {
		return "", fmt.Errorf("qobuz authorization failed: %s", failure)
	}
	return "", fmt.Errorf("callback URL did not contain an authorization code")
}
func (c *Client) ReceiveCode(ctx context.Context, appID string, port int) (string, string, error) {
	listener, e := listenLoopback("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if e != nil {
		return "", "", e
	}
	actual := listener.Addr().(*net.TCPAddr).Port
	zap.L().Debug("Qobuz callback listener started", zap.Int("port", actual))
	stateBytes := make([]byte, 32)
	if _, err := readAuthRandom(stateBytes); err != nil {
		_ = listener.Close()
		return "", "", fmt.Errorf("generate authorization state: %w", err)
	}
	state := base64.RawURLEncoding.EncodeToString(stateBytes)
	callbackPath := "/callback/" + state
	redirect := fmt.Sprintf("http://127.0.0.1:%d%s", actual, callbackPath)
	login := c.OAuthURL + "?" + url.Values{"ext_app_id": {appID}, "redirect_url": {redirect}}.Encode()
	result := make(chan string, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != callbackPath {
			http.NotFound(w, r)
			return
		}
		select {
		case result <- fmt.Sprintf("http://127.0.0.1:%d%s", actual, r.URL.RequestURI()):
		default:
		}
		_, _ = w.Write([]byte("Qobuz authorization received. You can close this tab."))
	}), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 15 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	if err := c.OpenBrowser(login); err != nil {
		_ = listener.Close()
		return "", login, fmt.Errorf("open Qobuz authorization page: %w", err)
	}
	shutdown := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}
	select {
	case value := <-result:
		shutdown()
		zap.L().Info("received the Qobuz authorization callback")
		code, e := ParseCode(value)
		return code, login, e
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			return "", login, fmt.Errorf("authorization callback server: %w", err)
		}
		return "", login, fmt.Errorf("authorization callback server stopped")
	case <-ctx.Done():
		shutdown()
		return "", login, fmt.Errorf("timed out waiting for Qobuz authorization")
	}
}
func (c *Client) Exchange(ctx context.Context, appID, code, privateKey string) (Credentials, error) {
	address := c.APIBase + "/oauth/callback?" + url.Values{"code": {code}, "private_key": {privateKey}}.Encode()
	// An authorization code can be single-use, so its exchange is deliberately
	// never retried after an ambiguous network failure.
	raw, e := c.getWithRetry(ctx, address, map[string]string{"X-App-Id": appID}, false)
	if e != nil {
		return Credentials{}, e
	}
	var tokenData map[string]any
	if e = json.Unmarshal(raw, &tokenData); e != nil {
		return Credentials{}, e
	}
	token, _ := tokenData["token"].(string)
	if token == "" {
		token, _ = tokenData["user_auth_token"].(string)
	}
	if token == "" {
		return Credentials{}, fmt.Errorf("qobuz did not return a token")
	}
	raw, e = c.get(ctx, c.APIBase+"/user/get", map[string]string{"X-App-Id": appID, "X-User-Auth-Token": token})
	if e != nil {
		return Credentials{}, e
	}
	var profile map[string]any
	if e = json.Unmarshal(raw, &profile); e != nil {
		return Credentials{}, fmt.Errorf("decode Qobuz profile: %w", e)
	}
	name := fmt.Sprint(profile["display_name"])
	if name == "<nil>" {
		name = fmt.Sprint(profile["firstname"])
	}
	zap.L().Info("Qobuz authentication completed", zap.Bool("profile_verified", true))
	return Credentials{AppID: appID, Token: token, DisplayName: name}, nil
}
