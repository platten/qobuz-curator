// Package model contains the provider-independent application contracts.
package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// TrackRequest describes a desired recording without depending on a music
// service's catalog identifiers.
type TrackRequest struct {
	Position        int      `json:"position"`
	Title           string   `json:"title"`
	Artists         []string `json:"artists"`
	Album           *string  `json:"album"`
	ReleaseYear     *int     `json:"release_year"`
	DurationSeconds *int     `json:"duration_seconds"`
	ISRC            *string  `json:"isrc"`
	VersionHints    []string `json:"version_hints"`
}

// PlaylistInput is the validated, versioned interchange format accepted from
// ChatGPT, OpenAI, and uploaded JSON files.
type PlaylistInput struct {
	SchemaVersion string         `json:"schema_version"`
	Name          string         `json:"name"`
	Description   string         `json:"description"`
	SourcePrompt  *string        `json:"source_prompt"`
	Tracks        []TrackRequest `json:"tracks"`
}

// DecodePlaylist rejects unknown fields and trailing JSON before applying the
// same semantic checks used for generated playlists.
func DecodePlaylist(raw []byte) (PlaylistInput, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var playlist PlaylistInput
	if err := decoder.Decode(&playlist); err != nil {
		return playlist, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return playlist, fmt.Errorf("playlist JSON must contain exactly one object")
		}
		return playlist, fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return playlist, playlist.Validate()
}

// Validate normalizes harmless whitespace and rejects inputs that do not meet
// the documented playlist-v1 bounds.
func (p *PlaylistInput) Validate() error {
	p.Name = strings.TrimSpace(p.Name)
	if p.SchemaVersion != "1.0" || p.Name == "" {
		return fmt.Errorf("schema_version must be 1.0 and name cannot be blank")
	}
	if utf8.RuneCountInString(p.Name) > 250 {
		return fmt.Errorf("name must not exceed 250 characters")
	}
	if utf8.RuneCountInString(p.Description) > 2000 {
		return fmt.Errorf("description must not exceed 2000 characters")
	}
	if p.SourcePrompt != nil && utf8.RuneCountInString(*p.SourcePrompt) > 10_000 {
		return fmt.Errorf("source_prompt must not exceed 10000 characters")
	}
	if len(p.Tracks) < 1 || len(p.Tracks) > 200 {
		return fmt.Errorf("tracks must contain between 1 and 200 items")
	}
	seen := map[int]bool{}
	for i := range p.Tracks {
		t := &p.Tracks[i]
		t.Title = strings.TrimSpace(t.Title)
		if t.Position < 1 || t.Title == "" || len(t.Artists) == 0 || len(t.Artists) > 20 || seen[t.Position] {
			return fmt.Errorf("track %d has an invalid position, title, artists, or duplicate position", i+1)
		}
		if utf8.RuneCountInString(t.Title) > 500 {
			return fmt.Errorf("track %d title must not exceed 500 characters", i+1)
		}
		seen[t.Position] = true
		clean := t.Artists[:0]
		for _, artist := range t.Artists {
			if value := strings.TrimSpace(artist); value != "" {
				if utf8.RuneCountInString(value) > 500 {
					return fmt.Errorf("track %d artist must not exceed 500 characters", i+1)
				}
				clean = append(clean, value)
			}
		}
		if len(clean) == 0 {
			return fmt.Errorf("track %d requires a non-blank artist", i+1)
		}
		t.Artists = clean
		if t.Album != nil {
			album := strings.TrimSpace(*t.Album)
			if utf8.RuneCountInString(album) > 500 {
				return fmt.Errorf("track %d album must not exceed 500 characters", i+1)
			}
			if album == "" {
				t.Album = nil
			} else {
				t.Album = &album
			}
		}
		if t.ReleaseYear != nil && (*t.ReleaseYear < 1900 || *t.ReleaseYear > 2200) {
			return fmt.Errorf("track %d release_year must be between 1900 and 2200", i+1)
		}
		if t.DurationSeconds != nil && (*t.DurationSeconds < 1 || *t.DurationSeconds > 7200) {
			return fmt.Errorf("track %d duration_seconds must be between 1 and 7200", i+1)
		}
		if t.ISRC != nil {
			value := strings.ToUpper(strings.ReplaceAll(*t.ISRC, "-", ""))
			if len(value) < 5 || len(value) > 32 {
				return fmt.Errorf("track %d ISRC must contain between 5 and 32 characters", i+1)
			}
			t.ISRC = &value
		}
		if t.VersionHints == nil {
			t.VersionHints = []string{}
		}
		if len(t.VersionHints) > 20 {
			return fmt.Errorf("track %d has more than 20 version hints", i+1)
		}
		for j := range t.VersionHints {
			t.VersionHints[j] = strings.TrimSpace(t.VersionHints[j])
			if utf8.RuneCountInString(t.VersionHints[j]) > 200 {
				return fmt.Errorf("track %d version hint must not exceed 200 characters", i+1)
			}
		}
	}
	sort.Slice(p.Tracks, func(i, j int) bool { return p.Tracks[i].Position < p.Tracks[j].Position })
	return nil
}

// CatalogTrack is a provider-resolved recording candidate.
type CatalogTrack struct {
	ID                  string   `json:"id"`
	Title               string   `json:"title"`
	Artists             []string `json:"artists"`
	Album               *string  `json:"album"`
	ReleaseYear         *int     `json:"release_year"`
	DurationSeconds     *int     `json:"duration_seconds"`
	ISRC                *string  `json:"isrc"`
	Explicit            *bool    `json:"explicit"`
	Version             *string  `json:"version"`
	MaximumBitDepth     *int     `json:"maximum_bit_depth"`
	MaximumSamplingRate *float64 `json:"maximum_sampling_rate"`
	PlaylistItemID      *string  `json:"playlist_item_id"`
	URL                 *string  `json:"url"`
}

// MatchResult records the deterministic decision for one requested track.
type MatchResult struct {
	Request     TrackRequest  `json:"request"`
	Candidate   *CatalogTrack `json:"candidate"`
	Score       float64       `json:"score"`
	Explanation []string      `json:"explanation"`
	Status      string        `json:"status"`
}

// Preview is an immutable, expiring set of catalog matches.
type Preview struct {
	ID        string        `json:"id"`
	Playlist  PlaylistInput `json:"playlist"`
	Matches   []MatchResult `json:"matches"`
	CreatedAt time.Time     `json:"created_at"`
	ExpiresAt time.Time     `json:"expires_at"`
}

func (p Preview) MatchedCount() int { return len(p.TrackIDs()) }
func (p Preview) SkippedCount() int { return len(p.Matches) - p.MatchedCount() }
func (p Preview) TrackIDs() []string {
	var ids []string
	for _, match := range p.Matches {
		if match.Status == "matched" && match.Candidate != nil {
			ids = append(ids, match.Candidate.ID)
		}
	}
	return ids
}

// PlaylistSummary contains provider playlist metadata without its tracks.
type PlaylistSummary struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	TrackCount  int     `json:"track_count"`
	IsPublic    bool    `json:"is_public"`
	URL         *string `json:"url"`
}

// ProviderPlaylist is a complete provider playlist used for taste analysis and
// rollback backups.
type ProviderPlaylist struct {
	Summary PlaylistSummary `json:"summary"`
	Tracks  []CatalogTrack  `json:"tracks"`
}

// Operation is the durable audit record for a remote playlist mutation.
type Operation struct {
	ID                    string         `json:"id"`
	PreviewID             string         `json:"preview_id"`
	Mode                  string         `json:"mode"`
	Status                string         `json:"status"`
	DestinationPlaylistID *string        `json:"destination_playlist_id"`
	DestinationName       string         `json:"destination_name"`
	CreatedAt             time.Time      `json:"created_at"`
	CompletedAt           *time.Time     `json:"completed_at"`
	Result                map[string]any `json:"result"`
	Error                 *string        `json:"error"`
}
