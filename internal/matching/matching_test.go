package matching

import (
	"strings"
	"testing"

	"github.com/pawel/qobuz-curator/internal/model"
)

func ptr[T any](v T) *T { return &v }
func TestNormalizeSimilarity(t *testing.T) {
	if Normalize("Miles & Davis feat. X") != "miles and davis x" {
		t.Fatal(Normalize("Miles & Davis feat. X"))
	}
	if Similarity("same", "same") != 1 || Similarity("", "x") != 0 || Similarity("kitten", "sitting") <= 0 {
		t.Fatal("similarity")
	}
}
func TestScoreAndChoose(t *testing.T) {
	req := model.TrackRequest{Position: 1, Title: "Take Five", Artists: []string{"Dave Brubeck"}, Album: ptr("Time Out"), ReleaseYear: ptr(1959), DurationSeconds: ptr(324), ISRC: ptr("ABC123"), VersionHints: []string{"studio"}}
	good := model.CatalogTrack{ID: "good", Title: "Take Five", Artists: []string{"Dave Brubeck"}, Album: ptr("Time Out"), ReleaseYear: ptr(1959), DurationSeconds: ptr(324), ISRC: ptr("ABC123"), Explicit: ptr(true), MaximumBitDepth: ptr(24), MaximumSamplingRate: ptr(192.0)}
	live := good
	live.ID = "live"
	live.Title = "Take Five Live"
	live.Version = ptr("live")
	live.ISRC = ptr("OTHER")
	score, why := ScoreCandidate(req, good)
	liveScore, _ := ScoreCandidate(req, live)
	if score <= liveScore || score < .98 || !strings.Contains(strings.Join(why, " "), "exact ISRC") {
		t.Fatalf("scores %f %f %v", score, liveScore, why)
	}
	m := Choose(req, []model.CatalogTrack{live, good}, .7)
	if m.Status != "matched" || m.Candidate.ID != "good" {
		t.Fatalf("%#v", m)
	}
	if Choose(req, nil, .7).Status != "skipped" || Choose(req, []model.CatalogTrack{live}, .99).Status != "skipped" {
		t.Fatal("skip")
	}
}
