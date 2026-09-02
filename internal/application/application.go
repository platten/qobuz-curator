// Package application assembles the reusable Qobuz Curator runtime.
package application

import (
	"fmt"
	"net/http"
	"os"
	"sync"

	"github.com/pawel/qobuz-curator/internal/config"
	"github.com/pawel/qobuz-curator/internal/provider"
	"github.com/pawel/qobuz-curator/internal/recommend"
	"github.com/pawel/qobuz-curator/internal/service"
	"github.com/pawel/qobuz-curator/internal/store"
	"github.com/pawel/qobuz-curator/internal/webapp"
	"go.uber.org/zap"
)

// Runtime owns the resources shared by the browser and desktop launchers.
type Runtime struct {
	Config config.Config
	Store  *store.Store
	Web    *webapp.App

	closeOnce sync.Once
	closeErr  error
}

// Open validates runtime-only requirements and assembles the application.
func Open(cfg config.Config) (*Runtime, error) {
	if cfg.Provider == "qobuz" && (cfg.QobuzAppID == "" || cfg.QobuzUserToken == "") {
		return nil, fmt.Errorf("qobuz_app_id and qobuz_user_auth_token are required for qobuz")
	}
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	db, err := store.Open(cfg.DatabasePath())
	if err != nil {
		return nil, err
	}
	p := provider.New(cfg)
	svc := &service.Service{Config: cfg, Store: db, Provider: p}
	web, err := webapp.New(cfg, svc, p, recommend.New(cfg), db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Runtime{Config: cfg, Store: db, Web: web}, nil
}

// Handler returns the existing server-rendered interface.
func (r *Runtime) Handler() http.Handler { return r.Web.Handler() }

// Close releases persistent resources. It is safe to call more than once.
func (r *Runtime) Close() error {
	r.closeOnce.Do(func() {
		r.closeErr = r.Store.Close()
		if r.closeErr != nil {
			zap.L().Warn("close database", zap.Error(r.closeErr))
		}
	})
	return r.closeErr
}
