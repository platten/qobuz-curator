package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pawel/qobuz-curator/internal/config"
)

func TestFakeLifecycle(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	tracks, _ := f.SearchTracks(ctx, "Miles Davis So What", 10)
	if len(tracks) == 0 || tracks[0].ID != "demo-1" {
		t.Fatal(tracks)
	}
	if limited, _ := f.SearchTracks(ctx, "Miles Davis", 1); len(limited) != 1 {
		t.Fatal(limited)
	}
	summary, e := f.CreatePlaylist(ctx, "Test", "Desc")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.AddTracks(ctx, summary.ID, []string{"demo-1", "demo-1"}); e != nil {
		t.Fatal(e)
	}
	p, e := f.GetPlaylist(ctx, summary.ID)
	if e != nil || len(p.Tracks) != 1 {
		t.Fatal(p, e)
	}
	list, _ := f.ListPlaylists(ctx)
	if len(list) != 1 || list[0].TrackCount != 1 {
		t.Fatal(list)
	}
	p, e = f.ClearPlaylist(ctx, summary.ID)
	if e != nil || len(p.Tracks) != 0 {
		t.Fatal(p, e)
	}
	for _, call := range []func() error{func() error { _, e := f.GetPlaylist(ctx, "bad"); return e }, func() error { _, e := f.AddTracks(ctx, "bad", nil); return e }, func() error { _, e := f.ClearPlaylist(ctx, "bad"); return e }, func() error { _, e := f.AddTracks(ctx, summary.ID, []string{"bad"}); return e }} {
		if call() == nil {
			t.Fatal("expected error")
		}
	}
}

func TestQobuzAPI(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user/get", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"id":42}`)) })
	mux.HandleFunc("/catalog/search", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"tracks":{"items":[{"id":1,"title":"Song","performer":{"name":"Artist"},"performers":"Artist, MainArtist - Guest, FeaturedArtist","duration":123,"isrc":"ABC","parental_warning":true,"maximum_bit_depth":24,"maximum_sampling_rate":96,"album":{"title":"Album","release_date_original":"2020-01-01"}}]}}`))
	})
	mux.HandleFunc("/playlist/getUserPlaylists", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"playlists":{"items":[{"id":9,"name":"Mine","tracks_count":1}]}}`))
	})
	mux.HandleFunc("/playlist/get", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":9,"name":"Mine","tracks_count":1,"tracks":{"items":[{"id":1,"title":"Song","artist":"Artist","playlist_track_id":88,"album":{"title":"Album"}}]}}`))
	})
	mux.HandleFunc("/playlist/create", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"id":10,"name":"Created"}`)) })
	mux.HandleFunc("/playlist/addTracks", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) })
	mux.HandleFunc("/playlist/deleteTracks", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) })
	server := httptest.NewServer(mux)
	defer server.Close()
	q := &Qobuz{Base: server.URL, AppID: "a", Token: "t", Client: server.Client()}
	ctx := context.Background()
	tracks, e := q.SearchTracks(ctx, "song", 10)
	if e != nil || len(tracks) != 1 || tracks[0].Artists[0] != "Artist" || tracks[0].ReleaseYear == nil {
		t.Fatal(tracks, e)
	}
	lists, e := q.ListPlaylists(ctx)
	if e != nil || len(lists) != 1 {
		t.Fatal(lists, e)
	}
	if _, e = q.ListPlaylists(ctx); e != nil {
		t.Fatal(e)
	}
	p, e := q.GetPlaylist(ctx, "9")
	if e != nil || len(p.Tracks) != 1 {
		t.Fatal(p, e)
	}
	created, e := q.CreatePlaylist(ctx, "Created", "")
	if e != nil || created.ID != "10" {
		t.Fatal(created, e)
	}
	if _, e = q.AddTracks(ctx, "9", []string{"1"}); e != nil {
		t.Fatal(e)
	}
	if _, e = q.AddTracks(ctx, "9", nil); e != nil {
		t.Fatal(e)
	}
	if _, e = q.ClearPlaylist(ctx, "9"); e != nil {
		t.Fatal(e)
	}
	if _, ok := New(config.Config{Provider: "fake"}).(*Fake); !ok {
		t.Fatal("factory")
	}
	if _, ok := New(config.Config{Provider: "qobuz", QobuzAPIBase: server.URL}).(*Qobuz); !ok {
		t.Fatal("Qobuz factory")
	}
}

func TestQobuzErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/badjson":
			w.Write([]byte("{"))
		case "/status":
			w.Write([]byte(`{"status":"error","message":"no"}`))
		case "/playlist/get":
			count := 1
			if r.URL.Query().Get("playlist_id") == "incomplete" {
				count = 2
			}
			_, _ = w.Write([]byte(fmt.Sprintf(`{"id":1,"name":"x","tracks_count":%d,"tracks":{"items":[{"id":2,"title":"t"}]}}`, count)))
		default:
			http.Error(w, "failure", 500)
		}
	}))
	defer server.Close()
	q := &Qobuz{Base: server.URL, AppID: "a", Token: "t", Client: server.Client()}
	for _, path := range []string{"badjson", "status", "missing"} {
		if _, e := q.request(context.Background(), "GET", path, nil); e == nil {
			t.Fatal(path)
		}
	}
	if _, e := q.ClearPlaylist(context.Background(), "1"); e == nil || !strings.Contains(e.Error(), "item ids") {
		t.Fatal(e)
	}
	if _, e := q.GetPlaylist(context.Background(), "incomplete"); e == nil || !strings.Contains(e.Error(), "incomplete backup") {
		t.Fatal(e)
	}
	if _, e := (&Qobuz{Base: ":", Client: server.Client()}).request(context.Background(), "GET", "x", nil); e == nil {
		t.Fatal("invalid request URL")
	}
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	client := closed.Client()
	closed.Close()
	if _, e := (&Qobuz{Base: closed.URL, Client: client}).request(context.Background(), "GET", "x", nil); e == nil {
		t.Fatal("transport error")
	}
	failing := &Qobuz{Base: closed.URL, Client: client}
	for name, call := range map[string]func() error{
		"search":    func() error { _, err := failing.SearchTracks(context.Background(), "x", 1); return err },
		"list-user": func() error { _, err := failing.ListPlaylists(context.Background()); return err },
		"get":       func() error { _, err := failing.GetPlaylist(context.Background(), "1"); return err },
		"create":    func() error { _, err := failing.CreatePlaylist(context.Background(), "x", ""); return err },
		"add":       func() error { _, err := failing.AddTracks(context.Background(), "1", []string{"1"}); return err },
		"clear":     func() error { _, err := failing.ClearPlaylist(context.Background(), "1"); return err },
	} {
		if call() == nil {
			t.Fatal(name)
		}
	}
	failing.userID = "known"
	if _, err := failing.ListPlaylists(context.Background()); err == nil {
		t.Fatal("list playlists failure")
	}
}

type providerFailReader struct{}

func (providerFailReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestProviderHelpers(t *testing.T) {
	if _, err := readBounded(providerFailReader{}, 10); err == nil {
		t.Fatal("read error")
	}
	if _, err := readBounded(strings.NewReader(strings.Repeat("x", 11)), 10); err == nil {
		t.Fatal("size limit")
	}
	m := map[string]any{
		"id": 1.0, "title": "Track", "performer": "Solo, Main",
		"performers":        []any{map[string]any{"name": "Guest"}, "Other, role"},
		"album":             map[string]any{"release_date_stream": "bad"},
		"maximum_bit_depth": "unknown", "parental_warning": false,
	}
	track := parseTrack(m)
	if track.ID != "1" || len(track.Artists) < 2 || track.ReleaseYear != nil || track.Explicit == nil {
		t.Fatal(track)
	}
	if got := text(true); got != "true" {
		t.Fatal(got)
	}
	if value(nil) != "" || number("bad") != 0 || boolean("bad") || nullable("") != nil || intptr("bad") != nil || floatptr("bad") != nil || boolptr("bad") != nil || yearFrom("20") != nil {
		t.Fatal("helper fallback")
	}
}
