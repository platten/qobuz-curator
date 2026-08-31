package service

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pawel/qobuz-curator/internal/config"
	"github.com/pawel/qobuz-curator/internal/model"
	"github.com/pawel/qobuz-curator/internal/provider"
	"github.com/pawel/qobuz-curator/internal/store"
)

func setup(t *testing.T) (*Service, *provider.Fake) {
	t.Helper()
	db, e := store.Open(filepath.Join(t.TempDir(), "db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	p := provider.NewFake()
	return &Service{Config: config.Config{MatchThreshold: .5, PreviewTTLHours: 24}, Store: db, Provider: p}, p
}
func playlist() model.PlaylistInput {
	return model.PlaylistInput{SchemaVersion: "1.0", Name: "Test", Description: "Desc", Tracks: []model.TrackRequest{{Position: 1, Title: "So What", Artists: []string{"Miles Davis"}}, {Position: 2, Title: "Take Five", Artists: []string{"Dave Brubeck"}}}}
}
func TestPrepareAndPublishLifecycle(t *testing.T) {
	ctx := context.Background()
	s, p := setup(t)
	preview, e := s.Prepare(ctx, playlist())
	if e != nil || preview.MatchedCount() != 2 {
		t.Fatal(preview, e)
	}
	if _, e = s.Preview(preview.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Create(ctx, preview.ID, "x", false); e == nil {
		t.Fatal("confirm")
	}
	created, e := s.Create(ctx, preview.ID, "Created", true)
	if e != nil || created.Status != "succeeded" {
		t.Fatal(created, e)
	}
	target, _ := p.CreatePlaylist(ctx, "Target", "")
	p.AddTracks(ctx, target.ID, []string{"demo-2"})
	appended, e := s.Append(ctx, preview.ID, target.ID, true)
	if e != nil || appended.Status != "succeeded" {
		t.Fatal(appended, e)
	}
	if _, e = s.Replace(ctx, preview.ID, target.ID, false); e == nil {
		t.Fatal("confirm")
	}
	replaced, e := s.Replace(ctx, preview.ID, target.ID, true)
	if e != nil || replaced.Status != "succeeded" {
		t.Fatal(replaced, e)
	}
	restored, e := s.Restore(ctx, replaced.ID, true)
	if e != nil || restored.Result["restored_track_count"] == nil {
		t.Fatal(restored, e)
	}
	if _, e = s.Restore(ctx, replaced.ID, false); e == nil {
		t.Fatal("confirm")
	}
	if _, e = s.Append(ctx, preview.ID, "", true); e == nil {
		t.Fatal("destination")
	}
}
func TestPrepareDuplicateAndErrors(t *testing.T) {
	ctx := context.Background()
	s, _ := setup(t)
	p := playlist()
	p.Tracks = append(p.Tracks, model.TrackRequest{Position: 3, Title: "So What", Artists: []string{"Miles Davis"}})
	preview, e := s.Prepare(ctx, p)
	if e != nil || preview.Matches[2].Status != "skipped" {
		t.Fatal(preview, e)
	}
	if _, e = s.Preview("missing"); e == nil {
		t.Fatal("missing")
	}
	expired := preview
	expired.ID = "old"
	expired.ExpiresAt = time.Now().Add(-time.Hour)
	s.Store.SavePreview(expired)
	if _, e = s.Preview("old"); e == nil {
		t.Fatal("expired")
	}
	bad := playlist()
	bad.Tracks = nil
	if _, e = s.Prepare(ctx, bad); e == nil {
		t.Fatal("bad")
	}
	if _, e = s.Restore(ctx, "missing", true); e == nil {
		t.Fatal("backup")
	}
}

func TestPublicationValidationAndHelpers(t *testing.T) {
	ctx := context.Background()
	s, _ := setup(t)
	preview, err := s.Prepare(ctx, playlist())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Create(ctx, preview.ID, " ", true); err == nil {
		t.Fatal("blank name")
	}
	unmatched := playlist()
	unmatched.Name = "Unmatched"
	unmatched.Tracks = []model.TrackRequest{{Position: 1, Title: "does not exist", Artists: []string{"nobody"}}}
	unmatchedPreview, err := s.Prepare(ctx, unmatched)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Create(ctx, unmatchedPreview.ID, "empty", true); err == nil {
		t.Fatal("empty matched set")
	}
	if err = verify([]model.CatalogTrack{{ID: "a"}}, []string{"a", "b"}); err == nil {
		t.Fatal("length mismatch")
	}
	if err = verify([]model.CatalogTrack{{ID: "a"}}, []string{"b"}); err == nil {
		t.Fatal("order mismatch")
	}
	missing := missingTrackIDs([]model.CatalogTrack{{ID: "a"}}, []string{"a", "b", "b"})
	if len(missing) != 1 || missing[0] != "b" {
		t.Fatal(missing)
	}
}

func TestPersistenceFailures(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	p := provider.NewFake()
	s := &Service{Config: config.Config{MatchThreshold: .5, PreviewTTLHours: 24}, Store: db, Provider: p}
	preview, err := s.Prepare(ctx, playlist())
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Prepare(ctx, playlist()); err == nil {
		t.Fatal("closed preview store")
	}
	if _, err = s.Create(ctx, preview.ID, "x", true); err == nil {
		t.Fatal("closed operation store")
	}
}

type failing struct {
	*provider.Fake
	failAdd     bool
	failClear   bool
	failCreate  bool
	failGet     bool
	failSearch  bool
	wrongResult bool
}

type failingPersistence struct {
	*store.Store
	failSaveOperation     bool
	failSaveOperationCall int
	saveOperationCalls    int
	failBackup            bool
}

func (f *failingPersistence) SaveOperation(o model.Operation) error {
	f.saveOperationCalls++
	if f.failSaveOperation || f.saveOperationCalls == f.failSaveOperationCall {
		return errors.New("save operation")
	}
	return f.Store.SaveOperation(o)
}

func (f *failingPersistence) SaveOperationWithBackup(o model.Operation, p model.ProviderPlaylist) error {
	if f.failBackup {
		return errors.New("save backup")
	}
	return f.Store.SaveOperationWithBackup(o, p)
}

func (f *failing) AddTracks(ctx context.Context, id string, ids []string) (model.ProviderPlaylist, error) {
	if f.failAdd {
		f.failAdd = false
		return model.ProviderPlaylist{}, context.DeadlineExceeded
	}
	result, err := f.Fake.AddTracks(ctx, id, ids)
	if f.wrongResult && err == nil {
		f.wrongResult = false
		result.Tracks = nil
	}
	return result, err
}
func (f *failing) ClearPlaylist(ctx context.Context, id string) (model.ProviderPlaylist, error) {
	if f.failClear {
		return model.ProviderPlaylist{}, context.Canceled
	}
	return f.Fake.ClearPlaylist(ctx, id)
}
func (f *failing) CreatePlaylist(ctx context.Context, name, description string) (model.PlaylistSummary, error) {
	if f.failCreate {
		return model.PlaylistSummary{}, context.Canceled
	}
	return f.Fake.CreatePlaylist(ctx, name, description)
}
func (f *failing) GetPlaylist(ctx context.Context, id string) (model.ProviderPlaylist, error) {
	if f.failGet {
		return model.ProviderPlaylist{}, context.Canceled
	}
	return f.Fake.GetPlaylist(ctx, id)
}
func (f *failing) SearchTracks(ctx context.Context, query string, limit int) ([]model.CatalogTrack, error) {
	if f.failSearch {
		return nil, context.Canceled
	}
	return f.Fake.SearchTracks(ctx, query, limit)
}
func TestRollbackAndCreateFailure(t *testing.T) {
	ctx := context.Background()
	base := provider.NewFake()
	db, e := store.Open(filepath.Join(t.TempDir(), "db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	f := &failing{Fake: base}
	s := &Service{Config: config.Config{MatchThreshold: .5, PreviewTTLHours: 24}, Store: db, Provider: f}
	preview, _ := s.Prepare(ctx, playlist())
	target, _ := f.CreatePlaylist(ctx, "Target", "")
	f.AddTracks(ctx, target.ID, []string{"demo-2"})
	f.failAdd = true
	o, e := s.Replace(ctx, preview.ID, target.ID, true)
	if e != nil || o.Status != "rolled_back" {
		t.Fatal(o, e)
	}
	f.failAdd = true
	created, e := s.Create(ctx, preview.ID, "broken", true)
	if e != nil || created.Status != "failed" {
		t.Fatal(created, e)
	}
	f.failAdd = true
	f.failClear = true
	o, e = s.Append(ctx, preview.ID, target.ID, true)
	if e != nil || o.Status != "rollback_failed" {
		t.Fatal(o, e)
	}
}

func TestServiceFailureBoundaries(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := provider.NewFake()
	persistence := &failingPersistence{Store: db}
	s := &Service{Config: config.Config{MatchThreshold: .5, PreviewTTLHours: 24}, Store: persistence, Provider: p}
	preview, err := s.Prepare(ctx, playlist())
	if err != nil {
		t.Fatal(err)
	}
	persistence.failSaveOperation = true
	if _, err = s.Create(ctx, preview.ID, "x", true); err == nil || !strings.Contains(err.Error(), "pending") {
		t.Fatal(err)
	}
	persistence.failSaveOperation = false
	target, _ := p.CreatePlaylist(ctx, "target", "")
	persistence.failBackup = true
	if _, err = s.Replace(ctx, preview.ID, target.ID, true); err == nil || !strings.Contains(err.Error(), "backup") {
		t.Fatal(err)
	}
	persistence.failBackup = false

	originalRandom := readRandom
	defer func() { readRandom = originalRandom }()
	readRandom = func([]byte) (int, error) { return 0, errors.New("entropy") }
	if _, err = s.Prepare(ctx, playlist()); err == nil || !strings.Contains(err.Error(), "identifier") {
		t.Fatal(err)
	}
	if _, err = newOperation(preview, "create_new", "x"); err == nil {
		t.Fatal("operation entropy")
	}
}

func TestServiceRemoteAndCompletionFailures(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	remote := &failing{Fake: provider.NewFake()}
	persistence := &failingPersistence{Store: db}
	s := &Service{Config: config.Config{MatchThreshold: .5, PreviewTTLHours: 24}, Store: persistence, Provider: remote}

	withAlbum := playlist()
	album := "Kind of Blue"
	withAlbum.Tracks[0].Album = &album
	preview, err := s.Prepare(ctx, withAlbum)
	if err != nil || join([]string{"Miles", "Davis"}) != "Miles Davis" {
		t.Fatal(preview, err)
	}
	target, _ := remote.Fake.CreatePlaylist(ctx, "Target", "")

	remote.failSearch = true
	if _, err = s.Prepare(ctx, playlist()); !errors.Is(err, context.Canceled) {
		t.Fatal("search failure", err)
	}
	remote.failSearch = false
	if _, err = s.Append(ctx, "missing", target.ID, true); err == nil {
		t.Fatal("missing preview")
	}
	unmatched := playlist()
	unmatched.Name = "Unmatched"
	unmatched.Tracks = []model.TrackRequest{{Position: 1, Title: "not found", Artists: []string{"nobody"}}}
	unmatchedPreview, _ := s.Prepare(ctx, unmatched)
	if _, err = s.Append(ctx, unmatchedPreview.ID, target.ID, true); err == nil {
		t.Fatal("unmatched mutation")
	}
	remote.failGet = true
	if _, err = s.Append(ctx, preview.ID, target.ID, true); !errors.Is(err, context.Canceled) {
		t.Fatal("get failure", err)
	}
	remote.failGet = false

	originalRandom := readRandom
	defer func() { readRandom = originalRandom }()
	readRandom = func([]byte) (int, error) { return 0, errors.New("entropy") }
	if _, err = s.Create(ctx, preview.ID, "new", true); err == nil {
		t.Fatal("create identifier failure")
	}
	if _, err = s.Append(ctx, preview.ID, target.ID, true); err == nil {
		t.Fatal("mutation identifier failure")
	}
	readRandom = originalRandom

	persistence.saveOperationCalls = 0
	persistence.failSaveOperationCall = 2
	if _, err = s.Create(ctx, preview.ID, "new", true); err == nil || !strings.Contains(err.Error(), "completed") {
		t.Fatal("create completion persistence", err)
	}
	persistence.saveOperationCalls = 0
	persistence.failSaveOperationCall = 1
	if _, err = s.Append(ctx, preview.ID, target.ID, true); err == nil || !strings.Contains(err.Error(), "completed") {
		t.Fatal("mutation completion persistence", err)
	}
	persistence.failSaveOperationCall = 0

	remote.failCreate = true
	created, err := s.Create(ctx, preview.ID, "remote failure", true)
	if err != nil || created.Status != "failed" {
		t.Fatal(created, err)
	}
	remote.failCreate = false
}

func TestRestoreFailureBoundaries(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	remote := &failing{Fake: provider.NewFake()}
	persistence := &failingPersistence{Store: db}
	s := &Service{Config: config.Config{MatchThreshold: .5, PreviewTTLHours: 24}, Store: persistence, Provider: remote}
	preview, _ := s.Prepare(ctx, playlist())
	target, _ := remote.Fake.CreatePlaylist(ctx, "Target", "")
	if _, err = remote.Fake.AddTracks(ctx, target.ID, []string{"demo-3"}); err != nil {
		t.Fatal(err)
	}
	operation, err := s.Replace(ctx, preview.ID, target.ID, true)
	if err != nil {
		t.Fatal(err)
	}

	withoutBackup := model.Operation{ID: "without-backup", PreviewID: preview.ID, Status: "succeeded", CreatedAt: time.Now(), Result: map[string]any{}}
	if err = db.SaveOperation(withoutBackup); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Restore(ctx, withoutBackup.ID, true); err == nil {
		t.Fatal("missing backup")
	}
	remote.failClear = true
	if _, err = s.Restore(ctx, operation.ID, true); !errors.Is(err, context.Canceled) {
		t.Fatal("clear failure", err)
	}
	remote.failClear = false
	remote.failAdd = true
	if _, err = s.Restore(ctx, operation.ID, true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("add failure", err)
	}
	remote.wrongResult = true
	if _, err = s.Restore(ctx, operation.ID, true); err == nil || !strings.Contains(err.Error(), "verification") {
		t.Fatal("verification failure", err)
	}
	persistence.failSaveOperation = true
	if _, err = s.Restore(ctx, operation.ID, true); err == nil || !strings.Contains(err.Error(), "save operation") {
		t.Fatal("save failure", err)
	}
}
