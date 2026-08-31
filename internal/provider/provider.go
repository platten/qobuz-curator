// Package provider adapts music catalogs and playlist services to the core application.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pawel/qobuz-curator/internal/config"
	"github.com/pawel/qobuz-curator/internal/httpretry"
	"github.com/pawel/qobuz-curator/internal/matching"
	"github.com/pawel/qobuz-curator/internal/model"
	"go.uber.org/zap"
)

const qobuzOperationTimeout = 45 * time.Second

// Provider is the catalog and playlist boundary used by the service layer.
type Provider interface {
	SearchTracks(context.Context, string, int) ([]model.CatalogTrack, error)
	ListPlaylists(context.Context) ([]model.PlaylistSummary, error)
	GetPlaylist(context.Context, string) (model.ProviderPlaylist, error)
	CreatePlaylist(context.Context, string, string) (model.PlaylistSummary, error)
	AddTracks(context.Context, string, []string) (model.ProviderPlaylist, error)
	ClearPlaylist(context.Context, string) (model.ProviderPlaylist, error)
}

func pointer[T any](value T) *T { return &value }

// DemoCatalog is the deterministic catalog exposed by the fake provider.
var DemoCatalog = []model.CatalogTrack{
	{ID: "demo-1", Title: "So What", Artists: []string{"Miles Davis"}, Album: pointer("Kind of Blue"), ReleaseYear: pointer(1959), DurationSeconds: pointer(545), ISRC: pointer("USSM15900101"), Explicit: pointer(false), MaximumBitDepth: pointer(24), MaximumSamplingRate: pointer(192.0)},
	{ID: "demo-2", Title: "Blue in Green", Artists: []string{"Miles Davis"}, Album: pointer("Kind of Blue"), ReleaseYear: pointer(1959), DurationSeconds: pointer(337), MaximumBitDepth: pointer(24), MaximumSamplingRate: pointer(192.0)},
	{ID: "demo-3", Title: "Take Five", Artists: []string{"The Dave Brubeck Quartet"}, Album: pointer("Time Out"), ReleaseYear: pointer(1959), DurationSeconds: pointer(324), MaximumBitDepth: pointer(24), MaximumSamplingRate: pointer(176.4)},
	{ID: "demo-4", Title: "Take Five (Live)", Artists: []string{"Dave Brubeck"}, Album: pointer("Live in Belgium 1964"), ReleaseYear: pointer(1964), DurationSeconds: pointer(410), Version: pointer("live"), MaximumBitDepth: pointer(16), MaximumSamplingRate: pointer(44.1)},
	{ID: "demo-5", Title: "A Love Supreme, Pt. I – Acknowledgement", Artists: []string{"John Coltrane"}, Album: pointer("A Love Supreme"), ReleaseYear: pointer(1965), DurationSeconds: pointer(447), MaximumBitDepth: pointer(24), MaximumSamplingRate: pointer(96.0)},
	{ID: "demo-6", Title: "My Favorite Things", Artists: []string{"John Coltrane"}, Album: pointer("My Favorite Things"), ReleaseYear: pointer(1961), DurationSeconds: pointer(824), MaximumBitDepth: pointer(24), MaximumSamplingRate: pointer(192.0)},
}

// Fake is a concurrency-safe in-memory provider for demos and tests.
type Fake struct {
	mu        sync.Mutex
	Catalog   []model.CatalogTrack
	Playlists map[string]model.ProviderPlaylist
	next      int
}

// NewFake returns an empty provider seeded with DemoCatalog.
func NewFake() *Fake {
	return &Fake{Catalog: append([]model.CatalogTrack(nil), DemoCatalog...), Playlists: map[string]model.ProviderPlaylist{}}
}
func (f *Fake) SearchTracks(_ context.Context, query string, limit int) ([]model.CatalogTrack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	terms := strings.Fields(matching.Normalize(query))
	type hit struct {
		n int
		t model.CatalogTrack
	}
	var hits []hit
	for _, t := range f.Catalog {
		text := matching.Normalize(t.Title + " " + strings.Join(t.Artists, " ") + " " + value(t.Album))
		n := 0
		for _, term := range terms {
			if strings.Contains(text, term) {
				n++
			}
		}
		if n > 0 {
			hits = append(hits, hit{n, t})
		}
	}
	for i := 0; i < len(hits); i++ {
		for j := i + 1; j < len(hits); j++ {
			if hits[j].n > hits[i].n {
				hits[i], hits[j] = hits[j], hits[i]
			}
		}
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	result := make([]model.CatalogTrack, len(hits))
	for i, h := range hits {
		result[i] = h.t
	}
	return result, nil
}
func (f *Fake) ListPlaylists(_ context.Context) ([]model.PlaylistSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := make([]model.PlaylistSummary, 0, len(f.Playlists))
	for _, p := range f.Playlists {
		r = append(r, p.Summary)
	}
	return r, nil
}
func (f *Fake) GetPlaylist(_ context.Context, id string) (model.ProviderPlaylist, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.Playlists[id]
	if !ok {
		return p, fmt.Errorf("playlist %s was not found", id)
	}
	return p, nil
}
func (f *Fake) CreatePlaylist(_ context.Context, name, description string) (model.PlaylistSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	id := fmt.Sprintf("fake-%d", f.next)
	u := "https://open.qobuz.com/playlist/" + id
	s := model.PlaylistSummary{ID: id, Name: name, Description: description, URL: &u}
	f.Playlists[id] = model.ProviderPlaylist{Summary: s, Tracks: []model.CatalogTrack{}}
	return s, nil
}
func (f *Fake) AddTracks(_ context.Context, id string, ids []string) (model.ProviderPlaylist, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.Playlists[id]
	if !ok {
		return p, fmt.Errorf("playlist %s was not found", id)
	}
	byID := map[string]model.CatalogTrack{}
	for _, t := range f.Catalog {
		byID[t.ID] = t
	}
	for _, trackID := range ids {
		t, ok := byID[trackID]
		if !ok {
			return p, fmt.Errorf("unknown track id: %s", trackID)
		}
		duplicate := false
		for _, existing := range p.Tracks {
			if existing.ID == trackID {
				duplicate = true
			}
		}
		if !duplicate {
			p.Tracks = append(p.Tracks, t)
		}
	}
	p.Summary.TrackCount = len(p.Tracks)
	f.Playlists[id] = p
	return p, nil
}
func (f *Fake) ClearPlaylist(_ context.Context, id string) (model.ProviderPlaylist, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.Playlists[id]
	if !ok {
		return p, fmt.Errorf("playlist %s was not found", id)
	}
	p.Tracks = []model.CatalogTrack{}
	p.Summary.TrackCount = 0
	f.Playlists[id] = p
	return p, nil
}

// Qobuz adapts the private Qobuz web API to Provider.
type Qobuz struct {
	Base   string
	AppID  string
	Token  string
	Client *http.Client

	userIDMu sync.Mutex
	userID   string
}

// New constructs the configured provider implementation.
func New(cfg config.Config) Provider {
	if cfg.Provider == "fake" {
		return NewFake()
	}
	return &Qobuz{
		Base:   strings.TrimRight(cfg.QobuzAPIBase, "/"),
		AppID:  cfg.QobuzAppID,
		Token:  cfg.QobuzUserToken,
		Client: &http.Client{Timeout: 20 * time.Second},
	}
}
func (q *Qobuz) request(ctx context.Context, method, path string, params url.Values) (map[string]any, error) {
	requestContext, cancel := context.WithTimeout(ctx, qobuzOperationTimeout)
	defer cancel()
	endpoint := path
	encoded := params.Encode()
	requestURL := q.Base + "/" + strings.TrimLeft(path, "/")
	if method == http.MethodGet && encoded != "" {
		requestURL += "?" + encoded
	}
	makeRequest := func() (*http.Request, error) {
		var body io.Reader
		if method != http.MethodGet {
			body = bytes.NewBufferString(encoded)
		}
		req, err := http.NewRequestWithContext(requestContext, method, requestURL, body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-App-Id", q.AppID)
		req.Header.Set("X-User-Auth-Token", q.Token)
		req.Header.Set("User-Agent", "QobuzCurator/1.0")
		if method != http.MethodGet {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		return req, nil
	}
	zap.L().Debug("sending Qobuz request", zap.String("method", method), zap.String("endpoint", endpoint), zap.Bool("retryable", method == http.MethodGet))
	resp, err := httpretry.Do(requestContext, q.Client, makeRequest, method == http.MethodGet, zap.L(), "Qobuz "+endpoint, httpretry.DefaultPolicy())
	if err != nil {
		return nil, fmt.Errorf("Qobuz %s failed: %w", endpoint, err)
	}
	defer resp.Body.Close()
	raw, err := readBounded(resp.Body, 4<<20)
	if err != nil {
		return nil, fmt.Errorf("read Qobuz %s response: %w", endpoint, err)
	}
	if resp.StatusCode/100 != 2 {
		zap.L().Warn("Qobuz request rejected", zap.String("endpoint", endpoint), zap.Int("http_status", resp.StatusCode))
		return nil, fmt.Errorf("Qobuz %s returned HTTP %d", endpoint, resp.StatusCode)
	}
	var data map[string]any
	if err = json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("decode Qobuz response: %w", err)
	}
	if data["status"] == "error" {
		zap.L().Warn("Qobuz returned an application error", zap.String("endpoint", endpoint))
		return nil, fmt.Errorf("Qobuz %s reported an error", endpoint)
	}
	zap.L().Debug("Qobuz request completed", zap.String("endpoint", endpoint), zap.Int("http_status", resp.StatusCode))
	return data, nil
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
func (q *Qobuz) SearchTracks(ctx context.Context, query string, limit int) ([]model.CatalogTrack, error) {
	d, e := q.request(ctx, "GET", "catalog/search", url.Values{"query": {query}, "type": {"tracks"}, "limit": {strconv.Itoa(min(limit, 50))}})
	if e != nil {
		return nil, e
	}
	var result []model.CatalogTrack
	for _, item := range items(nested(d, "tracks")) {
		result = append(result, parseTrack(item))
	}
	return result, nil
}
func (q *Qobuz) ListPlaylists(ctx context.Context) ([]model.PlaylistSummary, error) {
	q.userIDMu.Lock()
	defer q.userIDMu.Unlock()
	if q.userID == "" {
		d, e := q.request(ctx, "GET", "user/get", nil)
		if e != nil {
			return nil, e
		}
		q.userID = text(d["id"])
	}
	d, e := q.request(ctx, "GET", "playlist/getUserPlaylists", url.Values{"user_id": {q.userID}, "type": {"owner"}, "limit": {"500"}})
	if e != nil {
		return nil, e
	}
	container := nested(d, "playlists")
	if len(container) == 0 {
		container = d
	}
	var result []model.PlaylistSummary
	for _, item := range items(container) {
		result = append(result, parseSummary(item))
	}
	return result, nil
}
func (q *Qobuz) GetPlaylist(ctx context.Context, id string) (model.ProviderPlaylist, error) {
	d, e := q.request(ctx, "GET", "playlist/get", url.Values{"playlist_id": {id}, "extra": {"tracks"}, "limit": {"500"}})
	if e != nil {
		return model.ProviderPlaylist{}, e
	}
	var tracks []model.CatalogTrack
	for _, item := range items(nested(d, "tracks")) {
		tracks = append(tracks, parseTrack(item))
	}
	summary := parseSummary(d)
	if summary.TrackCount > len(tracks) {
		return model.ProviderPlaylist{}, fmt.Errorf(
			"Qobuz returned only %d of %d tracks for playlist %s; refusing an incomplete backup",
			len(tracks), summary.TrackCount, id,
		)
	}
	return model.ProviderPlaylist{Summary: summary, Tracks: tracks}, nil
}
func (q *Qobuz) CreatePlaylist(ctx context.Context, name, description string) (model.PlaylistSummary, error) {
	d, e := q.request(ctx, "POST", "playlist/create", url.Values{"name": {name}, "description": {description}, "is_public": {"false"}, "is_collaborative": {"false"}})
	if e != nil {
		return model.PlaylistSummary{}, e
	}
	return parseSummary(d), nil
}
func (q *Qobuz) AddTracks(ctx context.Context, id string, ids []string) (model.ProviderPlaylist, error) {
	if len(ids) > 0 {
		_, e := q.request(ctx, "POST", "playlist/addTracks", url.Values{"playlist_id": {id}, "track_ids": {strings.Join(ids, ",")}, "no_duplicate": {"true"}})
		if e != nil {
			return model.ProviderPlaylist{}, e
		}
	}
	return q.GetPlaylist(ctx, id)
}
func (q *Qobuz) ClearPlaylist(ctx context.Context, id string) (model.ProviderPlaylist, error) {
	p, e := q.GetPlaylist(ctx, id)
	if e != nil {
		return p, e
	}
	var ids []string
	for _, t := range p.Tracks {
		if t.PlaylistItemID == nil {
			return p, fmt.Errorf("Qobuz did not return playlist item ids; refusing to clear safely")
		}
		ids = append(ids, *t.PlaylistItemID)
	}
	if len(ids) > 0 {
		_, e = q.request(ctx, "POST", "playlist/deleteTracks", url.Values{"playlist_id": {id}, "playlist_track_ids": {strings.Join(ids, ",")}})
		if e != nil {
			return p, e
		}
	}
	return q.GetPlaylist(ctx, id)
}

func value(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
func text(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatInt(int64(x), 10)
	default:
		return fmt.Sprint(x)
	}
}
func nested(m map[string]any, key string) map[string]any { v, _ := m[key].(map[string]any); return v }
func items(m map[string]any) []map[string]any {
	raw, _ := m["items"].([]any)
	r := make([]map[string]any, 0, len(raw))
	for _, v := range raw {
		if item, ok := v.(map[string]any); ok {
			r = append(r, item)
		}
	}
	return r
}
func parseSummary(m map[string]any) model.PlaylistSummary {
	id := text(m["id"])
	u := "https://open.qobuz.com/playlist/" + id
	return model.PlaylistSummary{ID: id, Name: or(text(m["name"]), "Untitled"), Description: text(m["description"]), TrackCount: number(m["tracks_count"]), IsPublic: boolean(m["is_public"]), URL: &u}
}
func parseTrack(m map[string]any) model.CatalogTrack {
	album := nested(m, "album")
	artists := artistNames(m)
	albumName := nullable(text(album["title"]))
	year := yearFrom(or(text(album["release_date_original"]), text(album["release_date_stream"])))
	id := text(m["id"])
	u := "https://open.qobuz.com/track/" + id
	return model.CatalogTrack{ID: id, Title: or(text(m["title"]), "Unknown title"), Artists: artists, Album: albumName, ReleaseYear: year, DurationSeconds: intptr(m["duration"]), ISRC: nullable(text(m["isrc"])), Explicit: boolptr(m["parental_warning"]), Version: nullable(text(m["version"])), MaximumBitDepth: intptr(orAny(m["maximum_bit_depth"], album["maximum_bit_depth"])), MaximumSamplingRate: floatptr(orAny(m["maximum_sampling_rate"], album["maximum_sampling_rate"])), PlaylistItemID: nullable(text(m["playlist_track_id"])), URL: &u}
}
func artistNames(m map[string]any) []string {
	seen := map[string]bool{}
	var r []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		k := strings.ToLower(s)
		if s != "" && !seen[k] {
			seen[k] = true
			r = append(r, s)
		}
	}
	for _, key := range []string{"performer", "artist"} {
		if x, ok := m[key].(map[string]any); ok {
			add(text(x["name"]))
		} else {
			add(text(m[key]))
		}
	}
	switch x := m["performers"].(type) {
	case []any:
		for _, v := range x {
			if p, ok := v.(map[string]any); ok {
				add(text(p["name"]))
			} else {
				add(strings.Split(text(v), ",")[0])
			}
		}
	case string:
		for _, v := range strings.Split(x, " - ") {
			add(strings.Split(v, ",")[0])
		}
	}
	if len(r) == 0 {
		return []string{"Unknown artist"}
	}
	return r
}
func or(a, b string) string {
	if a != "" && a != "<nil>" {
		return a
	}
	return b
}
func orAny(a, b any) any {
	if a != nil {
		return a
	}
	return b
}
func number(v any) int {
	if n, ok := v.(float64); ok {
		return int(n)
	}
	return 0
}
func boolean(v any) bool { b, _ := v.(bool); return b }
func nullable(s string) *string {
	if s == "" || s == "<nil>" {
		return nil
	}
	return &s
}
func intptr(v any) *int {
	if n, ok := v.(float64); ok {
		x := int(n)
		return &x
	}
	return nil
}
func floatptr(v any) *float64 {
	if n, ok := v.(float64); ok {
		return &n
	}
	return nil
}
func boolptr(v any) *bool {
	if b, ok := v.(bool); ok {
		return &b
	}
	return nil
}
func yearFrom(s string) *int {
	if len(s) >= 4 {
		if n, e := strconv.Atoi(s[:4]); e == nil {
			return &n
		}
	}
	return nil
}
