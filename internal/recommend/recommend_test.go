package recommend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pawel/qobuz-curator/internal/config"
	"github.com/pawel/qobuz-curator/internal/model"
)

func output(count int) string {
	tracks := make([]map[string]any, count)
	for i := range tracks {
		tracks[i] = map[string]any{
			"position": i + 1, "title": "Track", "artists": []string{"Artist"},
			"album": nil, "release_year": nil, "duration_seconds": nil, "isrc": nil,
			"version_hints": []string{},
		}
	}
	playlist, _ := json.Marshal(map[string]any{
		"schema_version": "1.0", "name": "Generated", "description": "",
		"source_prompt": nil, "tracks": tracks,
	})
	envelope := map[string]any{
		"output": []any{
			map[string]any{
				"content": []any{
					map[string]any{"type": "output_text", "text": string(playlist)},
				},
			},
		},
	}
	raw, _ := json.Marshal(envelope)
	return string(raw)
}

func TestRecommendationFlows(t *testing.T) {
	var requestBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		requestBody = string(raw)
		_, _ = w.Write([]byte(output(2)))
	}))
	defer server.Close()
	c := &Client{Key: "key", Model: "model", Base: server.URL, HTTP: server.Client()}
	ctx := context.Background()
	if _, err := c.Recommend(ctx, "ambient"); err != nil {
		t.Fatal(err)
	}
	current := model.PlaylistInput{SchemaVersion: "1.0", Name: "x", Tracks: []model.TrackRequest{{Position: 1, Title: "t", Artists: []string{"a"}}}}
	if _, err := c.Refine(ctx, current, "quieter"); err != nil {
		t.Fatal(err)
	}
	source := model.ProviderPlaylist{Summary: model.PlaylistSummary{Name: "Reference"}, Tracks: []model.CatalogTrack{{Title: "Says", Artists: []string{"Nils Frahm"}}}}
	result, err := c.FromPlaylists(ctx, []model.ProviderPlaylist{source}, 2, "piano")
	if err != nil || len(result.Tracks) != 2 || !strings.Contains(requestBody, "not a candidate pool") {
		t.Fatal(result, err, requestBody)
	}
	if !strings.Contains(requestBody, `"store":false`) {
		t.Fatal("request should disable OpenAI response storage", requestBody)
	}
}

func TestRecommendationValidationAndErrors(t *testing.T) {
	cfg := config.Config{OpenAIModel: "x", OpenAIAPIBase: "http://x"}
	if _, err := New(cfg).Recommend(context.Background(), "x"); err == nil {
		t.Fatal("key")
	}
	c := &Client{Key: "x"}
	if _, err := c.FromPlaylists(context.Background(), nil, 2, ""); err == nil {
		t.Fatal("sources")
	}
	if _, err := c.FromPlaylists(context.Background(), []model.ProviderPlaylist{{}}, 201, ""); err == nil {
		t.Fatal("count")
	}
	cases := []struct {
		status     int
		body, want string
	}{
		{400, `{"error":{"message":"bad schema"}}`, "bad schema"},
		{200, `{`, "decode OpenAI response"},
		{200, `{"output":[]}`, "no playlist"},
		{200, `{"output_text":"{"}`, "decode OpenAI playlist"},
		{200, `{"output_text":"{\"schema_version\":\"1.0\",\"name\":\"x\",\"tracks\":[]}"}`, "tracks must"},
		{200, `{"status":"failed","error":{"message":"model failed"}}`, "model failed"},
		{200, `{"status":"failed"}`, "request failed"},
		{200, `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}`, "max_output_tokens"},
		{200, `{"status":"incomplete"}`, "unknown reason"},
	}
	for _, tc := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		client := &Client{Key: "key", Model: "m", Base: server.URL, HTTP: server.Client()}
		_, err := client.Recommend(context.Background(), "x")
		server.Close()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", tc.want, err)
		}
	}
}

func TestInputAndSizeLimits(t *testing.T) {
	c := &Client{Key: "key", Model: "m", Base: "http://127.0.0.1:1", HTTP: &http.Client{}}
	if _, err := c.Recommend(context.Background(), " "); err == nil {
		t.Fatal("blank prompt")
	}
	if _, err := c.Recommend(context.Background(), strings.Repeat("x", maxPromptBytes+1)); err == nil {
		t.Fatal("long prompt")
	}
	playlist := model.PlaylistInput{SchemaVersion: "1.0", Name: "x", Tracks: []model.TrackRequest{{Position: 1, Title: "t", Artists: []string{"a"}}}}
	if _, err := c.Refine(context.Background(), playlist, " "); err == nil {
		t.Fatal("blank refinement")
	}
	if _, err := c.Refine(context.Background(), playlist, strings.Repeat("x", maxPromptBytes+1)); err == nil {
		t.Fatal("long refinement")
	}
	manyLists := make([]model.ProviderPlaylist, maxSourceLists+1)
	if _, err := c.FromPlaylists(context.Background(), manyLists, 1, ""); err == nil {
		t.Fatal("too many playlists")
	}
	manyTracks := []model.ProviderPlaylist{{Tracks: make([]model.CatalogTrack, maxSourceTracks+1)}}
	if _, err := c.FromPlaylists(context.Background(), manyTracks, 1, ""); err == nil {
		t.Fatal("too many tracks")
	}
	tooMuchMetadata := []model.ProviderPlaylist{{Tracks: []model.CatalogTrack{{Title: strings.Repeat("x", maxSourceBytes+1)}}}}
	if _, err := c.FromPlaylists(context.Background(), tooMuchMetadata, 1, ""); err == nil {
		t.Fatal("oversized source metadata")
	}
	if safeError([]byte("not-json")) != "request rejected" {
		t.Fatal("unsafe error fallback")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestResponseReader(t *testing.T) {
	if _, err := readBounded(failingReader{}, 10); err == nil {
		t.Fatal("read error")
	}
	if _, err := readBounded(strings.NewReader(strings.Repeat("x", 11)), 10); err == nil {
		t.Fatal("oversized response")
	}
}

func TestCountMismatchAndPrompt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(output(1))) }))
	defer server.Close()
	c := &Client{Key: "k", Model: "m", Base: server.URL, HTTP: server.Client()}
	source := []model.ProviderPlaylist{{Summary: model.PlaylistSummary{Name: "x"}}}
	if _, err := c.FromPlaylists(context.Background(), source, 2, ""); err == nil || !strings.Contains(err.Error(), "instead") {
		t.Fatal(err)
	}
	p := model.PlaylistInput{SchemaVersion: "1.0", Name: "x", Tracks: []model.TrackRequest{{Position: 1, Title: "t", Artists: []string{"a"}}}}
	if !strings.Contains(ChatGPTRefinementPrompt(p), "Current playlist JSON") {
		t.Fatal("prompt")
	}
}
