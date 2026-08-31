package webapp

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pawel/qobuz-curator/internal/security"
	"go.uber.org/zap"
)

type loginAttempt struct {
	failures     int
	windowStart  time.Time
	blockedUntil time.Time
}

const (
	maxLoginClients   = 4096
	maxActiveSessions = 128
)

// loginLimiter is intentionally process-local. This is a single-user service,
// and avoiding client-controlled proxy headers prevents simple rate-limit
// bypasses. Reverse proxies should apply their own outer limit as well.
type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string]loginAttempt
	maximum  int
	window   time.Duration
}

func newLoginLimiter(maximum int, window time.Duration) *loginLimiter {
	return &loginLimiter{attempts: make(map[string]loginAttempt), maximum: maximum, window: window}
}

func (l *loginLimiter) allow(client string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	attempt := l.attempts[client]
	if !attempt.blockedUntil.IsZero() && now.Before(attempt.blockedUntil) {
		return false
	}
	if !attempt.windowStart.IsZero() && now.Sub(attempt.windowStart) >= l.window {
		delete(l.attempts, client)
	}
	if _, known := l.attempts[client]; !known && len(l.attempts) >= maxLoginClients {
		for address, candidate := range l.attempts {
			if !now.Before(candidate.blockedUntil) && now.Sub(candidate.windowStart) >= l.window {
				delete(l.attempts, address)
			}
		}
		if len(l.attempts) >= maxLoginClients {
			return false
		}
	}
	return true
}

func (l *loginLimiter) failure(client string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	attempt := l.attempts[client]
	if attempt.windowStart.IsZero() || now.Sub(attempt.windowStart) >= l.window {
		attempt = loginAttempt{windowStart: now}
	}
	attempt.failures++
	if attempt.failures >= l.maximum {
		attempt.blockedUntil = now.Add(l.window)
	}
	l.attempts[client] = attempt
}

func (l *loginLimiter) reset(client string) {
	l.mu.Lock()
	delete(l.attempts, client)
	l.mu.Unlock()
}

func clientAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

// validateHost protects a loopback deployment from DNS-rebinding attacks.
// Non-loopback proxy names must be declared explicitly in allowed_hosts.
func (a *App) validateHost(next http.Handler) http.Handler {
	allowed := map[string]struct{}{"localhost": {}}
	if host := strings.Trim(strings.ToLower(a.Config.Host), "[]"); host != "" && host != "0.0.0.0" && host != "::" {
		allowed[host] = struct{}{}
	}
	for _, host := range a.Config.AllowedHosts {
		allowed[strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")] = struct{}{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if parsed, _, err := net.SplitHostPort(host); err == nil {
			host = parsed
		}
		host = strings.Trim(strings.ToLower(host), "[]")
		ip := net.ParseIP(host)
		_, explicitlyAllowed := allowed[host]
		if !explicitlyAllowed && (ip == nil || !ip.IsLoopback()) {
			zap.L().Warn("rejected unrecognized Host header", zap.String("host", host), zap.String("client", clientAddress(r)))
			http.Error(w, "unrecognized host", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) activateSession(s security.Session) {
	a.activeMu.Lock()
	now := time.Now().Unix()
	for token, expiresAt := range a.active {
		if expiresAt <= now {
			delete(a.active, token)
		}
	}
	if len(a.active) >= maxActiveSessions {
		var oldestToken string
		var oldestExpiry int64
		for token, expiresAt := range a.active {
			if oldestToken == "" || expiresAt < oldestExpiry {
				oldestToken, oldestExpiry = token, expiresAt
			}
		}
		delete(a.active, oldestToken)
	}
	a.active[s.CSRF] = s.ExpiresAt
	a.activeMu.Unlock()
}

// verifyPassword serializes the memory-hard scrypt operation. A small local
// service should reject concurrent guesses instead of allowing them to multiply
// the KDF's memory cost into a process-level denial of service.
func (a *App) verifyPassword(password string) (matched, available bool) {
	select {
	case a.passwords <- struct{}{}:
		defer func() { <-a.passwords }()
		return security.VerifyPassword(password, a.Config.PasswordHash), true
	default:
		return false, false
	}
}

func (a *App) deactivateSession(s security.Session) {
	a.activeMu.Lock()
	delete(a.active, s.CSRF)
	a.activeMu.Unlock()
}

func (a *App) sessionIsActive(s security.Session) bool {
	a.activeMu.RLock()
	expiresAt, ok := a.active[s.CSRF]
	a.activeMu.RUnlock()
	return ok && expiresAt > time.Now().Unix()
}

func (a *App) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; script-src 'self'; img-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("Permissions-Policy", "camera=(), geolocation=(), microphone=()")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				zap.L().Error("request panic", zap.String("method", r.Method), zap.String("path", r.URL.Path), zap.Any("panic", recovered), zap.Stack("stack"))
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (a *App) internalError(w http.ResponseWriter, r *http.Request, err error) {
	zap.L().Error("request failed", zap.String("method", r.Method), zap.String("path", r.URL.Path), zap.Error(err))
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

type responseRecorder struct {
	http.ResponseWriter
	status int
}

func (w *responseRecorder) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseRecorder) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(body)
}

// accessLog emits one structured completion event per HTTP request. It omits
// query strings and form bodies so prompts, playlist names, and secrets never
// enter logs.
func (a *App) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		zap.L().Debug("HTTP request completed",
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.Int("status", status),
			zap.Duration("duration", time.Since(started)),
			zap.String("client", clientAddress(r)),
		)
	})
}

func readUpload(r io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("playlist file exceeds %d bytes", limit)
	}
	return raw, nil
}
