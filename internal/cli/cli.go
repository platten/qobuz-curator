// Package cli defines the qobuz-curator command tree and process lifecycle.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/briandowns/spinner"
	"github.com/fatih/color"
	"github.com/pawel/qobuz-curator/internal/browser"
	"github.com/pawel/qobuz-curator/internal/config"
	"github.com/pawel/qobuz-curator/internal/logging"
	"github.com/pawel/qobuz-curator/internal/provider"
	"github.com/pawel/qobuz-curator/internal/qobuzauth"
	"github.com/pawel/qobuz-curator/internal/recommend"
	"github.com/pawel/qobuz-curator/internal/security"
	"github.com/pawel/qobuz-curator/internal/service"
	"github.com/pawel/qobuz-curator/internal/store"
	"github.com/pawel/qobuz-curator/internal/webapp"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.yaml.in/yaml/v3"
	"golang.org/x/term"
)

type options struct {
	configFile string
	host       string
	port       int
	noBrowser  bool
	colorMode  string
}

var newAuthClient = qobuzauth.New
var readTerminalPassword = term.ReadPassword
var openBrowser = browser.Open
var isTerminal = term.IsTerminal

func Execute(version string) error { return ExecuteContext(context.Background(), version) }

func ExecuteContext(ctx context.Context, version string) error {
	root := NewRoot(version)
	root.SetContext(ctx)
	return root.Execute()
}

func NewRoot(version string) *cobra.Command {
	o := &options{}
	root := &cobra.Command{
		Use:   "qobuz-curator",
		Short: "Turn music recommendations into reviewed Qobuz playlists",
		Long:  "Qobuz Curator turns written or JSON music recommendations into a\nreviewed playlist, resolves recordings against Qobuz, and performs verified writes.\nIt runs locally, stores no OpenAI responses, and never needs administrator access.",
		Example: `  qobuz-curator
  qobuz-curator init --interactive
  qobuz-curator serve --port 49277
  qobuz-curator auth --write-config
  qobuz-curator config-path`,
		Version: version, SilenceUsage: true, SilenceErrors: true,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return serve(cmd, o) },
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.PersistentFlags().StringVar(&o.configFile, "config", "", "YAML configuration file (default: platform user config directory)")
	root.PersistentFlags().StringVar(&o.host, "host", "", "HTTP listen host")
	root.PersistentFlags().IntVarP(&o.port, "port", "p", 0, "HTTP listen port; 0 asks the OS for a free high port")
	root.PersistentFlags().BoolVar(&o.noBrowser, "no-browser", false, "do not open the web UI in a browser")
	root.PersistentFlags().StringVar(&o.colorMode, "color", "auto", "color output: auto, always, or never")
	root.AddCommand(serveCommand(o))
	root.AddCommand(initCommand(o))
	root.AddCommand(authCommand(o))
	root.AddCommand(passwordCommand())
	root.AddCommand(healthCommand(o))
	root.AddCommand(&cobra.Command{Use: "config-path", Short: "Print the platform-native configuration file path", Args: cobra.NoArgs, Run: func(cmd *cobra.Command, _ []string) { fmt.Fprintln(cmd.OutOrStdout(), config.DefaultPath()) }})
	root.AddCommand(&cobra.Command{Use: "version", Short: "Print the version", Args: cobra.NoArgs, Run: func(cmd *cobra.Command, _ []string) { fmt.Fprintln(cmd.OutOrStdout(), version) }})
	return root
}

func serveCommand(o *options) *cobra.Command {
	return &cobra.Command{Use: "serve", Short: "Start the embedded web application", Long: "Start the local web application, bind the requested port atomically, and open\nthe resulting URL in the default browser.", Example: "  qobuz-curator serve\n  qobuz-curator serve --port 49277 --no-browser", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return serve(cmd, o) }}
}

func healthCommand(o *options) *cobra.Command {
	return &cobra.Command{Use: "healthcheck", Hidden: true, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := load(cmd, o)
		if err != nil {
			return err
		}
		if cfg.Port == 0 {
			return fmt.Errorf("healthcheck requires a fixed --port or configured port; the automatic runtime port is not discoverable from another process")
		}
		response, err := (&http.Client{Timeout: 3 * time.Second}).Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", cfg.Port))
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("health endpoint returned %s", response.Status)
		}
		return nil
	}}
}
func load(cmd *cobra.Command, o *options) (config.Config, error) {
	path := configSource(o.configFile)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return config.Config{}, fmt.Errorf("configuration file not found: %s\nRun %q to create it with safe defaults, or %q to configure it interactively", path, "qobuz-curator init", "qobuz-curator init --interactive")
		}
		return config.Config{}, fmt.Errorf("inspect configuration file %s: %w", path, err)
	}
	cfg, _, e := config.Load(o.configFile, cmd.Flags())
	if e != nil {
		return cfg, e
	}
	return cfg, nil
}

func configureLogger(cfg config.Config) (func(), error) {
	logger, err := logging.New(cfg.LogLevel, cfg.LogFormat, cfg.LogColor, os.Stderr)
	if err != nil {
		return nil, err
	}
	undo := zap.ReplaceGlobals(logger)
	return func() {
		_ = logger.Sync()
		undo()
	}, nil
}

func serve(cmd *cobra.Command, o *options) error {
	cfg, e := load(cmd, o)
	if e != nil {
		return e
	}
	cleanupLogger, e := configureLogger(cfg)
	if e != nil {
		return e
	}
	defer cleanupLogger()
	zap.L().Debug("configuration loaded", zap.String("config_file", configSource(o.configFile)), zap.String("provider", cfg.Provider), zap.String("host", cfg.Host), zap.Int("port", cfg.Port))
	if cfg.Provider == "qobuz" && (cfg.QobuzAppID == "" || cfg.QobuzUserToken == "") {
		return fmt.Errorf("qobuz_app_id and qobuz_user_auth_token are required for qobuz")
	}
	if e = os.MkdirAll(cfg.DataDir, 0700); e != nil {
		return e
	}
	db, e := store.Open(cfg.DatabasePath())
	if e != nil {
		return e
	}
	defer func() {
		if err := db.Close(); err != nil {
			zap.L().Warn("close database", zap.Error(err))
		}
	}()
	p := provider.New(cfg)
	svc := &service.Service{Config: cfg, Store: db, Provider: p}
	app, e := webapp.New(cfg, svc, p, recommend.New(cfg), db)
	if e != nil {
		return e
	}
	listener, e := net.Listen("tcp", fmt.Sprintf("%s:%d", cfg.Host, cfg.Port))
	if e != nil {
		return e
	}
	defer func() { _ = listener.Close() }()
	address := listener.Addr().String()
	host := cfg.Host
	if host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if tcp, ok := listener.Addr().(*net.TCPAddr); ok {
		address = fmt.Sprintf("%s:%d", host, tcp.Port)
	}
	webURL := "http://" + address
	colorEnabled, colorErr := useColor(o.colorMode, cmd.OutOrStdout())
	if colorErr != nil {
		return colorErr
	}
	fmt.Fprintln(cmd.OutOrStdout(), paint(colorEnabled, color.FgGreen, color.Bold)("✓ Qobuz Curator is ready:"), paint(colorEnabled, color.FgCyan, color.Underline)(webURL))
	zap.L().Info("HTTP server listening", zap.String("url", webURL), zap.String("bound_address", listener.Addr().String()), zap.Bool("automatic_port", cfg.Port == 0))
	if !cfg.BrowserDisabled {
		go func() {
			timer := time.NewTimer(150 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-cmd.Context().Done():
				return
			case <-timer.C:
			}
			if e := openBrowser(webURL); e != nil {
				zap.L().Warn("could not open the default browser", zap.String("url", webURL), zap.Error(e))
			} else {
				zap.L().Debug("opened the web UI in the default browser", zap.String("url", webURL))
			}
		}()
	}
	server := &http.Server{
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	go func() {
		<-cmd.Context().Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			zap.L().Error("graceful HTTP shutdown failed", zap.Error(err))
		}
	}()
	e = server.Serve(listener)
	if errors.Is(e, http.ErrServerClosed) {
		zap.L().Info("HTTP server stopped cleanly")
		return nil
	}
	logging.Critical(zap.L(), "HTTP server stopped unexpectedly", zap.Error(e))
	return e
}

func authCommand(o *options) *cobra.Command {
	var appID, privateKey string
	var port, timeout int
	var appIDOnly, asJSON, write bool
	cmd := &cobra.Command{Use: "auth", Short: "Obtain Qobuz credentials through browser sign-in", Long: "Discover a current Qobuz application ID, complete Qobuz sign-in through a\nloopback browser callback, and optionally save the resulting credentials.", Example: "  qobuz-curator auth --write-config\n  qobuz-curator auth --app-id-only", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, _, e := config.Load(o.configFile, cmd.Flags())
		if e != nil {
			return e
		}
		cleanupLogger, e := configureLogger(cfg)
		if e != nil {
			return e
		}
		defer cleanupLogger()
		colorEnabled, e := useColor(o.colorMode, cmd.ErrOrStderr())
		if e != nil {
			return e
		}
		client := newAuthClient()
		ctx, cancel := context.WithTimeout(cmd.Context(), time.Duration(timeout)*time.Second)
		defer cancel()
		if appID == "" {
			stop := startSpinner(cmd.ErrOrStderr(), "Discovering Qobuz application ID", colorEnabled)
			appID, e = client.DiscoverAppID(ctx)
			stop(e == nil)
			if e != nil {
				return e
			}
		}
		if appIDOnly {
			fmt.Fprintln(cmd.OutOrStdout(), appID)
			return nil
		}
		stop := startSpinner(cmd.ErrOrStderr(), "Waiting for Qobuz browser authorization", colorEnabled)
		code, login, e := client.ReceiveCode(ctx, appID, port)
		stop(e == nil)
		fmt.Fprintln(cmd.ErrOrStderr(), paint(colorEnabled, color.FgCyan)("Official Qobuz authorization URL:"), login)
		if e != nil {
			return e
		}
		stop = startSpinner(cmd.ErrOrStderr(), "Exchanging the authorization code", colorEnabled)
		creds, e := client.Exchange(ctx, appID, code, privateKey)
		stop(e == nil)
		if e != nil {
			return e
		}
		fmt.Fprintln(cmd.ErrOrStderr(), paint(colorEnabled, color.FgGreen, color.Bold)("✓ Authenticated Qobuz user:"), creds.DisplayName)
		if asJSON {
			raw, marshalErr := json.MarshalIndent(creds, "", "  ")
			if marshalErr != nil {
				return marshalErr
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "qobuz_app_id: %q\nqobuz_user_auth_token: %q\n", creds.AppID, creds.Token)
		}
		if write {
			target := o.configFile
			if target == "" {
				target = config.DefaultPath()
			}
			if e = writeCredentials(target, creds); e != nil {
				return e
			}
			fmt.Fprintln(cmd.ErrOrStderr(), paint(colorEnabled, color.FgGreen)("✓ Updated"), target)
		}
		return nil
	}}
	cmd.Flags().StringVar(&appID, "app-id", "", "skip discovery and use this Qobuz app ID")
	cmd.Flags().BoolVar(&appIDOnly, "app-id-only", false, "print the discovered app ID and exit")
	cmd.Flags().IntVar(&port, "callback-port", 8765, "local OAuth callback port (0 chooses a free port)")
	cmd.Flags().IntVar(&timeout, "timeout", 300, "seconds to wait for browser sign-in")
	cmd.Flags().StringVar(&privateKey, "private-key", qobuzauth.DefaultPrivateKey, "Qobuz OAuth private key")
	_ = cmd.Flags().MarkHidden("private-key")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print credentials as JSON")
	cmd.Flags().BoolVar(&write, "write-config", false, "update the configuration file while preserving other settings")
	return cmd
}

func configSource(explicit string) string {
	if explicit != "" {
		return explicit
	}
	return config.DefaultPath()
}

func useColor(mode string, writer io.Writer) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "always":
		return true, nil
	case "never":
		return false, nil
	case "auto":
		file, ok := writer.(*os.File)
		return ok && os.Getenv("NO_COLOR") == "" && isTerminal(int(file.Fd())), nil
	default:
		return false, fmt.Errorf("color must be auto, always, or never")
	}
}

func paint(enabled bool, attributes ...color.Attribute) func(...any) string {
	c := color.New(attributes...)
	if enabled {
		c.EnableColor()
	} else {
		c.DisableColor()
	}
	return c.SprintFunc()
}

// startSpinner displays animated progress only on an interactive terminal. In
// redirected output it emits a stable line suitable for logs and scripts.
func startSpinner(writer io.Writer, message string, colored bool) func(bool) {
	file, terminal := writer.(*os.File)
	terminal = terminal && isTerminal(int(file.Fd()))
	if !terminal {
		fmt.Fprintln(writer, message+"…")
		return func(bool) {}
	}
	options := []spinner.Option{spinner.WithWriterFile(file), spinner.WithSuffix(" " + message + "…")}
	if colored {
		options = append(options, spinner.WithColor("cyan"))
	}
	indicator := spinner.New(spinner.CharSets[14], 90*time.Millisecond, options...)
	indicator.Start()
	return func(success bool) {
		if success {
			indicator.FinalMSG = paint(colored, color.FgGreen)("✓ " + message + " complete\n")
		} else {
			indicator.FinalMSG = paint(colored, color.FgRed)("✗ " + message + " failed\n")
		}
		indicator.Stop()
	}
}

// writeCredentials updates only the two Qobuz keys present in the target file.
// In particular, secrets supplied through environment variables are never
// copied into the configuration file as a side effect.
func writeCredentials(path string, creds qobuzauth.Credentials) error {
	settings := make(map[string]any)
	if raw, err := os.ReadFile(path); err == nil {
		if err = yaml.Unmarshal(raw, &settings); err != nil {
			return fmt.Errorf("decode configuration before credential update: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	settings["qobuz_app_id"] = creds.AppID
	settings["qobuz_user_auth_token"] = creds.Token
	raw, err := yaml.Marshal(settings)
	if err != nil {
		return fmt.Errorf("encode configuration: %w", err)
	}
	directory := filepath.Dir(path)
	if err = os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".qobuz-curator-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err = temporary.Chmod(0600); err == nil {
		_, err = temporary.Write(raw)
	}
	if err == nil {
		err = temporary.Sync()
	}
	closeErr := temporary.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write temporary configuration: %w", err)
	}
	if err = os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace configuration atomically: %w", err)
	}
	if err = os.Chmod(path, 0600); err != nil && !errors.Is(err, os.ErrPermission) {
		return err
	}
	return nil
}
func passwordCommand() *cobra.Command {
	return &cobra.Command{Use: "password-hash", Short: "Generate a password hash for web authentication", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		fmt.Fprint(cmd.ErrOrStderr(), "Password: ")
		first, e := readTerminalPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(cmd.ErrOrStderr())
		if e != nil {
			return e
		}
		fmt.Fprint(cmd.ErrOrStderr(), "Confirm: ")
		second, e := readTerminalPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(cmd.ErrOrStderr())
		if e != nil {
			return e
		}
		if strings.TrimSpace(string(first)) == "" || string(first) != string(second) {
			return fmt.Errorf("passwords do not match or are blank")
		}
		hash, e := security.HashPassword(string(first))
		if e == nil {
			fmt.Fprintln(cmd.OutOrStdout(), hash)
		}
		return e
	}}
}
