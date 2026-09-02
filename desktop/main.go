//go:build desktop

package main

import (
	"context"
	"embed"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/pawel/qobuz-curator/internal/config"
	"github.com/pawel/qobuz-curator/internal/desktopapp"
	"github.com/pawel/qobuz-curator/internal/logging"
	"github.com/pawel/qobuz-curator/internal/privilege"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/logger"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
	"go.uber.org/zap"
)

//go:embed build/appicon.png
var iconFS embed.FS

var version = "v0.5.0"

type shell struct {
	mu  sync.RWMutex
	ctx context.Context
	app *desktopapp.App
}

func (s *shell) startup(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
}

func (s *shell) showExistingWindow(options.SecondInstanceData) {
	s.mu.RLock()
	ctx := s.ctx
	s.mu.RUnlock()
	if ctx != nil {
		wailsRuntime.WindowUnminimise(ctx)
		wailsRuntime.WindowShow(ctx)
	}
}

func (s *shell) shutdown(context.Context) {
	if s.app != nil {
		_ = s.app.Close()
	}
}

func main() {
	icon, _ := iconFS.ReadFile("build/appicon.png")
	logFile, undoLogger := configureDesktopLogger()
	defer func() {
		undoLogger()
		if logFile != nil {
			_ = logFile.Close()
		}
	}()

	var handler http.Handler
	var app *desktopapp.App
	if err := privilege.RefuseElevated(); err != nil {
		handler = startupErrorHandler(err)
	} else {
		var err error
		app, err = desktopapp.New(desktopapp.SystemOptions())
		if err != nil {
			handler = startupErrorHandler(err)
		} else {
			handler = app
		}
	}
	shell := &shell{app: app}
	err := wails.Run(&options.App{
		Title:                            "Qobuz Curator",
		Width:                            1200,
		Height:                           820,
		MinWidth:                         860,
		MinHeight:                        640,
		BackgroundColour:                 options.NewRGB(245, 241, 232),
		AssetServer:                      &assetserver.Options{Handler: handler},
		OnStartup:                        shell.startup,
		OnShutdown:                       shell.shutdown,
		EnableDefaultContextMenu:         false,
		EnableFraudulentWebsiteDetection: false,
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId:               "3bd67e1c-8e66-45a8-a3fc-cd5fe85a8c11",
			OnSecondInstanceLaunch: shell.showExistingWindow,
		},
		DragAndDrop:        &options.DragAndDrop{DisableWebViewDrop: true},
		LogLevel:           logger.INFO,
		LogLevelProduction: logger.ERROR,
		Windows: &windows.Options{
			Theme:                windows.SystemDefault,
			IsZoomControlEnabled: true,
			WindowClassName:      "QobuzCuratorWindow",
		},
		Mac: &mac.Options{
			TitleBar: mac.TitleBarDefault(),
			About: &mac.AboutInfo{
				Title:   "Qobuz Curator " + version,
				Message: "Turn music recommendations into reviewed Qobuz playlists.",
				Icon:    icon,
			},
		},
		Linux: &linux.Options{
			Icon:             icon,
			ProgramName:      "qobuz-curator-desktop",
			WebviewGpuPolicy: linux.WebviewGpuPolicyOnDemand,
		},
	})
	if err != nil {
		logging.Critical(zap.L(), "desktop application stopped with an error", zap.Error(err))
	}
}

func startupErrorHandler(startupErr error) http.Handler {
	zap.L().Error("desktop startup failed", zap.Error(startupErr))
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprintf(w, "Qobuz Curator could not start.\n\n%v\n\nSee the desktop log for details.", startupErr)
	})
}

func configureDesktopLogger() (*os.File, func()) {
	directory := config.DefaultDir()
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, func() {}
	}
	file, err := os.OpenFile(filepath.Join(directory, "qobuz-curator-desktop.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, func() {}
	}
	configured, err := logging.New("info", "json", false, file)
	if err != nil {
		_ = file.Close()
		return nil, func() {}
	}
	undo := zap.ReplaceGlobals(configured)
	return file, func() {
		_ = configured.Sync()
		undo()
	}
}
