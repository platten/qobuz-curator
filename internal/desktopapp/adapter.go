package desktopapp

import (
	"bytes"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
)

var navigationPage = template.Must(template.New("navigation").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta http-equiv="refresh" content="0;url={{.}}"><title>Qobuz Curator</title></head>
<body><p>Opening <a href="{{.}}">Qobuz Curator</a>…</p></body></html>`))

// RedirectCompatible converts redirects into a navigation document because
// Wails v2's asset handler does not implement 30x navigation on macOS/Linux.
func RedirectCompatible(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorded := httptest.NewRecorder()
		next.ServeHTTP(recorded, r)
		response := recorded.Result()
		defer response.Body.Close()
		for name, values := range response.Header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			location := response.Header.Get("Location")
			if !safeInternalLocation(location) {
				http.Error(w, "refused unsafe desktop navigation", http.StatusBadGateway)
				return
			}
			w.Header().Del("Location")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; form-action 'none'")
			w.WriteHeader(http.StatusOK)
			_ = navigationPage.Execute(w, location)
			return
		}
		w.WriteHeader(response.StatusCode)
		_, _ = bytes.NewBuffer(recorded.Body.Bytes()).WriteTo(w)
	})
}

func safeInternalLocation(location string) bool {
	return strings.HasPrefix(location, "/") && !strings.HasPrefix(location, "//") && !strings.ContainsAny(location, "\r\n")
}
