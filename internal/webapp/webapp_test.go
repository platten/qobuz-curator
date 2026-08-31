package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pawel/qobuz-curator/internal/config"
	"github.com/pawel/qobuz-curator/internal/model"
	"github.com/pawel/qobuz-curator/internal/provider"
	"github.com/pawel/qobuz-curator/internal/security"
	"github.com/pawel/qobuz-curator/internal/service"
	"github.com/pawel/qobuz-curator/internal/store"
)

type fakeRecommender struct{ err error }

type failingProvider struct{ provider.Provider }

func (f failingProvider) ListPlaylists(context.Context) ([]model.PlaylistSummary, error) {
	return nil, errors.New("provider private detail")
}

func recommendation() model.PlaylistInput {
	return model.PlaylistInput{SchemaVersion: "1.0", Name: "Generated", Tracks: []model.TrackRequest{{Position: 1, Title: "So What", Artists: []string{"Miles Davis"}}}}
}
func (f fakeRecommender) Recommend(context.Context, string) (model.PlaylistInput, error) {
	return recommendation(), f.err
}
func (f fakeRecommender) Refine(context.Context, model.PlaylistInput, string) (model.PlaylistInput, error) {
	p := recommendation()
	p.Name = "Refined"
	return p, f.err
}
func (f fakeRecommender) FromPlaylists(context.Context, []model.ProviderPlaylist, int, string) (model.PlaylistInput, error) {
	return recommendation(), f.err
}
func newTestApp(t *testing.T, auth bool) (*httptest.Server, *provider.Fake) {
	t.Helper()
	cfg := config.Config{DataDir: t.TempDir(), DatabaseName: "db", Provider: "fake", MatchThreshold: .5, PreviewTTLHours: 24, SessionTTLHours: 24, SessionSecret: strings.Repeat("x", 32), AuthDisabled: !auth, OpenAIAPIKey: "key"}
	if auth {
		cfg.PasswordHash, _ = security.HashPassword("secret")
	}
	db, e := store.Open(filepath.Join(cfg.DataDir, "db"))
	if e != nil {
		t.Fatal(e)
	}
	p := provider.NewFake()
	svc := &service.Service{Config: cfg, Store: db, Provider: p}
	app, e := New(cfg, svc, p, fakeRecommender{}, db)
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(app.Handler())
	t.Cleanup(func() { server.Close(); db.Close() })
	return server, p
}

var csrfRE = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func getPage(t *testing.T, c *http.Client, address string) (string, *http.Cookie, string) {
	t.Helper()
	resp, e := c.Get(address)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	match := csrfRE.FindSubmatch(raw)
	token := ""
	if len(match) > 1 {
		token = string(match[1])
	}
	var cookie *http.Cookie
	if len(resp.Cookies()) > 0 {
		cookie = resp.Cookies()[0]
	}
	return string(raw), cookie, token
}
func post(t *testing.T, c *http.Client, address string, values url.Values, cookie *http.Cookie) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", address, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, e := c.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(raw)
}
func TestWebWorkflow(t *testing.T) {
	server, p := newTestApp(t, false)
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	body, cookie, token := getPage(t, client, server.URL+"/")
	if !strings.Contains(body, "Recommend from your Qobuz playlists") {
		t.Fatal(body)
	}
	if response, err := client.Get(server.URL + "/healthz"); err != nil || response.Header.Get("Content-Security-Policy") == "" || response.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal(response, err)
	}
	if resp, e := client.Get(server.URL + "/healthz"); e != nil || resp.StatusCode != 200 {
		t.Fatal(e)
	}
	req, _ := http.NewRequest("GET", server.URL+"/playlist-v1.schema.json", nil)
	req.AddCookie(cookie)
	resp, e := client.Do(req)
	if e != nil || resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatal(resp, e)
	}
	bad, _ := post(t, client, server.URL+"/recommend", url.Values{"csrf": {"bad"}}, cookie)
	if bad.StatusCode != 403 {
		t.Fatal(bad.StatusCode)
	}
	raw, _ := json.Marshal(recommendation())
	created, _ := post(t, client, server.URL+"/previews", url.Values{"csrf": {token}, "playlist_json": {string(raw)}}, cookie)
	if created.StatusCode != 303 {
		t.Fatal(created.StatusCode)
	}
	location := created.Header.Get("Location")
	previewBody, _, previewToken := getPage(t, clientWithCookie(client, cookie), server.URL+location)
	if !strings.Contains(previewBody, "So What") {
		t.Fatal(previewBody)
	}
	refined, _ := post(t, client, server.URL+location+"/refine", url.Values{"csrf": {previewToken}, "prompt": {"quieter"}}, cookie)
	if refined.StatusCode != 303 {
		t.Fatal(refined.StatusCode)
	}
	generated, _ := post(t, client, server.URL+"/recommend", url.Values{"csrf": {token}, "prompt": {"jazz"}}, cookie)
	if generated.StatusCode != 303 {
		t.Fatal(generated.StatusCode)
	}
	source, _ := p.CreatePlaylist(context.Background(), "Source", "")
	p.AddTracks(context.Background(), source.ID, []string{"demo-1"})
	seeded, _ := post(t, client, server.URL+"/recommend/from-playlists", url.Values{"csrf": {token}, "playlist_ids": {source.ID}, "track_count": {"10"}}, cookie)
	if seeded.StatusCode != 303 {
		t.Fatal(seeded.StatusCode)
	}
	published, _ := post(t, client, server.URL+location+"/publish", url.Values{"csrf": {previewToken}, "mode": {"create_new"}, "new_name": {"Web"}, "confirmed": {"true"}}, cookie)
	if published.StatusCode != 303 {
		t.Fatal(published.StatusCode)
	}
	operationBody, _, operationToken := getPage(t, clientWithCookie(client, cookie), server.URL+published.Header.Get("Location"))
	if !strings.Contains(operationBody, "succeeded") {
		t.Fatal(operationBody)
	}
	target, _ := p.CreatePlaylist(context.Background(), "Target", "")
	appended, _ := post(t, client, server.URL+location+"/publish", url.Values{"csrf": {previewToken}, "mode": {"append_existing"}, "append_playlist_id": {target.ID}, "confirmed": {"true"}}, cookie)
	if appended.StatusCode != 303 {
		t.Fatal(appended.StatusCode)
	}
	replaced, _ := post(t, client, server.URL+location+"/publish", url.Values{"csrf": {previewToken}, "mode": {"replace_existing"}, "playlist_id": {target.ID}, "confirmed": {"true"}}, cookie)
	opPath := replaced.Header.Get("Location")
	_, _, restoreToken := getPage(t, clientWithCookie(client, cookie), server.URL+opPath)
	restored, _ := post(t, client, server.URL+opPath+"/restore", url.Values{"csrf": {restoreToken}, "confirmed": {"true"}}, cookie)
	if restored.StatusCode != 303 {
		t.Fatal(restored.StatusCode, operationToken)
	}
}

func TestWebHandlerErrors(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir(), DatabaseName: "db", Provider: "fake", MatchThreshold: .5, PreviewTTLHours: 24, SessionTTLHours: 24, SessionSecret: strings.Repeat("z", 32), AuthDisabled: true, OpenAIAPIKey: "key"}
	db, err := store.Open(filepath.Join(cfg.DataDir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := provider.NewFake()
	svc := &service.Service{Config: cfg, Store: db, Provider: p}
	app, err := New(cfg, svc, p, fakeRecommender{err: errors.New("generation failed")}, db)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	_, cookie, token := getPage(t, client, server.URL+"/")
	for path, values := range map[string]url.Values{
		"/recommend":                {"csrf": {token}, "prompt": {"x"}},
		"/recommend/from-playlists": {"csrf": {token}, "playlist_ids": {"missing"}, "track_count": {"2"}},
	} {
		resp, body := post(t, client, server.URL+path, values, cookie)
		if resp.StatusCode != 422 || body == "" {
			t.Fatal(path, resp.StatusCode, body)
		}
	}
	raw, _ := json.Marshal(recommendation())
	created, _ := post(t, client, server.URL+"/previews", url.Values{"csrf": {token}, "playlist_json": {string(raw)}}, cookie)
	location := created.Header.Get("Location")
	_, _, previewToken := getPage(t, clientWithCookie(client, cookie), server.URL+location)
	blank, body := post(t, client, server.URL+location+"/refine", url.Values{"csrf": {previewToken}, "prompt": {" "}}, cookie)
	if blank.StatusCode != 422 || !strings.Contains(body, "describe how") {
		t.Fatal(blank.StatusCode, body)
	}
	failed, body := post(t, client, server.URL+location+"/refine", url.Values{"csrf": {previewToken}, "prompt": {"change"}}, cookie)
	if failed.StatusCode != 422 || !strings.Contains(body, "generation failed") {
		t.Fatal(failed.StatusCode, body)
	}
	for _, values := range []url.Values{
		{"csrf": {previewToken}, "mode": {"invalid"}, "confirmed": {"true"}},
		{"csrf": {previewToken}, "mode": {"create_new"}},
		{"csrf": {previewToken}, "mode": {"append_existing"}, "confirmed": {"true"}},
	} {
		resp, _ := post(t, client, server.URL+location+"/publish", values, cookie)
		if resp.StatusCode != 422 {
			t.Fatal(resp.StatusCode)
		}
	}
	missing, _ := clientWithCookie(client, cookie).Get(server.URL + "/operations/missing")
	if missing.StatusCode != 404 {
		t.Fatal(missing.StatusCode)
	}
	restore, _ := post(t, client, server.URL+"/operations/missing/restore", url.Values{"csrf": {token}, "confirmed": {"true"}}, cookie)
	if restore.StatusCode != 422 {
		t.Fatal(restore.StatusCode)
	}
	if pointerText(nil, "fallback") != "fallback" || pointerText((*string)(nil), "fallback") != "fallback" || pointerText("", "fallback") != "fallback" || pointerText("x", "fallback") != "x" {
		t.Fatal("pointer text")
	}
	value, integer, decimalValue := "value", 24, 96.0
	if joinText([]string{"a", "b"}) != "a, b" || matched(model.Preview{}) != 0 || skipped(model.Preview{}) != 0 || formatScore(.5) != "0.500" || deref(nil) != "" || deref(&value) != value || number(nil) != "?" || number(&integer) != "24" || decimal(nil) != "?" || decimal(&decimalValue) != "96" || !mutable("append_existing") || mutable("create_new") {
		t.Fatal("template helpers")
	}
}
func TestWebErrorsAndAuthentication(t *testing.T) {
	server, _ := newTestApp(t, true)
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, _ := client.Get(server.URL + "/")
	if resp.StatusCode != 303 {
		t.Fatal(resp.StatusCode)
	}
	_, cookie, token := getPage(t, client, server.URL+"/login")
	wrong, body := post(t, client, server.URL+"/login", url.Values{"csrf": {token}, "password": {"bad"}}, cookie)
	if wrong.StatusCode != 401 || !strings.Contains(body, "Incorrect") {
		t.Fatal(wrong.StatusCode, body)
	}
	login, _ := post(t, client, server.URL+"/login", url.Values{"csrf": {token}, "password": {"secret"}}, cookie)
	if login.StatusCode != 303 {
		t.Fatal(login.StatusCode)
	}
	authCookie := login.Cookies()[len(login.Cookies())-1]
	page, _, authToken := getPage(t, clientWithCookie(client, authCookie), server.URL+"/")
	if !strings.Contains(page, "Qobuz Curator") {
		t.Fatal(page)
	}
	invalid, _ := post(t, client, server.URL+"/previews", url.Values{"csrf": {authToken}, "playlist_json": {"{"}}, authCookie)
	if invalid.StatusCode != 422 {
		t.Fatal(invalid.StatusCode)
	}
	missing, _ := clientWithCookie(client, authCookie).Get(server.URL + "/previews/missing")
	if missing.StatusCode != 404 {
		t.Fatal(missing.StatusCode)
	}
	blank, _ := post(t, client, server.URL+"/recommend/from-playlists", url.Values{"csrf": {authToken}}, authCookie)
	if blank.StatusCode != 422 {
		t.Fatal(blank.StatusCode)
	}
	logout, _ := post(t, client, server.URL+"/logout", url.Values{"csrf": {authToken}}, authCookie)
	if logout.StatusCode != 303 {
		t.Fatal(logout.StatusCode)
	}
	replayed, _ := clientWithCookie(client, authCookie).Get(server.URL + "/")
	if replayed.StatusCode != http.StatusSeeOther {
		t.Fatal("logged-out cookie was replayable", replayed.StatusCode)
	}
}

func TestSecurityMiddlewareAndRateLimit(t *testing.T) {
	server, _ := newTestApp(t, true)
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	_, cookie, token := getPage(t, client, server.URL+"/login")
	for i := 0; i < 5; i++ {
		response, _ := post(t, client, server.URL+"/login", url.Values{"csrf": {token}, "password": {"wrong"}}, cookie)
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatal(i, response.StatusCode)
		}
	}
	blocked, _ := post(t, client, server.URL+"/login", url.Values{"csrf": {token}, "password": {"secret"}}, cookie)
	if blocked.StatusCode != http.StatusTooManyRequests {
		t.Fatal(blocked.StatusCode)
	}

	app := &App{Config: config.Config{Host: "127.0.0.1"}, logins: newLoginLimiter(1, time.Minute), active: map[string]int64{}}
	request := httptest.NewRequest(http.MethodGet, "http://evil.example/healthz", nil)
	request.Host = "evil.example"
	recorder := httptest.NewRecorder()
	app.validateHost(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("called") })).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMisdirectedRequest {
		t.Fatal(recorder.Code)
	}
	now := time.Now()
	app.logins.failure("client", now)
	if app.logins.allow("client", now) || !app.logins.allow("client", now.Add(2*time.Minute)) {
		t.Fatal("limiter window")
	}
	app.logins.reset("client")
	if !app.logins.allow("client", now) {
		t.Fatal("limiter reset")
	}
	for i := 0; i < maxLoginClients; i++ {
		app.logins.attempts[fmt.Sprintf("client-%d", i)] = loginAttempt{windowStart: now, blockedUntil: now.Add(time.Minute)}
	}
	if app.logins.allow("new-client", now) {
		t.Fatal("login limiter accepted an unbounded client set")
	}
	app.logins.attempts["client-0"] = loginAttempt{windowStart: now.Add(-2 * time.Minute)}
	if !app.logins.allow("new-client", now) {
		t.Fatal("login limiter did not prune an expired client")
	}
	app.passwords = make(chan struct{}, 1)
	app.passwords <- struct{}{}
	if matched, available := app.verifyPassword("anything"); matched || available {
		t.Fatal("concurrent password verification was not rejected")
	}
	<-app.passwords
	app.active = make(map[string]int64, maxActiveSessions)
	app.active["expired"] = now.Add(-time.Minute).Unix()
	for i := 1; i < maxActiveSessions; i++ {
		app.active[fmt.Sprintf("session-%d", i)] = now.Add(time.Duration(i) * time.Minute).Unix()
	}
	app.activateSession(security.Session{CSRF: "new", ExpiresAt: now.Add(time.Hour).Unix()})
	if _, exists := app.active["expired"]; exists || len(app.active) > maxActiveSessions {
		t.Fatal("active session registry was not bounded")
	}
	app.activateSession(security.Session{CSRF: "newer", ExpiresAt: now.Add(2 * time.Hour).Unix()})
	if _, exists := app.active["session-1"]; exists || len(app.active) != maxActiveSessions {
		t.Fatal("oldest active session was not evicted")
	}
	request.RemoteAddr = "not-a-host-port"
	if clientAddress(request) != "not-a-host-port" {
		t.Fatal(clientAddress(request))
	}
}

type webErrorReader struct{}

func (webErrorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestWebHelpersAndFailureBoundaries(t *testing.T) {
	if raw, err := readUpload(strings.NewReader("abc"), 3); err != nil || string(raw) != "abc" {
		t.Fatal(string(raw), err)
	}
	if _, err := readUpload(strings.NewReader("abcd"), 3); err == nil {
		t.Fatal("oversized upload")
	}
	if _, err := readUpload(webErrorReader{}, 3); err == nil {
		t.Fatal("upload read error")
	}
	app := &App{}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
	app.recoverPanic(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatal(recorder.Code)
	}
	recorder = httptest.NewRecorder()
	app.internalError(recorder, request, errors.New("private detail"))
	if recorder.Code != http.StatusInternalServerError || strings.Contains(recorder.Body.String(), "private") {
		t.Fatal(recorder.Body.String())
	}
	app.templates = template.New("empty")
	recorder = httptest.NewRecorder()
	app.render(recorder, http.StatusOK, "missing", page{})
	if recorder.Code != http.StatusInternalServerError {
		t.Fatal(recorder.Code)
	}
}

func TestMultipartUploadAndSourceLimit(t *testing.T) {
	server, _ := newTestApp(t, false)
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	_, cookie, token := getPage(t, client, server.URL+"/")
	raw, _ := json.Marshal(recommendation())
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("csrf", token)
	file, err := writer.CreateFormFile("playlist_file", "playlist.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write(raw)
	_ = writer.Close()
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/previews", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.AddCookie(cookie)
	response, err := client.Do(request)
	if err != nil || response.StatusCode != http.StatusSeeOther {
		t.Fatal(response, err)
	}
	_ = response.Body.Close()
	values := url.Values{"csrf": {token}, "track_count": {"1"}}
	for i := 0; i < 11; i++ {
		values.Add("playlist_ids", "id")
	}
	response, _ = post(t, client, server.URL+"/recommend/from-playlists", values, cookie)
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatal(response.StatusCode)
	}
}

func TestSessionsCSRFAndDashboardFailures(t *testing.T) {
	cfg := config.Config{
		Host: "127.0.0.1", SessionSecret: strings.Repeat("s", 32), SessionTTLHours: 1,
		SecureCookies: true, AuthDisabled: true,
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	fake := provider.NewFake()
	app := &App{Config: cfg, Provider: failingProvider{Provider: fake}, Store: db, templates: template.New("x"), active: map[string]int64{}, logins: newLoginLimiter(5, time.Minute)}
	session, err := security.NewSession(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	app.saveSession(recorder, session)
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "__Host-qobuz_curator" || !cookies[0].Secure {
		t.Fatal(cookies)
	}
	request := httptest.NewRequest(http.MethodGet, "https://localhost/", nil)
	request.AddCookie(cookies[0])
	if got, err := app.session(request); err != nil || got.CSRF != session.CSRF {
		t.Fatal(got, err)
	}
	badRequest := httptest.NewRequest(http.MethodPost, "https://localhost/", strings.NewReader("garbage"))
	badRequest.Header.Set("Content-Type", "multipart/form-data")
	recorder = httptest.NewRecorder()
	if app.csrf(recorder, badRequest, session) || recorder.Code != http.StatusUnprocessableEntity {
		t.Fatal(recorder.Code)
	}
	data := app.dashboardData(context.Background(), session, "original")
	if data.PlaylistLoadError == "" || strings.Contains(data.PlaylistLoadError, "private detail") {
		t.Fatal(data.PlaylistLoadError)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	data = app.dashboardData(context.Background(), session, "original")
	if !strings.Contains(data.Error, "could not be loaded") {
		t.Fatal(data.Error)
	}
}

func TestAuthDisabledLoginRedirect(t *testing.T) {
	server, _ := newTestApp(t, false)
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		request, _ := http.NewRequest(method, server.URL+"/login", nil)
		response, err := client.Do(request)
		if err != nil || response.StatusCode != http.StatusSeeOther {
			t.Fatal(method, response, err)
		}
		_ = response.Body.Close()
	}
}

func TestProtectedRoutesAndCSRFFailClosed(t *testing.T) {
	protectedServer, _ := newTestApp(t, true)
	protectedClient := protectedServer.Client()
	protectedClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for _, route := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/playlist-v1.schema.json"},
		{http.MethodPost, "/previews"},
		{http.MethodPost, "/recommend"},
		{http.MethodPost, "/recommend/from-playlists"},
		{http.MethodGet, "/previews/missing"},
		{http.MethodPost, "/previews/missing/refine"},
		{http.MethodPost, "/previews/missing/publish"},
		{http.MethodGet, "/operations/missing"},
		{http.MethodPost, "/operations/missing/restore"},
	} {
		request, err := http.NewRequest(route.method, protectedServer.URL+route.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := protectedClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s %s: status=%v", route.method, route.path, response.StatusCode)
		}
		_ = response.Body.Close()
	}

	localServer, _ := newTestApp(t, false)
	localClient := localServer.Client()
	localClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	_, cookie, _ := getPage(t, localClient, localServer.URL+"/")
	for _, path := range []string{
		"/previews", "/recommend", "/recommend/from-playlists",
		"/previews/missing/refine", "/previews/missing/publish",
		"/operations/missing/restore", "/logout",
	} {
		response, _ := post(t, localClient, localServer.URL+path, url.Values{"csrf": {"invalid"}}, cookie)
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("POST %s: status=%d", path, response.StatusCode)
		}
	}

	// Login must reject the request before doing an expensive password check.
	_, loginCookie, _ := getPage(t, protectedClient, protectedServer.URL+"/login")
	response, _ := post(t, protectedClient, protectedServer.URL+"/login", url.Values{"csrf": {"invalid"}, "password": {"secret"}}, loginCookie)
	if response.StatusCode != http.StatusForbidden {
		t.Fatal(response.StatusCode)
	}
}

func clientWithCookie(base *http.Client, cookie *http.Cookie) *http.Client {
	return &http.Client{Transport: roundTripper{base.Transport, cookie}, CheckRedirect: base.CheckRedirect}
}

type roundTripper struct {
	base   http.RoundTripper
	cookie *http.Cookie
}

func (r roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if r.base == nil {
		r.base = http.DefaultTransport
	}
	req.AddCookie(r.cookie)
	return r.base.RoundTrip(req)
}
