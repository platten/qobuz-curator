package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/fatih/color"
	"github.com/pawel/qobuz-curator/internal/config"
	"github.com/pawel/qobuz-curator/internal/security"
	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"
)

// configPrompter owns all interactive input so initialization is testable with
// redirected streams as well as safe to use in a real terminal.
type configPrompter struct {
	reader *bufio.Reader
	input  io.Reader
	output io.Writer
}

func initCommand(o *options) *cobra.Command {
	var interactive, force bool
	cmd := &cobra.Command{
		Use:     "init",
		Short:   "Create a configuration file",
		Long:    "Create a complete configuration file in the platform-native user\nconfiguration directory. Interactive mode prompts for settings and hides secret input\nwhen connected to a terminal.",
		Example: "  qobuz-curator init\n  qobuz-curator init --interactive\n  qobuz-curator init --config ./qobuz-curator.yaml",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Initial()
			if err != nil {
				return err
			}
			if interactive {
				prompter := &configPrompter{reader: bufio.NewReader(cmd.InOrStdin()), input: cmd.InOrStdin(), output: cmd.OutOrStdout()}
				if err = prompter.configure(&cfg); err != nil {
					return fmt.Errorf("interactive configuration: %w", err)
				}
			}
			if err = cfg.Validate(); err != nil {
				return fmt.Errorf("invalid configuration: %w", err)
			}
			target := configSource(o.configFile)
			if err = writeInitialConfig(target, cfg, force); err != nil {
				return err
			}
			colored, err := useColor(o.colorMode, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), paint(colored, color.FgGreen, color.Bold)("✓ Configuration created:"), target)
			fmt.Fprintln(cmd.OutOrStdout(), "Next: configure Qobuz with \"qobuz-curator auth --write-config\", then run \"qobuz-curator serve\".")
			return nil
		},
	}
	cmd.Flags().BoolVarP(&interactive, "interactive", "i", false, "prompt for configuration values")
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing configuration file")
	return cmd
}

func (p *configPrompter) configure(cfg *config.Config) error {
	var err error
	if cfg.DataDir, err = p.text("Data directory", cfg.DataDir); err != nil {
		return err
	}
	if cfg.DatabaseName, err = p.text("Database file name", cfg.DatabaseName); err != nil {
		return err
	}
	if cfg.Host, err = p.text("HTTP listen host", cfg.Host); err != nil {
		return err
	}
	allowed, err := p.text("Allowed host names or IPs (comma-separated)", strings.Join(cfg.AllowedHosts, ","))
	if err != nil {
		return err
	}
	cfg.AllowedHosts = splitCommaSeparated(allowed)
	if cfg.Port, err = p.integer("HTTP port (0 selects a free high port)", cfg.Port); err != nil {
		return err
	}
	if cfg.Provider, err = p.text("Provider (fake or qobuz)", cfg.Provider); err != nil {
		return err
	}
	if cfg.QobuzAppID, err = p.text("Qobuz application ID", cfg.QobuzAppID); err != nil {
		return err
	}
	if cfg.QobuzUserToken, err = p.secret("Qobuz user authentication token"); err != nil {
		return err
	}
	if cfg.QobuzAPIBase, err = p.text("Qobuz API base URL", cfg.QobuzAPIBase); err != nil {
		return err
	}
	if cfg.OpenAIAPIKey, err = p.secret("OpenAI API key"); err != nil {
		return err
	}
	if cfg.OpenAIModel, err = p.text("OpenAI model", cfg.OpenAIModel); err != nil {
		return err
	}
	if cfg.OpenAIAPIBase, err = p.text("OpenAI API base URL", cfg.OpenAIAPIBase); err != nil {
		return err
	}
	if cfg.MatchThreshold, err = p.decimal("Match confidence threshold", cfg.MatchThreshold); err != nil {
		return err
	}
	if cfg.PreviewTTLHours, err = p.integer("Preview lifetime in hours", cfg.PreviewTTLHours); err != nil {
		return err
	}
	if cfg.AuthDisabled, err = p.boolean("Disable web authentication", cfg.AuthDisabled); err != nil {
		return err
	}
	if !cfg.AuthDisabled {
		password, passwordErr := p.secret("Web interface password")
		if passwordErr != nil {
			return passwordErr
		}
		confirmation, confirmationErr := p.secret("Confirm web interface password")
		if confirmationErr != nil {
			return confirmationErr
		}
		if strings.TrimSpace(password) == "" || password != confirmation {
			return fmt.Errorf("web interface passwords do not match or are blank")
		}
		if cfg.PasswordHash, err = security.HashPassword(password); err != nil {
			return fmt.Errorf("hash web interface password: %w", err)
		}
	}
	if cfg.SessionTTLHours, err = p.integer("Login session lifetime in hours", cfg.SessionTTLHours); err != nil {
		return err
	}
	if cfg.SecureCookies, err = p.boolean("Require HTTPS cookies", cfg.SecureCookies); err != nil {
		return err
	}
	if cfg.BrowserDisabled, err = p.boolean("Disable automatic browser opening", cfg.BrowserDisabled); err != nil {
		return err
	}
	if cfg.LogLevel, err = p.text("Log level (debug, info, warn, or error)", cfg.LogLevel); err != nil {
		return err
	}
	if cfg.LogFormat, err = p.text("Log format (console or json)", cfg.LogFormat); err != nil {
		return err
	}
	if cfg.LogColor, err = p.boolean("Color console logs", cfg.LogColor); err != nil {
		return err
	}
	return nil
}

func (p *configPrompter) text(label, current string) (string, error) {
	fmt.Fprintf(p.output, "%s [%s]: ", label, current)
	value, err := p.line()
	if err != nil {
		return "", err
	}
	if value == "" {
		return current, nil
	}
	return value, nil
}

func (p *configPrompter) secret(label string) (string, error) {
	fmt.Fprintf(p.output, "%s [leave blank to omit]: ", label)
	if file, ok := p.input.(*os.File); ok && isTerminal(int(file.Fd())) {
		value, err := readTerminalPassword(int(file.Fd()))
		fmt.Fprintln(p.output)
		return string(value), err
	}
	return p.line()
}

func (p *configPrompter) line() (string, error) {
	value, err := p.reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if errors.Is(err, io.EOF) && value == "" {
		return "", io.ErrUnexpectedEOF
	}
	return strings.TrimSpace(value), nil
}

func (p *configPrompter) integer(label string, current int) (int, error) {
	value, err := p.text(label, strconv.Itoa(current))
	if err != nil {
		return 0, err
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a whole number", label)
	}
	return parsed, nil
}

func (p *configPrompter) decimal(label string, current float64) (float64, error) {
	value, err := p.text(label, strconv.FormatFloat(current, 'f', -1, 64))
	if err != nil {
		return 0, err
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number", label)
	}
	return parsed, nil
}

func (p *configPrompter) boolean(label string, current bool) (bool, error) {
	defaultValue := "no"
	if current {
		defaultValue = "yes"
	}
	value, err := p.text(label+" (yes/no)", defaultValue)
	if err != nil {
		return false, err
	}
	switch strings.ToLower(value) {
	case "yes", "y", "true", "1":
		return true, nil
	case "no", "n", "false", "0":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be yes or no", label)
	}
}

func splitCommaSeparated(value string) []string {
	if strings.TrimSpace(value) == "" {
		return []string{}
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func writeInitialConfig(path string, cfg config.Config, force bool) error {
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode configuration: %w", err)
	}
	raw = append([]byte("# Generated by qobuz-curator init. Keep credentials private.\n"), raw...)
	directory := filepath.Dir(path)
	if err = os.MkdirAll(directory, 0700); err != nil {
		return fmt.Errorf("create configuration directory: %w", err)
	}
	if !force {
		file, openErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if openErr != nil {
			if errors.Is(openErr, os.ErrExist) {
				return fmt.Errorf("configuration file already exists: %s (use --force to replace it)", path)
			}
			return fmt.Errorf("create configuration file: %w", openErr)
		}
		if err = writeAndSync(file, raw); err != nil {
			_ = os.Remove(path)
			return fmt.Errorf("write configuration file: %w", err)
		}
		return protectConfigFile(path)
	}

	temporary, err := os.CreateTemp(directory, ".qobuz-curator-init-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary configuration file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()
	if err = temporary.Chmod(0600); err != nil && !errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("protect temporary configuration file: %w", err)
	}
	if err = writeAndSync(temporary, raw); err != nil {
		return fmt.Errorf("write temporary configuration file: %w", err)
	}
	if err = os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace configuration file atomically: %w", err)
	}
	return protectConfigFile(path)
}

func writeAndSync(file *os.File, raw []byte) error {
	_, err := file.Write(raw)
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

func protectConfigFile(path string) error {
	err := os.Chmod(path, 0600)
	if err != nil && !errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("protect configuration file: %w", err)
	}
	return nil
}
