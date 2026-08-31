// Package recommend produces validated playlist inputs from written prompts.
package recommend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pawel/qobuz-curator/internal/config"
	"github.com/pawel/qobuz-curator/internal/httpretry"
	"github.com/pawel/qobuz-curator/internal/model"
	"go.uber.org/zap"
)

// Recommender creates complete service-neutral playlist inputs.
type Recommender interface {
	Recommend(context.Context, string) (model.PlaylistInput, error)
	Refine(context.Context, model.PlaylistInput, string) (model.PlaylistInput, error)
	FromPlaylists(context.Context, []model.ProviderPlaylist, int, string) (model.PlaylistInput, error)
}

// Client is a small Responses API client specialized for strict playlist JSON.
type Client struct {
	Key, Model, Base string
	HTTP             *http.Client
}

const (
	maxResponseBytes       = 8 << 20
	maxPromptBytes         = 10_000
	maxSourceLists         = 10
	maxSourceTracks        = 1_000
	maxSourceBytes         = 2 << 20
	defaultTrackCount      = "20-50"
	openAIOperationTimeout = 110 * time.Second
)

// New builds an OpenAI recommender from application configuration.
func New(cfg config.Config) *Client {
	return &Client{
		Key:   cfg.OpenAIAPIKey,
		Model: cfg.OpenAIModel,
		Base:  strings.TrimRight(cfg.OpenAIAPIBase, "/"),
		HTTP:  &http.Client{Timeout: 90 * time.Second},
	}
}

var playlistSchema = func() map[string]any {
	const raw = `{"type":"object","additionalProperties":false,"required":["schema_version","name","description","source_prompt","tracks"],"properties":{"schema_version":{"type":"string","const":"1.0"},"name":{"type":"string","minLength":1,"maxLength":250},"description":{"type":"string","maxLength":2000},"source_prompt":{"type":["string","null"],"maxLength":10000},"tracks":{"type":"array","minItems":1,"maxItems":200,"items":{"type":"object","additionalProperties":false,"required":["position","title","artists","album","release_year","duration_seconds","isrc","version_hints"],"properties":{"position":{"type":"integer","minimum":1},"title":{"type":"string","minLength":1,"maxLength":500},"artists":{"type":"array","minItems":1,"maxItems":20,"items":{"type":"string","minLength":1,"maxLength":500}},"album":{"type":["string","null"],"maxLength":500},"release_year":{"type":["integer","null"],"minimum":1900,"maximum":2200},"duration_seconds":{"type":["integer","null"],"minimum":1,"maximum":7200},"isrc":{"type":["string","null"],"minLength":5,"maxLength":32},"version_hints":{"type":"array","maxItems":20,"items":{"type":"string","maxLength":200}}}}}}}`
	var schema map[string]any
	_ = json.Unmarshal([]byte(raw), &schema)
	return schema
}()

func (c *Client) generate(ctx context.Context, input, instructions string) (model.PlaylistInput, error) {
	requestContext, cancel := context.WithTimeout(ctx, openAIOperationTimeout)
	defer cancel()
	if c.Key == "" {
		return model.PlaylistInput{}, fmt.Errorf("OPENAI_API_KEY is not configured")
	}
	body := map[string]any{
		"model":        c.Model,
		"instructions": instructions,
		"input":        input,
		// Recommendations can contain personal taste data. The application does
		// not need server-side response retention to operate.
		"store": false,
		"text": map[string]any{"format": map[string]any{
			"type": "json_schema", "name": "qobuz_playlist", "strict": true, "schema": playlistSchema,
		}},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return model.PlaylistInput{}, fmt.Errorf("encode OpenAI request: %w", err)
	}
	makeRequest := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(requestContext, http.MethodPost, c.Base+"/responses", bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.Key)
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	}
	zap.L().Info("requesting an OpenAI playlist recommendation", zap.String("model", c.Model), zap.Int("request_bytes", len(raw)))
	// The Responses request is safe to repeat because server-side storage is
	// disabled and it performs no Qobuz mutation. Temporary throttling is only
	// retried when the response explicitly provides Retry-After.
	resp, e := httpretry.Do(requestContext, c.HTTP, makeRequest, true, zap.L(), "OpenAI recommendation", httpretry.DefaultPolicy())
	if e != nil {
		return model.PlaylistInput{}, fmt.Errorf("OpenAI recommendation failed: %w", e)
	}
	defer resp.Body.Close()
	responseRaw, e := readBounded(resp.Body, maxResponseBytes)
	if e != nil {
		return model.PlaylistInput{}, fmt.Errorf("read OpenAI response: %w", e)
	}
	if resp.StatusCode/100 != 2 {
		zap.L().Warn("OpenAI recommendation request rejected", zap.Int("http_status", resp.StatusCode), zap.String("request_id", resp.Header.Get("x-request-id")))
		return model.PlaylistInput{}, fmt.Errorf("OpenAI recommendation failed with HTTP %d: %s", resp.StatusCode, safeError(responseRaw))
	}
	var envelope struct {
		OutputText string `json:"output_text"`
		Status     string `json:"status"`
		Error      *struct {
			Message string `json:"message"`
		} `json:"error"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Output []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if e = json.Unmarshal(responseRaw, &envelope); e != nil {
		return model.PlaylistInput{}, fmt.Errorf("decode OpenAI response: %w", e)
	}
	if envelope.Status == "failed" {
		return model.PlaylistInput{}, fmt.Errorf("OpenAI recommendation failed: %s", safeMessage(envelope.Error))
	}
	if envelope.Status == "incomplete" {
		reason := "unknown reason"
		if envelope.IncompleteDetails != nil && envelope.IncompleteDetails.Reason != "" {
			reason = envelope.IncompleteDetails.Reason
		}
		return model.PlaylistInput{}, fmt.Errorf("OpenAI recommendation was incomplete: %s", reason)
	}
	text := envelope.OutputText
	if text == "" {
		for _, out := range envelope.Output {
			for _, content := range out.Content {
				if content.Type == "output_text" {
					text = content.Text
					break
				}
			}
		}
	}
	if text == "" {
		return model.PlaylistInput{}, fmt.Errorf("OpenAI returned no playlist")
	}
	playlist, e := model.DecodePlaylist([]byte(text))
	if e != nil {
		return playlist, fmt.Errorf("decode OpenAI playlist: %w", e)
	}
	zap.L().Info("OpenAI recommendation completed", zap.Int("track_count", len(playlist.Tracks)), zap.String("request_id", resp.Header.Get("x-request-id")))
	return playlist, nil
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

func safeMessage(apiError *struct {
	Message string `json:"message"`
}) string {
	if apiError == nil || strings.TrimSpace(apiError.Message) == "" {
		return "request failed"
	}
	return apiError.Message
}
func safeError(raw []byte) string {
	var v struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &v) == nil && v.Error.Message != "" {
		return v.Error.Message
	}
	return "request rejected"
}

func (c *Client) Recommend(ctx context.Context, prompt string) (model.PlaylistInput, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return model.PlaylistInput{}, fmt.Errorf("recommendation prompt is required")
	}
	if len(prompt) > maxPromptBytes {
		return model.PlaylistInput{}, fmt.Errorf("recommendation prompt exceeds %d bytes", maxPromptBytes)
	}
	return c.generate(ctx, prompt, "Create a coherent music playlist from the user's written request. Return "+defaultTrackCount+" tracks unless the request calls for a shorter set. Never invent ISRCs, service IDs, durations, albums, or years; use null for uncertain nullable fields and an empty list for version hints. Prefer canonical studio recordings unless requested otherwise.")
}
func (c *Client) Refine(ctx context.Context, current model.PlaylistInput, prompt string) (model.PlaylistInput, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return model.PlaylistInput{}, fmt.Errorf("refinement prompt is required")
	}
	if len(prompt) > maxPromptBytes {
		return model.PlaylistInput{}, fmt.Errorf("refinement prompt exceeds %d bytes", maxPromptBytes)
	}
	raw, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return model.PlaylistInput{}, fmt.Errorf("encode current playlist: %w", err)
	}
	return c.generate(ctx, "CURRENT PLAYLIST JSON\n"+string(raw)+"\n\nREFINEMENT REQUEST\n"+prompt, "Revise the supplied current playlist according to the refinement request. Preserve good tracks and ordering that the request does not affect. Return a complete replacement playlist, not a patch. Keep positions unique and sequential from one. Never invent uncertain metadata.")
}
func (c *Client) FromPlaylists(ctx context.Context, playlists []model.ProviderPlaylist, count int, direction string) (model.PlaylistInput, error) {
	if len(playlists) == 0 {
		return model.PlaylistInput{}, fmt.Errorf("select at least one source playlist")
	}
	if len(playlists) > maxSourceLists {
		return model.PlaylistInput{}, fmt.Errorf("select no more than %d source playlists", maxSourceLists)
	}
	if count < 1 || count > 200 {
		return model.PlaylistInput{}, fmt.Errorf("track count must be between 1 and 200")
	}
	type sourceTrack struct {
		Title       string   `json:"title"`
		Artists     []string `json:"artists"`
		Album       *string  `json:"album"`
		ReleaseYear *int     `json:"release_year"`
		Version     *string  `json:"version"`
	}
	type source struct {
		Name, Description string
		Tracks            []sourceTrack
	}
	sources := make([]source, 0, len(playlists))
	totalTracks := 0
	for _, p := range playlists {
		totalTracks += len(p.Tracks)
		if totalTracks > maxSourceTracks {
			return model.PlaylistInput{}, fmt.Errorf("source playlists contain more than %d tracks", maxSourceTracks)
		}
		s := source{Name: p.Summary.Name, Description: p.Summary.Description}
		for _, t := range p.Tracks {
			s.Tracks = append(s.Tracks, sourceTrack{t.Title, t.Artists, t.Album, t.ReleaseYear, t.Version})
		}
		sources = append(sources, s)
	}
	raw, err := json.MarshalIndent(sources, "", "  ")
	if err != nil {
		return model.PlaylistInput{}, fmt.Errorf("encode source playlists: %w", err)
	}
	if len(raw) > maxSourceBytes {
		return model.PlaylistInput{}, fmt.Errorf("source playlist metadata exceeds %d bytes", maxSourceBytes)
	}
	input := fmt.Sprintf("SOURCE PLAYLISTS\n%s\n\nREQUESTED TRACK COUNT\n%d\n\nADDITIONAL DIRECTION\n%s", raw, count, or(direction, "None"))
	result, e := c.generate(ctx, input, "Analyze the supplied playlists as musical references. Infer styles, moods, eras, instrumentation, energy, sequencing, and artist relationships, then create a coherent new recommendation. The source tracks are evidence about taste, not a candidate pool: explore beyond them and do not simply copy or combine the source playlists. Return exactly the requested number of tracks with sequential positions. Prefer canonical studio recordings unless requested otherwise and never invent uncertain metadata.")
	if e == nil && len(result.Tracks) != count {
		return result, fmt.Errorf("OpenAI returned %d tracks instead of the requested %d", len(result.Tracks), count)
	}
	return result, e
}
func or(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return strings.TrimSpace(a)
	}
	return b
}

// ChatGPTRefinementPrompt creates the manual ChatGPT fallback prompt shown in
// the preview UI.
func ChatGPTRefinementPrompt(p model.PlaylistInput) string {
	raw, _ := json.MarshalIndent(p, "", "  ")
	return "Refine the Qobuz Curator playlist below according to my next instructions. Preserve good tracks and ordering that are not affected. Return only a complete JSON object conforming to playlist-v1.schema.json, not commentary or a patch. Never invent uncertain metadata.\n\nMy refinement instructions: [describe the changes]\n\nCurrent playlist JSON:\n" + string(raw)
}
