// Package service orchestrates previews and verified playlist mutations.
package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pawel/qobuz-curator/internal/config"
	"github.com/pawel/qobuz-curator/internal/logging"
	"github.com/pawel/qobuz-curator/internal/matching"
	"github.com/pawel/qobuz-curator/internal/model"
	"github.com/pawel/qobuz-curator/internal/provider"
	"go.uber.org/zap"
)

// Service orchestrates read-only previews and verified provider mutations.
type Service struct {
	Config     config.Config
	Store      Persistence
	Provider   provider.Provider
	mutationMu sync.Mutex
}

// Persistence captures the durable operations needed by the service. Keeping
// it narrow makes failure behavior testable without coupling orchestration to
// a database implementation.
type Persistence interface {
	SavePreview(model.Preview) error
	Preview(string) (model.Preview, error)
	SaveOperation(model.Operation) error
	SaveOperationWithBackup(model.Operation, model.ProviderPlaylist) error
	Operation(string) (model.Operation, error)
	Backup(string) (model.ProviderPlaylist, error)
}

var readRandom = rand.Read

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := readRandom(b); err != nil {
		return "", fmt.Errorf("generate identifier: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Prepare resolves every requested recording against the provider catalog and
// persists an immutable, expiring preview. It never mutates a remote playlist.
func (s *Service) Prepare(ctx context.Context, playlist model.PlaylistInput) (model.Preview, error) {
	if e := playlist.Validate(); e != nil {
		return model.Preview{}, e
	}
	seen := map[string]bool{}
	zap.L().Info("preparing playlist preview", zap.Int("requested_track_count", len(playlist.Tracks)))
	matches := make([]model.MatchResult, 0, len(playlist.Tracks))
	for _, request := range playlist.Tracks {
		queries := []string{request.Artists[0] + " " + request.Title, request.Title + " " + join(request.Artists)}
		if request.Album != nil {
			queries = append(queries, request.Artists[0]+" "+request.Title+" "+*request.Album)
		}
		var candidates []model.CatalogTrack
		candidateIDs := map[string]bool{}
		for _, query := range queries {
			found, e := s.Provider.SearchTracks(ctx, query, 10)
			if e != nil {
				return model.Preview{}, e
			}
			for _, candidate := range found {
				if !candidateIDs[candidate.ID] {
					candidateIDs[candidate.ID] = true
					candidates = append(candidates, candidate)
				}
			}
		}
		match := matching.Choose(request, candidates, s.Config.MatchThreshold)
		if match.Status == "matched" && match.Candidate != nil {
			if seen[match.Candidate.ID] {
				match.Status = "skipped"
				match.Explanation = append(match.Explanation, "duplicate Qobuz track")
			} else {
				seen[match.Candidate.ID] = true
			}
		}
		matches = append(matches, match)
	}
	now := time.Now().UTC()
	id, err := newID()
	if err != nil {
		return model.Preview{}, err
	}
	preview := model.Preview{ID: id, Playlist: playlist, Matches: matches, CreatedAt: now, ExpiresAt: now.Add(time.Duration(s.Config.PreviewTTLHours) * time.Hour)}
	if err := s.Store.SavePreview(preview); err != nil {
		return preview, err
	}
	zap.L().Info("playlist preview saved", zap.String("preview_id", preview.ID), zap.Int("matched_count", preview.MatchedCount()), zap.Int("skipped_count", preview.SkippedCount()))
	return preview, nil
}
func join(v []string) string {
	r := ""
	for i, s := range v {
		if i > 0 {
			r += " "
		}
		r += s
	}
	return r
}
func (s *Service) Preview(id string) (model.Preview, error) {
	p, e := s.Store.Preview(id)
	if e == nil && p.ExpiresAt.Before(time.Now().UTC()) {
		return p, fmt.Errorf("preview %s has expired", id)
	}
	return p, e
}
func newOperation(preview model.Preview, mode, name string) (*model.Operation, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	return &model.Operation{ID: id, PreviewID: preview.ID, Mode: mode, Status: "pending", DestinationName: name, CreatedAt: time.Now().UTC(), Result: map[string]any{}}, nil
}
func finish(s Persistence, o *model.Operation) error {
	now := time.Now().UTC()
	o.CompletedAt = &now
	return s.SaveOperation(*o)
}
func fail(o *model.Operation, e error) { message := e.Error(); o.Error = &message }
func verify(actual []model.CatalogTrack, expected []string) error {
	if len(actual) != len(expected) {
		return fmt.Errorf("qobuz verification failed: final track order differs")
	}
	for i, t := range actual {
		if t.ID != expected[i] {
			return fmt.Errorf("qobuz verification failed: final track order differs")
		}
	}
	return nil
}
func (s *Service) Create(ctx context.Context, id, name string, confirmed bool) (model.Operation, error) {
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	if !confirmed {
		return model.Operation{}, fmt.Errorf("explicit confirmation is required")
	}
	p, e := s.Preview(id)
	if e != nil {
		return model.Operation{}, e
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return model.Operation{}, fmt.Errorf("playlist name is required")
	}
	if len(p.TrackIDs()) == 0 {
		return model.Operation{}, fmt.Errorf("preview contains no matched tracks")
	}
	o, e := newOperation(p, "create_new", name)
	if e != nil {
		return model.Operation{}, e
	}
	if e = s.Store.SaveOperation(*o); e != nil {
		return *o, fmt.Errorf("save pending operation: %w", e)
	}
	zap.L().Info("creating Qobuz playlist", zap.String("operation_id", o.ID), zap.Int("track_count", len(p.TrackIDs())))
	summary, e := s.Provider.CreatePlaylist(ctx, name, p.Playlist.Description)
	if e == nil {
		var final model.ProviderPlaylist
		final, e = s.Provider.AddTracks(ctx, summary.ID, p.TrackIDs())
		if e == nil {
			e = verify(final.Tracks, p.TrackIDs())
		}
		if e == nil {
			o.DestinationPlaylistID = &summary.ID
			o.Status = "succeeded"
			o.Result = map[string]any{"playlist_id": summary.ID, "playlist_url": summary.URL, "track_count": len(final.Tracks), "skipped_count": p.SkippedCount()}
		}
	}
	if e != nil {
		o.Status = "failed"
		fail(o, e)
	}
	if e = finish(s.Store, o); e != nil {
		return *o, fmt.Errorf("save completed operation: %w", e)
	}
	zap.L().Info("Qobuz playlist creation finalized", zap.String("operation_id", o.ID), zap.String("status", o.Status))
	return *o, nil
}
func (s *Service) mutate(ctx context.Context, id, destination, mode string, confirmed bool) (model.Operation, error) {
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	if !confirmed {
		return model.Operation{}, fmt.Errorf("explicit confirmation is required")
	}
	if destination == "" {
		return model.Operation{}, fmt.Errorf("choose an existing playlist")
	}
	p, e := s.Preview(id)
	if e != nil {
		return model.Operation{}, e
	}
	if len(p.TrackIDs()) == 0 {
		return model.Operation{}, fmt.Errorf("preview contains no matched tracks")
	}
	original, e := s.Provider.GetPlaylist(ctx, destination)
	if e != nil {
		return model.Operation{}, e
	}
	o, e := newOperation(p, mode, original.Summary.Name)
	if e != nil {
		return model.Operation{}, e
	}
	o.DestinationPlaylistID = &destination
	if e = s.Store.SaveOperationWithBackup(*o, original); e != nil {
		return *o, fmt.Errorf("save operation backup: %w", e)
	}
	zap.L().Info("starting verified Qobuz playlist mutation", zap.String("operation_id", o.ID), zap.String("mode", mode), zap.Int("original_track_count", len(original.Tracks)))
	requested := p.TrackIDs()
	expected := requested
	if mode == "append_existing" {
		requested = missingTrackIDs(original.Tracks, requested)
		expected = append(trackIDs(original.Tracks), requested...)
	} else {
		_, e = s.Provider.ClearPlaylist(ctx, destination)
	}
	var final model.ProviderPlaylist
	if e == nil {
		final, e = s.Provider.AddTracks(ctx, destination, requested)
	}
	if e == nil {
		e = verify(final.Tracks, expected)
	}
	if e == nil {
		o.Status = "succeeded"
		o.Result = map[string]any{"playlist_id": destination, "playlist_url": original.Summary.URL, "track_count": len(final.Tracks), "skipped_count": p.SkippedCount(), "backup_available": true}
		if mode == "append_existing" {
			o.Result["original_track_count"] = len(original.Tracks)
			o.Result["appended_track_count"] = len(requested)
		}
	} else {
		fail(o, e)
		_, rollbackErr := s.Provider.ClearPlaylist(ctx, destination)
		if rollbackErr == nil {
			var restored model.ProviderPlaylist
			restored, rollbackErr = s.Provider.AddTracks(ctx, destination, trackIDs(original.Tracks))
			if rollbackErr == nil {
				rollbackErr = verify(restored.Tracks, trackIDs(original.Tracks))
			}
		}
		if rollbackErr == nil {
			o.Status = "rolled_back"
			zap.L().Warn("Qobuz mutation failed and backup was restored", zap.String("operation_id", o.ID), zap.Error(e))
		} else {
			o.Status = "rollback_failed"
			o.Result["rollback_error"] = rollbackErr.Error()
			logging.Critical(zap.L(), "Qobuz mutation and automatic rollback both failed", zap.String("operation_id", o.ID), zap.Error(e), zap.NamedError("rollback_error", rollbackErr))
		}
	}
	if e = finish(s.Store, o); e != nil {
		return *o, fmt.Errorf("save completed operation: %w", e)
	}
	zap.L().Info("Qobuz playlist mutation finalized", zap.String("operation_id", o.ID), zap.String("status", o.Status))
	return *o, nil
}
func (s *Service) Append(ctx context.Context, id, destination string, confirmed bool) (model.Operation, error) {
	return s.mutate(ctx, id, destination, "append_existing", confirmed)
}
func (s *Service) Replace(ctx context.Context, id, destination string, confirmed bool) (model.Operation, error) {
	return s.mutate(ctx, id, destination, "replace_existing", confirmed)
}
func trackIDs(tracks []model.CatalogTrack) []string {
	ids := make([]string, len(tracks))
	for i, t := range tracks {
		ids[i] = t.ID
	}
	return ids
}

func missingTrackIDs(existing []model.CatalogTrack, requested []string) []string {
	seen := make(map[string]struct{}, len(existing))
	for _, track := range existing {
		seen[track.ID] = struct{}{}
	}
	result := make([]string, 0, len(requested))
	for _, id := range requested {
		if _, ok := seen[id]; !ok {
			result = append(result, id)
			seen[id] = struct{}{}
		}
	}
	return result
}
func (s *Service) Restore(ctx context.Context, id string, confirmed bool) (model.Operation, error) {
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	if !confirmed {
		return model.Operation{}, fmt.Errorf("explicit confirmation is required")
	}
	o, e := s.Store.Operation(id)
	if e != nil {
		return o, e
	}
	backup, e := s.Store.Backup(id)
	if e != nil {
		return o, e
	}
	zap.L().Info("restoring Qobuz playlist from local backup", zap.String("operation_id", id), zap.Int("track_count", len(backup.Tracks)))
	if _, e = s.Provider.ClearPlaylist(ctx, backup.Summary.ID); e != nil {
		return o, e
	}
	final, e := s.Provider.AddTracks(ctx, backup.Summary.ID, trackIDs(backup.Tracks))
	if e != nil {
		return o, e
	}
	if e = verify(final.Tracks, trackIDs(backup.Tracks)); e != nil {
		return o, e
	}
	o.Result["restored_track_count"] = len(final.Tracks)
	if err := s.Store.SaveOperation(o); err != nil {
		return o, err
	}
	zap.L().Info("Qobuz playlist backup restored", zap.String("operation_id", id), zap.Int("track_count", len(final.Tracks)))
	return o, nil
}
