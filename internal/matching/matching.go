// Package matching scores catalog candidates against requested recordings.
package matching

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/pawel/qobuz-curator/internal/model"
)

var nonAlphanumeric = regexp.MustCompile(`[^a-z0-9]+`)
var qualifiers = []string{"live", "remix", "remastered", "remaster", "radio edit", "clean", "explicit", "instrumental", "karaoke", "tribute", "acoustic", "demo", "edit"}

// Normalize removes punctuation and common featuring syntax for comparisons.
func Normalize(value string) string {
	value = strings.ToLower(strings.ReplaceAll(value, "&", " and "))
	value = strings.ReplaceAll(value, "featuring", " ")
	value = strings.ReplaceAll(value, "feat", " ")
	return strings.Join(strings.Fields(nonAlphanumeric.ReplaceAllString(value, " ")), " ")
}

// Similarity returns a normalized Levenshtein similarity in the range [0, 1].
func Similarity(a, b string) float64 {
	a, b = Normalize(a), Normalize(b)
	if a == b {
		return 1
	}
	if a == "" || b == "" {
		return 0
	}
	ra, rb := []rune(a), []rune(b)
	row := make([]int, len(rb)+1)
	for j := range row {
		row[j] = j
	}
	for i, ca := range ra {
		previous := row[0]
		row[0] = i + 1
		for j, cb := range rb {
			old := row[j+1]
			cost := 1
			if ca == cb {
				cost = 0
			}
			row[j+1] = min(row[j+1]+1, row[j]+1, previous+cost)
			previous = old
		}
	}
	return 1 - float64(row[len(rb)])/float64(max(len(ra), len(rb)))
}

func artistSimilarity(requested, candidate []string) float64 {
	if len(requested) == 0 || len(candidate) == 0 {
		return 0
	}
	total := 0.0
	for _, artist := range requested {
		best := 0.0
		for _, other := range candidate {
			best = math.Max(best, Similarity(artist, other))
		}
		total += best
	}
	return math.Min(1, .75*(total/float64(len(requested)))+.25*Similarity(requested[0], candidate[0]))
}

func detected(values ...string) map[string]bool {
	text := Normalize(strings.Join(values, " "))
	result := map[string]bool{}
	for _, q := range qualifiers {
		if strings.Contains(text, q) {
			result[q] = true
		}
	}
	return result
}

// ScoreCandidate combines identity evidence, version penalties, and a small
// maximum-quality tie-breaker.
func ScoreCandidate(req model.TrackRequest, candidate model.CatalogTrack) (float64, []string) {
	title, artists := Similarity(req.Title, candidate.Title), artistSimilarity(req.Artists, candidate.Artists)
	score := .47*title + .38*artists
	explanation := []string{fmt.Sprintf("title %.2f", title), fmt.Sprintf("artists %.2f", artists)}
	used, optional := 0.0, 0.0
	if req.Album != nil && candidate.Album != nil {
		v := Similarity(*req.Album, *candidate.Album)
		optional += .07 * v
		used += .07
		explanation = append(explanation, fmt.Sprintf("album %.2f", v))
	}
	if req.DurationSeconds != nil && candidate.DurationSeconds != nil {
		d := abs(*req.DurationSeconds - *candidate.DurationSeconds)
		optional += .05 * math.Max(0, 1-float64(d)/30)
		used += .05
		explanation = append(explanation, fmt.Sprintf("duration delta %ds", d))
	}
	if req.ReleaseYear != nil && candidate.ReleaseYear != nil {
		d := abs(*req.ReleaseYear - *candidate.ReleaseYear)
		y := 0.0
		if d == 0 {
			y = 1
		} else if d <= 1 {
			y = .7
		} else if d <= 5 {
			y = .3
		}
		optional += .03 * y
		used += .03
		explanation = append(explanation, fmt.Sprintf("year delta %d", d))
	}
	score += optional + (.15-used)*((title+artists)/2)
	if req.ISRC != nil && candidate.ISRC != nil {
		if *req.ISRC == strings.ToUpper(strings.ReplaceAll(*candidate.ISRC, "-", "")) {
			score = math.Max(score, .985)
			explanation = append(explanation, "exact ISRC")
		} else {
			score -= .12
			explanation = append(explanation, "ISRC mismatch")
		}
	}
	reqValues := []string{req.Title}
	reqValues = append(reqValues, req.VersionHints...)
	want := detected(reqValues...)
	candidateValues := []string{candidate.Title}
	if candidate.Album != nil {
		candidateValues = append(candidateValues, *candidate.Album)
	}
	if candidate.Version != nil {
		candidateValues = append(candidateValues, *candidate.Version)
	}
	strong := map[string]bool{"live": true, "remix": true, "radio edit": true, "clean": true, "instrumental": true, "karaoke": true, "tribute": true}
	var unwanted []string
	for q := range detected(candidateValues...) {
		if !want[q] && strong[q] {
			unwanted = append(unwanted, q)
		}
	}
	sort.Strings(unwanted)
	if len(unwanted) > 0 {
		score -= math.Min(.30, .14*float64(len(unwanted)))
		explanation = append(explanation, "unrequested "+strings.Join(unwanted, ", "))
	}
	if candidate.Explicit != nil && *candidate.Explicit && !want["clean"] {
		score += .006
		explanation = append(explanation, "explicit preferred")
	}
	quality := 0.0
	if candidate.MaximumBitDepth != nil {
		quality += math.Min(float64(*candidate.MaximumBitDepth), 24) / 24
	}
	if candidate.MaximumSamplingRate != nil {
		quality += math.Min(*candidate.MaximumSamplingRate, 192) / 192
	}
	if quality > 0 {
		score += math.Min(.008, quality*.004)
		explanation = append(explanation, "maximum-quality edition preferred")
	}
	return math.Max(0, math.Min(1, score)), explanation
}

// Choose returns the highest-scoring candidate or a documented skipped result.
func Choose(req model.TrackRequest, candidates []model.CatalogTrack, threshold float64) model.MatchResult {
	if len(candidates) == 0 {
		return model.MatchResult{Request: req, Status: "skipped", Explanation: []string{"no Qobuz candidates returned"}}
	}
	type scored struct {
		score float64
		track model.CatalogTrack
		why   []string
	}
	ranked := make([]scored, 0, len(candidates))
	for _, c := range candidates {
		score, why := ScoreCandidate(req, c)
		ranked = append(ranked, scored{score, c, why})
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
	best := ranked[0]
	status := "matched"
	if best.score < threshold {
		status = "skipped"
		best.why = append(best.why, fmt.Sprintf("below threshold %.2f", threshold))
	}
	return model.MatchResult{Request: req, Candidate: &best.track, Score: best.score, Explanation: best.why, Status: status}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
