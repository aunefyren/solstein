package logger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
)

// restoreLog puts the package-level Log back after a test replaces it, so the
// change doesn't leak into other tests in the same binary.
func restoreLog(t *testing.T) {
	t.Helper()
	original := Log
	t.Cleanup(func() { Log = original })
}

func TestInitWritesToFile(t *testing.T) {
	restoreLog(t)
	configDir := t.TempDir()

	closer, err := Init(configDir, "debug")
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if Log.GetLevel() != logrus.DebugLevel {
		t.Errorf("level = %s, want debug", Log.GetLevel())
	}
	Log.Info("hello from test")
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(configDir, logFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "[INFO]: ") || !strings.Contains(string(data), "hello from test") {
		t.Errorf("log file content = %q", data)
	}
}

func TestInitErrors(t *testing.T) {
	restoreLog(t)

	if _, err := Init(t.TempDir(), "loud"); err == nil {
		t.Error("expected an error for an invalid level")
	}
	if _, err := Init(filepath.Join(t.TempDir(), "missing"), "info"); err == nil {
		t.Error("expected an error for a missing config directory")
	}
}
