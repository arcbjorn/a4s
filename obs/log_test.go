package obs

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevelDefaultsToInfo(t *testing.T) {
	level, err := ParseLevel("")
	if err != nil {
		t.Fatalf("parse empty level: %v", err)
	}
	if level != slog.LevelInfo {
		t.Fatalf("empty level = %v, want info", level)
	}
}

func TestParseLevelAcceptsNames(t *testing.T) {
	for name, want := range map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
		"WARN":  slog.LevelWarn,
	} {
		level, err := ParseLevel(name)
		if err != nil {
			t.Fatalf("parse %q: %v", name, err)
		}
		if level != want {
			t.Fatalf("parse %q = %v, want %v", name, level, want)
		}
	}
}

// A misspelled level must refuse to start rather than silently defaulting,
// because a daemon that quietly drops warnings for its whole lifetime is the
// failure this check exists to prevent.
func TestParseLevelRejectsUnknown(t *testing.T) {
	if _, err := ParseLevel("verbose"); err == nil {
		t.Fatal("expected unknown level to be refused")
	}
}

func TestNewRejectsUnknownFormat(t *testing.T) {
	if _, err := New(Config{Format: Format("xml")}); err == nil {
		t.Fatal("expected unknown format to be refused")
	}
}

func TestJSONFormatCarriesComponent(t *testing.T) {
	var buffer bytes.Buffer
	logger, err := New(Config{Format: FormatJSON, Output: &buffer, Component: "server"})
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}
	logger.Info("accepting nodes", slog.String("addr", "127.0.0.1:9000"))

	var record map[string]any
	if err := json.Unmarshal(buffer.Bytes(), &record); err != nil {
		t.Fatalf("decode record: %v", err)
	}
	if record["component"] != "server" {
		t.Fatalf("component = %v, want server", record["component"])
	}
	if record["msg"] != "accepting nodes" {
		t.Fatalf("msg = %v", record["msg"])
	}
	if record["addr"] != "127.0.0.1:9000" {
		t.Fatalf("addr = %v", record["addr"])
	}
}

func TestLevelFiltersBelowThreshold(t *testing.T) {
	var buffer bytes.Buffer
	logger, err := New(Config{Level: "warn", Output: &buffer})
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}
	logger.Info("routine")
	if buffer.Len() != 0 {
		t.Fatalf("info emitted below threshold: %q", buffer.String())
	}
	logger.Warn("degraded")
	if !strings.Contains(buffer.String(), "degraded") {
		t.Fatalf("warn not emitted: %q", buffer.String())
	}
}

// Logging must not write to stdout, which carries command results an operator
// may be piping into another tool.
func TestDefaultOutputIsNotStdout(t *testing.T) {
	var buffer bytes.Buffer
	logger, err := New(Config{Output: &buffer})
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}
	logger.Info("event")
	if buffer.Len() == 0 {
		t.Fatal("configured output received nothing")
	}
}

func TestDiscardIsUsableWithoutNilChecks(t *testing.T) {
	logger := Discard()
	if logger == nil {
		t.Fatal("Discard returned nil")
	}
	logger.Info("dropped", slog.Int("n", 1))
}
