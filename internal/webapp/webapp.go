// Package webapp serves the embedded local user interface and HTTP endpoints.
package webapp

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pawel/qobuz-curator/internal/config"
	"github.com/pawel/qobuz-curator/internal/model"
	"github.com/pawel/qobuz-curator/internal/provider"
	"github.com/pawel/qobuz-curator/internal/recommend"
	"github.com/pawel/qobuz-curator/internal/security"
	"github.com/pawel/qobuz-curator/internal/service"
	"github.com/pawel/qobuz-curator/internal/store"
	"go.uber.org/zap"
)

//go:embed assets
var assets embed.FS

const requestTimeout = 110 * time.Second

// App owns the embedded HTTP interface and its process-local session state.
type App struct {
	Config      config.Config
	Service     *service.Service
	Provider    provider.Provider
	Recommender recommend.Recommender
	Store       *store.Store
	templates   *template.Template
	logins      *loginLimiter
	passwords   chan struct{}
	activeMu    sync.RWMutex
	active      map[string]int64
}
type page struct {
	Title, CSRF, Error, PlaylistLoadError, RefinementPrompt, ChatGPTPrompt string
	AuthDisabled, OpenAIEnabled                                            bool
	Playlists                                                              []model.PlaylistSummary
	Operations                                                             []model.Operation
	Preview                                                                model.Preview
	Operation                                                              model.Operation
}

// New parses embedded templates and wires the web application dependencies.
func New(cfg config.Config, s *service.Service, p provider.Provider, r recommend.Recommender, db *store.Store) (*App, error) {
	funcs := template.FuncMap{"join": joinText, "matched": matched, "skipped": skipped, "score": formatScore, "text": pointerText, "deref": deref, "number": number, "decimal": decimal, "mutable": mutable}
	t, e := template.New("all").Funcs(funcs).ParseFS(assets, "assets/templates/*.html")
	if e != nil {
		return nil, e
	}
	return &App{
		Config: cfg, Service: s, Provider: p, Recommender: r, Store: db,
		templates: t, logins: newLoginLimiter(5, 15*time.Minute), passwords: make(chan struct{}, 1), active: make(map[string]int64),
	}, nil
}
func joinText(v []string) string   { return strings.Join(v, ", ") }
func matched(p model.Preview) int  { return p.MatchedCount() }
func skipped(p model.Preview) int  { return p.SkippedCount() }
func formatScore(v float64) string { return fmt.Sprintf("%.3f", v) }
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
func number(p *int) string {
	if p == nil {
		return "?"
	}
	return strconv.Itoa(*p)
}
func decimal(p *float64) string {
	if p == nil {
		return "?"
	}
	return strconv.FormatFloat(*p, 'f', -1, 64)
}
func mutable(mode string) bool { return mode == "append_existing" || mode == "replace_existing" }
func pointerText(v any, fallback string) string {
	if v == nil {
		return fallback
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return fallback
		}
		return fmt.Sprint(rv.Elem().Interface())
	}
	s := fmt.Sprint(v)
	if s == "" {
		return fallback
	}
	return s
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "assets/static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("GET /playlist-v1.schema.json", a.schema)
	mux.HandleFunc("GET /login", a.loginPage)
	mux.HandleFunc("POST /login", a.login)
	mux.HandleFunc("POST /logout", a.logout)
	mux.HandleFunc("GET /", a.dashboard)
	mux.HandleFunc("POST /previews", a.createPreview)
	mux.HandleFunc("POST /recommend", a.createRecommendation)
	mux.HandleFunc("POST /recommend/from-playlists", a.fromPlaylists)
	mux.HandleFunc("GET /previews/{id}", a.preview)
	mux.HandleFunc("POST /previews/{id}/refine", a.refine)
	mux.HandleFunc("POST /previews/{id}/publish", a.publish)
	mux.HandleFunc("GET /operations/{id}", a.operation)
	mux.HandleFunc("POST /operations/{id}/restore", a.restore)
	bounded := http.TimeoutHandler(http.MaxBytesHandler(mux, 2<<20), requestTimeout, "request timed out\n")
	return a.recoverPanic(a.accessLog(a.validateHost(a.securityHeaders(bounded))))
}

func (a *App) session(r *http.Request) (security.Session, error) {
	cookie, e := r.Cookie("qobuz_curator")
	if a.Config.SecureCookies {
		cookie, e = r.Cookie("__Host-qobuz_curator")
	}
	if e == nil {
		if s, ok := security.DecodeSession(cookie.Value, a.Config.SessionSecret); ok {
			return s, nil
		}
	}
	return security.NewSession(time.Duration(a.Config.SessionTTLHours) * time.Hour)
}
func (a *App) saveSession(w http.ResponseWriter, s security.Session) {
	name := "qobuz_curator"
	if a.Config.SecureCookies {
		name = "__Host-qobuz_curator"
	}
	maxAge := max(0, int(time.Until(time.Unix(s.ExpiresAt, 0)).Seconds()))
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: security.EncodeSession(s, a.Config.SessionSecret), Path: "/",
		Expires: time.Unix(s.ExpiresAt, 0), MaxAge: maxAge, HttpOnly: true,
		Secure: a.Config.SecureCookies, SameSite: http.SameSiteStrictMode,
	})
}
func (a *App) authorize(w http.ResponseWriter, r *http.Request) (security.Session, bool) {
	s, err := a.session(r)
	if err != nil {
		a.internalError(w, r, err)
		return security.Session{}, false
	}
	a.saveSession(w, s)
	if a.Config.AuthDisabled || (s.Authenticated && a.sessionIsActive(s)) {
		return s, true
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
	return s, false
}
func (a *App) csrf(w http.ResponseWriter, r *http.Request, s security.Session) bool {
	if e := r.ParseMultipartForm(2 << 20); e != nil && e != http.ErrNotMultipart {
		zap.L().Warn("invalid form submission", zap.String("path", r.URL.Path), zap.Error(e))
		http.Error(w, "invalid form submission", http.StatusUnprocessableEntity)
		return false
	}
	if !security.ValidCSRF(s, r.FormValue("csrf")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return false
	}
	return true
}
func (a *App) render(w http.ResponseWriter, status int, name string, data page) {
	var output bytes.Buffer
	if e := a.templates.ExecuteTemplate(&output, name, data); e != nil {
		zap.L().Error("render template", zap.String("template", name), zap.Error(e))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = output.WriteTo(w)
}
func (a *App) base(s security.Session) page {
	return page{CSRF: s.CSRF, AuthDisabled: a.Config.AuthDisabled, OpenAIEnabled: a.Config.OpenAIAPIKey != ""}
}
func (a *App) dashboardData(ctx context.Context, s security.Session, message string) page {
	d := a.base(s)
	d.Error = message
	var err error
	d.Operations, err = a.Store.Operations(10)
	if err != nil {
		zap.L().Error("load operation history", zap.Error(err))
		d.Error = "Operation history could not be loaded."
	}
	d.Playlists, err = a.Provider.ListPlaylists(ctx)
	if err != nil {
		zap.L().Warn("load provider playlists", zap.Error(err))
		d.PlaylistLoadError = "Could not load Qobuz playlists. Check the application logs and credentials."
	}
	return d
}
func (a *App) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
func (a *App) schema(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authorize(w, r); !ok {
		return
	}
	raw, e := assets.ReadFile("assets/playlist-v1.schema.json")
	if e != nil {
		a.internalError(w, r, e)
		return
	}
	w.Header().Set("Content-Type", "application/schema+json")
	w.Header().Set("Content-Disposition", `attachment; filename="playlist-v1.schema.json"`)
	_, _ = w.Write(raw)
}
func (a *App) dashboard(w http.ResponseWriter, r *http.Request) {
	s, ok := a.authorize(w, r)
	if !ok {
		return
	}
	a.render(w, 200, "index", a.dashboardData(r.Context(), s, ""))
}
func (a *App) loginPage(w http.ResponseWriter, r *http.Request) {
	if a.Config.AuthDisabled {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s, err := a.session(r)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	a.saveSession(w, s)
	a.render(w, 200, "login", a.base(s))
}
func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if a.Config.AuthDisabled {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s, err := a.session(r)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	a.saveSession(w, s)
	if !a.csrf(w, r, s) {
		return
	}
	client := clientAddress(r)
	if !a.logins.allow(client, time.Now()) {
		zap.L().Warn("web login rate limited", zap.String("client", client))
		http.Error(w, "too many login attempts; try again later", http.StatusTooManyRequests)
		return
	}
	passwordOK, available := a.verifyPassword(r.FormValue("password"))
	if !available {
		zap.L().Warn("web login rejected while password verifier is busy", zap.String("client", client))
		http.Error(w, "another login attempt is being checked; try again", http.StatusTooManyRequests)
		return
	}
	if !passwordOK {
		a.logins.failure(client, time.Now())
		zap.L().Warn("web login failed", zap.String("client", client))
		d := a.base(s)
		d.Error = "Incorrect password"
		a.render(w, 401, "login", d)
		return
	}
	a.logins.reset(client)
	zap.L().Info("web login succeeded", zap.String("client", client))
	s.Authenticated = true
	a.activateSession(s)
	a.saveSession(w, s)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	s, err := a.session(r)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if !a.csrf(w, r, s) {
		return
	}
	a.deactivateSession(s)
	zap.L().Info("web session logged out", zap.String("client", clientAddress(r)))
	s, err = security.NewSession(time.Duration(a.Config.SessionTTLHours) * time.Hour)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	a.saveSession(w, s)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
func (a *App) createPreview(w http.ResponseWriter, r *http.Request) {
	s, ok := a.authorize(w, r)
	if !ok {
		return
	}
	if !a.csrf(w, r, s) {
		return
	}
	raw := []byte(r.FormValue("playlist_json"))
	file, _, fileErr := r.FormFile("playlist_file")
	if fileErr == nil {
		defer file.Close()
		var readErr error
		raw, readErr = readUpload(file, 2<<20)
		if readErr != nil {
			a.render(w, http.StatusUnprocessableEntity, "index", a.dashboardData(r.Context(), s, readErr.Error()))
			return
		}
	} else if !errors.Is(fileErr, http.ErrMissingFile) && !errors.Is(fileErr, http.ErrNotMultipart) {
		a.render(w, http.StatusUnprocessableEntity, "index", a.dashboardData(r.Context(), s, "could not read the uploaded playlist"))
		return
	}
	input, e := model.DecodePlaylist(raw)
	if e == nil {
		var preview model.Preview
		preview, e = a.Service.Prepare(r.Context(), input)
		if e == nil {
			zap.L().Info("JSON playlist resolved to a preview", zap.String("preview_id", preview.ID), zap.Int("track_count", len(input.Tracks)))
			http.Redirect(w, r, "/previews/"+preview.ID, http.StatusSeeOther)
			return
		}
	}
	a.render(w, 422, "index", a.dashboardData(r.Context(), s, e.Error()))
}
func (a *App) createRecommendation(w http.ResponseWriter, r *http.Request) {
	s, ok := a.authorize(w, r)
	if !ok {
		return
	}
	if !a.csrf(w, r, s) {
		return
	}
	zap.L().Info("starting written-prompt recommendation")
	input, e := a.Recommender.Recommend(r.Context(), r.FormValue("prompt"))
	a.prepareOrError(w, r, s, input, e)
}
func (a *App) prepareOrError(w http.ResponseWriter, r *http.Request, s security.Session, input model.PlaylistInput, e error) {
	if e == nil {
		var preview model.Preview
		preview, e = a.Service.Prepare(r.Context(), input)
		if e == nil {
			zap.L().Info("recommendation prepared for review", zap.String("preview_id", preview.ID), zap.Int("track_count", len(input.Tracks)))
			http.Redirect(w, r, "/previews/"+preview.ID, http.StatusSeeOther)
			return
		}
	}
	a.render(w, 422, "index", a.dashboardData(r.Context(), s, e.Error()))
}
func (a *App) fromPlaylists(w http.ResponseWriter, r *http.Request) {
	s, ok := a.authorize(w, r)
	if !ok {
		return
	}
	if !a.csrf(w, r, s) {
		return
	}
	ids := r.Form["playlist_ids"]
	if len(ids) == 0 {
		a.render(w, 422, "index", a.dashboardData(r.Context(), s, "select at least one source playlist"))
		return
	}
	if len(ids) > 10 {
		a.render(w, 422, "index", a.dashboardData(r.Context(), s, "select no more than 10 source playlists"))
		return
	}
	count, e := strconv.Atoi(r.FormValue("track_count"))
	var playlists []model.ProviderPlaylist
	if e == nil {
		seen := map[string]bool{}
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				var p model.ProviderPlaylist
				p, e = a.Provider.GetPlaylist(r.Context(), id)
				if e != nil {
					break
				}
				playlists = append(playlists, p)
			}
		}
	}
	var input model.PlaylistInput
	if e == nil {
		zap.L().Info("starting playlist-seeded recommendation", zap.Int("source_playlist_count", len(playlists)), zap.Int("requested_track_count", count))
		input, e = a.Recommender.FromPlaylists(r.Context(), playlists, count, r.FormValue("additional_prompt"))
	}
	a.prepareOrError(w, r, s, input, e)
}
func (a *App) previewData(ctx context.Context, s security.Session, p model.Preview, message, prompt string) page {
	d := a.base(s)
	d.Title = "Preview"
	d.Preview = p
	d.Error = message
	d.RefinementPrompt = prompt
	d.ChatGPTPrompt = recommend.ChatGPTRefinementPrompt(p.Playlist)
	d.Playlists, _ = a.Provider.ListPlaylists(ctx)
	return d
}
func (a *App) preview(w http.ResponseWriter, r *http.Request) {
	s, ok := a.authorize(w, r)
	if !ok {
		return
	}
	p, e := a.Service.Preview(r.PathValue("id"))
	if e != nil {
		http.Error(w, "preview not found or expired", http.StatusNotFound)
		return
	}
	a.render(w, 200, "preview", a.previewData(r.Context(), s, p, "", ""))
}
func (a *App) refine(w http.ResponseWriter, r *http.Request) {
	s, ok := a.authorize(w, r)
	if !ok {
		return
	}
	if !a.csrf(w, r, s) {
		return
	}
	p, e := a.Service.Preview(r.PathValue("id"))
	prompt := strings.TrimSpace(r.FormValue("prompt"))
	if e == nil && prompt == "" {
		e = fmt.Errorf("describe how to refine the playlist")
	}
	var input model.PlaylistInput
	if e == nil {
		zap.L().Info("starting playlist refinement", zap.String("preview_id", p.ID))
		input, e = a.Recommender.Refine(r.Context(), p.Playlist, prompt)
	}
	if e == nil {
		var next model.Preview
		next, e = a.Service.Prepare(r.Context(), input)
		if e == nil {
			http.Redirect(w, r, "/previews/"+next.ID, http.StatusSeeOther)
			return
		}
	}
	a.render(w, 422, "preview", a.previewData(r.Context(), s, p, e.Error(), prompt))
}
func (a *App) publish(w http.ResponseWriter, r *http.Request) {
	s, ok := a.authorize(w, r)
	if !ok {
		return
	}
	if !a.csrf(w, r, s) {
		return
	}
	confirmed := r.FormValue("confirmed") == "true"
	var o model.Operation
	var e error
	switch r.FormValue("mode") {
	case "create_new":
		o, e = a.Service.Create(r.Context(), r.PathValue("id"), strings.TrimSpace(r.FormValue("new_name")), confirmed)
	case "append_existing":
		o, e = a.Service.Append(r.Context(), r.PathValue("id"), r.FormValue("append_playlist_id"), confirmed)
	case "replace_existing":
		o, e = a.Service.Replace(r.Context(), r.PathValue("id"), r.FormValue("playlist_id"), confirmed)
	default:
		e = fmt.Errorf("choose create new, append existing, or replace existing")
	}
	if e != nil {
		zap.L().Warn("publish playlist failed", zap.String("mode", r.FormValue("mode")), zap.Error(e))
		http.Error(w, "playlist could not be published; check the application logs", http.StatusUnprocessableEntity)
		return
	}
	zap.L().Info("playlist publish completed", zap.String("operation_id", o.ID), zap.String("mode", o.Mode), zap.String("status", o.Status))
	http.Redirect(w, r, "/operations/"+o.ID, http.StatusSeeOther)
}
func (a *App) operation(w http.ResponseWriter, r *http.Request) {
	s, ok := a.authorize(w, r)
	if !ok {
		return
	}
	o, e := a.Store.Operation(r.PathValue("id"))
	if e != nil {
		http.Error(w, "operation not found", http.StatusNotFound)
		return
	}
	d := a.base(s)
	d.Operation = o
	d.Title = "Operation"
	a.render(w, 200, "operation", d)
}
func (a *App) restore(w http.ResponseWriter, r *http.Request) {
	s, ok := a.authorize(w, r)
	if !ok {
		return
	}
	if !a.csrf(w, r, s) {
		return
	}
	o, e := a.Service.Restore(r.Context(), r.PathValue("id"), r.FormValue("confirmed") == "true")
	if e != nil {
		zap.L().Warn("restore playlist failed", zap.String("operation_id", r.PathValue("id")), zap.Error(e))
		http.Error(w, "playlist could not be restored; check the application logs", http.StatusUnprocessableEntity)
		return
	}
	zap.L().Info("playlist restore completed", zap.String("operation_id", o.ID))
	http.Redirect(w, r, "/operations/"+o.ID, http.StatusSeeOther)
}
