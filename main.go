package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/pawel/qobuz-curator/internal/cli"
	"github.com/pawel/qobuz-curator/internal/logging"
	"github.com/pawel/qobuz-curator/internal/privilege"
	"go.uber.org/zap"
	"golang.org/x/term"
)

// version is the source release fallback. Production builds replace it with
// the exact Git-derived version through -ldflags "-X main.version=...".
var version = "v0.4.0"

func main() {
	bootLogger, loggerErr := logging.New("info", "console", term.IsTerminal(int(os.Stderr.Fd())), os.Stderr)
	if loggerErr != nil {
		os.Exit(1)
	}
	zap.ReplaceGlobals(bootLogger)
	defer func() { _ = bootLogger.Sync() }()

	if err := privilege.RefuseElevated(); err != nil {
		logging.Critical(zap.L(), "unsafe process privileges", zap.Error(err))
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := cli.ExecuteContext(ctx, version); err != nil {
		logging.Critical(zap.L(), "application stopped with an error", zap.Error(err))
		os.Exit(1)
	}
}
