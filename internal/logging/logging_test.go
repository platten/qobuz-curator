package logging

import (
	"bytes"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestConsoleAndJSONLogging(t *testing.T) {
	var console bytes.Buffer
	logger, err := New("debug", "console", true, &console)
	if err != nil {
		t.Fatal(err)
	}
	logger.Debug("diagnostic", zap.String("component", "test"))
	Critical(logger, "critical condition", zap.Int("code", 7))
	if output := console.String(); !strings.Contains(output, "diagnostic") || !strings.Contains(output, "severity") || !strings.Contains(output, "\x1b[") {
		t.Fatal(output)
	}

	var jsonOutput bytes.Buffer
	logger, err = New("info", "json", true, &jsonOutput)
	if err != nil {
		t.Fatal(err)
	}
	logger.Debug("hidden")
	logger.Info("visible")
	if output := jsonOutput.String(); strings.Contains(output, "hidden") || !strings.Contains(output, `"message":"visible"`) || strings.Contains(output, "\x1b[") {
		t.Fatal(output)
	}
}

func TestLoggingValidation(t *testing.T) {
	if _, err := New("verbose", "console", false, &bytes.Buffer{}); err == nil {
		t.Fatal("invalid level accepted")
	}
	if _, err := New("info", "xml", false, &bytes.Buffer{}); err == nil {
		t.Fatal("invalid format accepted")
	}
	if _, err := New("info", "console", false, nil); err == nil {
		t.Fatal("nil output accepted")
	}
}
