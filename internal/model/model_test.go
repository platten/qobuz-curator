package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func validPlaylist() PlaylistInput {
	return PlaylistInput{SchemaVersion: "1.0", Name: " Test ", Tracks: []TrackRequest{{Position: 2, Title: " Second ", Artists: []string{" Artist "}, VersionHints: nil}, {Position: 1, Title: "First", Artists: []string{"One"}}}}
}
func TestPlaylistValidation(t *testing.T) {
	p := validPlaylist()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if p.Tracks[0].Position != 1 || p.Tracks[1].Title != "Second" || p.Tracks[1].Artists[0] != "Artist" || p.Tracks[1].VersionHints == nil {
		t.Fatalf("not normalized: %#v", p)
	}
	bad := []PlaylistInput{{SchemaVersion: "2", Name: "x", Tracks: p.Tracks}, {SchemaVersion: "1.0", Name: " ", Tracks: p.Tracks}, {SchemaVersion: "1.0", Name: "x"}, {SchemaVersion: "1.0", Name: "x", Tracks: []TrackRequest{{Position: 1, Title: "", Artists: []string{"x"}}}}, {SchemaVersion: "1.0", Name: "x", Tracks: []TrackRequest{{Position: 1, Title: "x", Artists: []string{" "}}}}, {SchemaVersion: "1.0", Name: "x", Tracks: []TrackRequest{{Position: 1, Title: "x", Artists: []string{"x"}}, {Position: 1, Title: "y", Artists: []string{"y"}}}}}
	for i, p := range bad {
		if p.Validate() == nil {
			t.Errorf("case %d should fail", i)
		}
	}
}

func TestDecodePlaylist(t *testing.T) {
	raw, err := json.Marshal(validPlaylist())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := DecodePlaylist(raw); err != nil || got.Name != "Test" {
		t.Fatal(got, err)
	}
	for _, raw := range [][]byte{
		[]byte(`{"schema_version":"1.0","name":"x","tracks":[],"unknown":true}`),
		append(append([]byte(nil), raw...), []byte(` {}`)...),
		[]byte(`{`),
	} {
		if _, err := DecodePlaylist(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestPlaylistBounds(t *testing.T) {
	year, duration := 2020, 120
	isrc, album, source := "US-SM1-59-00101", "Album", "source"
	base := PlaylistInput{SchemaVersion: "1.0", Name: "x", SourcePrompt: &source, Tracks: []TrackRequest{{Position: 1, Title: "t", Artists: []string{"a"}, Album: &album, ReleaseYear: &year, DurationSeconds: &duration, ISRC: &isrc}}}
	if err := base.Validate(); err != nil || *base.Tracks[0].ISRC != "USSM15900101" {
		t.Fatal(base, err)
	}
	mutations := []func(*PlaylistInput){
		func(p *PlaylistInput) { p.Name = strings.Repeat("n", 251) },
		func(p *PlaylistInput) { p.Description = strings.Repeat("d", 2001) },
		func(p *PlaylistInput) { value := strings.Repeat("s", 10001); p.SourcePrompt = &value },
		func(p *PlaylistInput) { p.Tracks[0].Title = strings.Repeat("t", 501) },
		func(p *PlaylistInput) { p.Tracks[0].Artists = make([]string, 21) },
		func(p *PlaylistInput) { p.Tracks[0].Artists = []string{strings.Repeat("a", 501)} },
		func(p *PlaylistInput) { value := strings.Repeat("a", 501); p.Tracks[0].Album = &value },
		func(p *PlaylistInput) { value := 1899; p.Tracks[0].ReleaseYear = &value },
		func(p *PlaylistInput) { value := 7201; p.Tracks[0].DurationSeconds = &value },
		func(p *PlaylistInput) { value := "x"; p.Tracks[0].ISRC = &value },
		func(p *PlaylistInput) { p.Tracks[0].VersionHints = make([]string, 21) },
		func(p *PlaylistInput) { p.Tracks[0].VersionHints = []string{strings.Repeat("v", 201)} },
	}
	for i, mutate := range mutations {
		candidate := base
		candidate.Tracks = append([]TrackRequest(nil), base.Tracks...)
		mutate(&candidate)
		if candidate.Validate() == nil {
			t.Errorf("mutation %d should fail", i)
		}
	}
	blankAlbum := base
	blankAlbum.Tracks = append([]TrackRequest(nil), base.Tracks...)
	value := "  "
	blankAlbum.Tracks[0].Album = &value
	if err := blankAlbum.Validate(); err != nil || blankAlbum.Tracks[0].Album != nil {
		t.Fatal("blank album was not normalized", err)
	}
}
func TestPreviewCounts(t *testing.T) {
	p := Preview{Matches: []MatchResult{{Status: "matched", Candidate: &CatalogTrack{ID: "1"}}, {Status: "skipped"}, {Status: "matched"}}}
	if p.MatchedCount() != 1 || p.SkippedCount() != 2 || len(p.TrackIDs()) != 1 {
		t.Fatal("wrong counts")
	}
	_ = time.Now()
}
