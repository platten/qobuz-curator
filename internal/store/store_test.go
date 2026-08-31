package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pawel/qobuz-curator/internal/model"
)

func TestStoreRoundTrip(t *testing.T) {
	s, e := Open(filepath.Join(t.TempDir(), "nested", "db.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	now := time.Now().UTC()
	p := model.Preview{ID: "p", Playlist: model.PlaylistInput{SchemaVersion: "1.0", Name: "x", Tracks: []model.TrackRequest{{Position: 1, Title: "t", Artists: []string{"a"}}}}, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if e = s.SavePreview(p); e != nil {
		t.Fatal(e)
	}
	if got, e := s.Preview("p"); e != nil || got.ID != "p" {
		t.Fatal(got, e)
	}
	if _, e = s.Preview("missing"); e == nil {
		t.Fatal("missing")
	}
	o := model.Operation{ID: "o", PreviewID: "p", Mode: "replace_existing", Status: "pending", DestinationName: "x", CreatedAt: now, Result: map[string]any{}}
	if e = s.SaveOperation(o); e != nil {
		t.Fatal(e)
	}
	if got, e := s.Operation("o"); e != nil || got.ID != "o" {
		t.Fatal(got, e)
	}
	if list, e := s.Operations(10); e != nil || len(list) != 1 {
		t.Fatal(list, e)
	}
	if _, e = s.Operation("missing"); e == nil {
		t.Fatal("missing")
	}
	backup := model.ProviderPlaylist{Summary: model.PlaylistSummary{ID: "x"}}
	if e = s.SaveBackup("o", backup); e != nil {
		t.Fatal(e)
	}
	if got, e := s.Backup("o"); e != nil || got.Summary.ID != "x" {
		t.Fatal(got, e)
	}
	if _, e = s.Backup("missing"); e == nil {
		t.Fatal("missing")
	}
	o.ID = "atomic"
	if e = s.SaveOperationWithBackup(o, backup); e != nil {
		t.Fatal(e)
	}
	if got, e := s.Backup("atomic"); e != nil || got.Summary.ID != "x" {
		t.Fatal(got, e)
	}
}

func TestStoreErrors(t *testing.T) {
	parentFile := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(parentFile, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(parentFile, "db")); err == nil {
		t.Fatal("invalid parent")
	}
	if _, err := Open(t.TempDir()); err == nil {
		t.Fatal("directory as database")
	}
	s, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	preview := model.Preview{ID: "bad", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	operation := model.Operation{ID: "bad", PreviewID: "bad", CreatedAt: now, Result: map[string]any{"unsupported": func() {}}}
	if err := s.SaveOperation(operation); err == nil {
		t.Fatal("marshal operation")
	}
	if _, err := s.db.Exec("INSERT INTO previews VALUES(?,?,?,?)", "corrupt", "x", "x", "{"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Preview("corrupt"); err == nil {
		t.Fatal("corrupt preview")
	}
	if _, err := s.db.Exec("INSERT INTO operations VALUES(?,?,?,?,?)", "corrupt", "x", "x", "x", "{"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Operation("corrupt"); err == nil {
		t.Fatal("corrupt operation")
	}
	if _, err := s.Operations(10); err == nil {
		t.Fatal("corrupt operation list")
	}
	if _, err := s.db.Exec("INSERT INTO backups VALUES(?,?,?)", "corrupt", "x", "{"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Backup("corrupt"); err == nil {
		t.Fatal("corrupt backup")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePreview(preview); err == nil {
		t.Fatal("closed save preview")
	}
	operation.Result = map[string]any{}
	if err := s.SaveOperation(operation); err == nil {
		t.Fatal("closed save operation")
	}
	if err := s.SaveBackup("x", model.ProviderPlaylist{}); err == nil {
		t.Fatal("closed save backup")
	}
	if err := s.SaveOperationWithBackup(operation, model.ProviderPlaylist{}); err == nil {
		t.Fatal("closed transaction")
	}
	if _, err := s.Operations(1); err == nil {
		t.Fatal("closed operations")
	}
}

func TestAtomicBackupFailures(t *testing.T) {
	newStore := func(t *testing.T) *Store {
		t.Helper()
		s, err := Open(filepath.Join(t.TempDir(), "db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	now := time.Now().UTC()
	operation := model.Operation{ID: "o", PreviewID: "p", CreatedAt: now, Result: map[string]any{}}
	backup := model.ProviderPlaylist{Summary: model.PlaylistSummary{ID: "p"}}
	s := newStore(t)
	bad := operation
	bad.Result = map[string]any{"bad": func() {}}
	if err := s.SaveOperationWithBackup(bad, backup); err == nil {
		t.Fatal("operation marshal")
	}
	if _, err := s.db.Exec("DROP TABLE operations"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveOperationWithBackup(operation, backup); err == nil {
		t.Fatal("operation insert")
	}
	s = newStore(t)
	if _, err := s.db.Exec("DROP TABLE backups"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveOperationWithBackup(operation, backup); err == nil {
		t.Fatal("backup insert")
	}
}
