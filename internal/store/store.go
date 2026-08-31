// Package store persists previews, operations, and rollback backups in SQLite.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pawel/qobuz-curator/internal/model"
	"go.uber.org/zap"
	_ "modernc.org/sqlite"
)

// Store persists previews, mutation audit records, and rollback backups.
type Store struct{ db *sql.DB }

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA busy_timeout=5000;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS previews (
    id TEXT PRIMARY KEY,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    payload TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS operations (
    id TEXT PRIMARY KEY,
    created_at TEXT NOT NULL,
    preview_id TEXT NOT NULL,
    status TEXT NOT NULL,
    payload TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS backups (
    operation_id TEXT PRIMARY KEY,
    playlist_id TEXT NOT NULL,
    payload TEXT NOT NULL
);`

// Open creates a private SQLite store. A single connection avoids lock
// contention for this deliberately small, local application.
func Open(path string) (*Store, error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(directory, 0700); err != nil && !errors.Is(err, os.ErrPermission) {
		return nil, fmt.Errorf("protect data directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db}
	if err = s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil && !errors.Is(err, os.ErrPermission) {
		_ = db.Close()
		return nil, fmt.Errorf("protect database file: %w", err)
	}
	zap.L().Info("SQLite store opened", zap.String("path", path), zap.Int("max_connections", 1))
	return s, nil
}
func (s *Store) Close() error {
	err := s.db.Close()
	if err == nil {
		zap.L().Debug("SQLite store closed")
	}
	return err
}
func (s *Store) init() error {
	_, err := s.db.Exec(schema)
	return err
}
func marshal(v any) (string, error) { b, e := json.Marshal(v); return string(b), e }
func (s *Store) SavePreview(p model.Preview) error {
	raw, e := marshal(p)
	if e != nil {
		return e
	}
	_, e = s.db.Exec("INSERT OR REPLACE INTO previews VALUES(?,?,?,?)", p.ID, p.CreatedAt.Format("2006-01-02T15:04:05.999999-07:00"), p.ExpiresAt.Format("2006-01-02T15:04:05.999999-07:00"), raw)
	if e == nil {
		zap.L().Debug("preview persisted", zap.String("preview_id", p.ID))
	}
	return e
}
func (s *Store) Preview(id string) (model.Preview, error) {
	var raw string
	e := s.db.QueryRow("SELECT payload FROM previews WHERE id=?", id).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return model.Preview{}, fmt.Errorf("preview %s was not found", id)
	}
	var p model.Preview
	if e == nil {
		e = json.Unmarshal([]byte(raw), &p)
	}
	return p, e
}
func (s *Store) SaveOperation(o model.Operation) error {
	raw, e := marshal(o)
	if e != nil {
		return e
	}
	_, e = s.db.Exec("INSERT OR REPLACE INTO operations VALUES(?,?,?,?,?)", o.ID, o.CreatedAt.Format("2006-01-02T15:04:05.999999-07:00"), o.PreviewID, o.Status, raw)
	if e == nil {
		zap.L().Debug("operation persisted", zap.String("operation_id", o.ID), zap.String("status", o.Status))
	}
	return e
}
func (s *Store) Operation(id string) (model.Operation, error) {
	var raw string
	e := s.db.QueryRow("SELECT payload FROM operations WHERE id=?", id).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return model.Operation{}, fmt.Errorf("operation not found")
	}
	var o model.Operation
	if e == nil {
		e = json.Unmarshal([]byte(raw), &o)
	}
	return o, e
}
func (s *Store) Operations(limit int) ([]model.Operation, error) {
	rows, e := s.db.Query("SELECT payload FROM operations ORDER BY created_at DESC LIMIT ?", limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var result []model.Operation
	for rows.Next() {
		var raw string
		if e = rows.Scan(&raw); e != nil {
			return nil, e
		}
		var o model.Operation
		if e = json.Unmarshal([]byte(raw), &o); e != nil {
			return nil, e
		}
		result = append(result, o)
	}
	return result, rows.Err()
}
func (s *Store) SaveBackup(operationID string, p model.ProviderPlaylist) error {
	raw, e := marshal(p)
	if e != nil {
		return e
	}
	_, e = s.db.Exec("INSERT OR REPLACE INTO backups VALUES(?,?,?)", operationID, p.Summary.ID, raw)
	return e
}

// SaveOperationWithBackup atomically establishes the audit record and backup
// required before a destructive remote playlist mutation begins.
func (s *Store) SaveOperationWithBackup(o model.Operation, p model.ProviderPlaylist) error {
	operationRaw, err := marshal(o)
	if err != nil {
		return err
	}
	backupRaw, err := marshal(p)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(
		"INSERT OR REPLACE INTO operations VALUES(?,?,?,?,?)",
		o.ID, o.CreatedAt.Format("2006-01-02T15:04:05.999999-07:00"), o.PreviewID, o.Status, operationRaw,
	); err != nil {
		return err
	}
	if _, err = tx.Exec(
		"INSERT OR REPLACE INTO backups VALUES(?,?,?)",
		o.ID, p.Summary.ID, backupRaw,
	); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	zap.L().Debug("operation and rollback backup persisted atomically", zap.String("operation_id", o.ID), zap.Int("backup_track_count", len(p.Tracks)))
	return nil
}

func (s *Store) Backup(id string) (model.ProviderPlaylist, error) {
	var raw string
	e := s.db.QueryRow("SELECT payload FROM backups WHERE operation_id=?", id).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return model.ProviderPlaylist{}, fmt.Errorf("operation backup was not found")
	}
	var p model.ProviderPlaylist
	if e == nil {
		e = json.Unmarshal([]byte(raw), &p)
	}
	return p, e
}
