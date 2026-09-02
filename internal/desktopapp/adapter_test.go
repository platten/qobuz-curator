package desktopapp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRedirectCompatible(t *testing.T) {
	handler := RedirectCompatible(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "session=value")
		http.Redirect(w, r, "/previews/id", http.StatusSeeOther)
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "http://wails.localhost/action", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "/previews/id") {
		t.Fatalf("unexpected navigation response: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Set-Cookie") != "session=value" || recorder.Header().Get("Location") != "" {
		t.Fatalf("headers = %#v", recorder.Header())
	}
}

func TestRedirectCompatibleRejectsExternalTargetAndCopiesNormalResponse(t *testing.T) {
	external := RedirectCompatible(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example", http.StatusFound)
	}))
	recorder := httptest.NewRecorder()
	external.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://wails.localhost/", nil))
	if recorder.Code != http.StatusBadGateway {
		t.Fatal(recorder.Code)
	}
	normal := RedirectCompatible(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Test", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	}))
	recorder = httptest.NewRecorder()
	normal.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://wails.localhost/", nil))
	if recorder.Code != http.StatusCreated || recorder.Body.String() != "created" || recorder.Header().Get("X-Test") != "yes" {
		t.Fatalf("unexpected normal response: %d %q %#v", recorder.Code, recorder.Body.String(), recorder.Header())
	}
}

func TestSafeInternalLocation(t *testing.T) {
	for _, value := range []string{"", "relative", "//evil.example", "https://evil.example", "/ok\r\nX: bad"} {
		if safeInternalLocation(value) {
			t.Fatalf("accepted %q", value)
		}
	}
	if !safeInternalLocation("/operations/id") {
		t.Fatal("rejected internal location")
	}
}
