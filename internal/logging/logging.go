// Package logging configures the application's structured Zap logger.
package logging

import (
	"fmt"
	"io"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// New creates a production logger. Console output is human-oriented and can
// color levels; JSON output is intended for container log collectors.
func New(level, format string, color bool, output io.Writer) (*zap.Logger, error) {
	var minimum zapcore.Level
	if err := minimum.UnmarshalText([]byte(strings.ToLower(strings.TrimSpace(level)))); err != nil {
		return nil, fmt.Errorf("invalid log level %q: %w", level, err)
	}
	if output == nil {
		return nil, fmt.Errorf("log output must not be nil")
	}

	encoderConfig := zap.NewProductionEncoderConfig()
	encoderConfig.TimeKey = "time"
	encoderConfig.MessageKey = "message"
	encoderConfig.LevelKey = "level"
	encoderConfig.CallerKey = "caller"
	encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	encoderConfig.EncodeDuration = zapcore.StringDurationEncoder
	encoderConfig.EncodeCaller = zapcore.ShortCallerEncoder

	var encoder zapcore.Encoder
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "console":
		encoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder
		if color {
			encoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
		}
		encoder = zapcore.NewConsoleEncoder(encoderConfig)
	case "json":
		encoderConfig.EncodeLevel = zapcore.LowercaseLevelEncoder
		encoder = zapcore.NewJSONEncoder(encoderConfig)
	default:
		return nil, fmt.Errorf("invalid log format %q", format)
	}

	core := zapcore.NewCore(encoder, zapcore.AddSync(output), minimum)
	return zap.New(core, zap.AddCaller(), zap.AddStacktrace(zapcore.DPanicLevel)), nil
}

// Critical records a process-threatening condition without terminating the
// process. Lifecycle code remains responsible for cleanup and exit status.
func Critical(logger *zap.Logger, message string, fields ...zap.Field) {
	fields = append(fields, zap.String("severity", "critical"))
	logger.Error(message, fields...)
}
