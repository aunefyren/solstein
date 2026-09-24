package settings

import (
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envFrom(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func writeConfig(t *testing.T, configDir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(configDir, configFileName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolvePrecedence(t *testing.T) {
	cases := []struct {
		name     string
		file     string
		env      map[string]string
		args     []string
		wantPort int
	}{
		{name: "default", wantPort: defaultPort},
		{name: "file", file: `{"port": 9000}`, wantPort: 9000},
		{name: "env over file", file: `{"port": 9000}`, env: map[string]string{"SOLSTEIN_PORT": "9100"}, wantPort: 9100},
		{name: "flag over env", file: `{"port": 9000}`, env: map[string]string{"SOLSTEIN_PORT": "9100"}, args: []string{"-port", "9200"}, wantPort: 9200},
		{name: "empty env ignored", file: `{"port": 9000}`, env: map[string]string{"SOLSTEIN_PORT": ""}, wantPort: 9000},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			configDir := t.TempDir()
			if c.file != "" {
				writeConfig(t, configDir, c.file)
			}
			args := append([]string{"-configdir", configDir}, c.args...)

			cfg, _, err := Resolve(args, envFrom(c.env), io.Discard)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if cfg.Port != c.wantPort {
				t.Errorf("port = %d, want %d", cfg.Port, c.wantPort)
			}
		})
	}
}

func TestResolvePersistsOverrides(t *testing.T) {
	configDir := t.TempDir()
	writeConfig(t, configDir, `{"port": 9000, "log_level": "info"}`)
	env := envFrom(map[string]string{"SOLSTEIN_LOG_LEVEL": "debug"})

	if _, _, err := Resolve([]string{"-configdir", configDir, "-externalurl", "http://solstein:8080/"}, env, io.Discard); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	cfg, err := Load(configDir)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Port: 9000, LogLevel: "debug", ExternalURL: "http://solstein:8080"}
	if cfg != want {
		t.Errorf("config.json = %+v, want %+v (overrides saved, normalised, untouched fields kept)", cfg, want)
	}

	// With the flag and env var gone, the saved values remain.
	cfg, _, err = Resolve([]string{"-configdir", configDir}, envFrom(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg != want {
		t.Errorf("after removing overrides got %+v, want %+v", cfg, want)
	}
}

func TestResolveCreatesConfigFile(t *testing.T) {
	configDir := t.TempDir()

	if _, _, err := Resolve([]string{"-configdir", configDir}, envFrom(nil), io.Discard); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	cfg, err := Load(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != defaultPort || cfg.LogLevel != defaultLogLevel {
		t.Errorf("config.json = %+v, want defaults", cfg)
	}
}

func TestResolveInvalidOverrideLeavesFileAlone(t *testing.T) {
	configDir := t.TempDir()
	original := `{"port": 9000}`
	writeConfig(t, configDir, original)

	if _, _, err := Resolve([]string{"-configdir", configDir, "-port", "70000"}, envFrom(nil), io.Discard); err == nil {
		t.Fatal("expected a validation error")
	}

	data, err := os.ReadFile(filepath.Join(configDir, configFileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != original {
		t.Errorf("config.json changed after a failed override: %s", data)
	}
}

func TestResolveConfigDirFromEnv(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "nested")

	_, startup, err := Resolve(nil, envFrom(map[string]string{configDirEnv: configDir}), io.Discard)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if startup.ConfigDir != configDir {
		t.Errorf("data dir = %q, want %q", startup.ConfigDir, configDir)
	}
	if _, err := os.Stat(filepath.Join(configDir, configFileName)); err != nil {
		t.Errorf("config.json not created in data dir from env: %v", err)
	}
}

func TestResolveVersionSkipsConfig(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "unused")

	_, startup, err := Resolve([]string{"-configdir", configDir, "-version"}, envFrom(nil), io.Discard)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !startup.ShowVersion {
		t.Error("ShowVersion not set")
	}
	if _, err := os.Stat(configDir); !errors.Is(err, os.ErrNotExist) {
		t.Error("config directory was created for -version")
	}
}

func TestResolveErrors(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		args    []string
		wantErr string
	}{
		{name: "bad port flag", args: []string{"-port", "eighty"}, wantErr: "flag -port"},
		{name: "bad port env", env: map[string]string{"SOLSTEIN_PORT": "eighty"}, wantErr: "SOLSTEIN_PORT"},
		{name: "invalid value fails validation", args: []string{"-loglevel", "loud"}, wantErr: "invalid log level"},
		{name: "unknown flag", args: []string{"-nope"}, wantErr: "not defined"},
		{name: "stray argument", args: []string{"serve"}, wantErr: "unexpected argument"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := append([]string{"-configdir", t.TempDir()}, c.args...)
			_, _, err := Resolve(args, envFrom(c.env), io.Discard)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("err = %v, want it to contain %q", err, c.wantErr)
			}
		})
	}
}

func TestResolveHelp(t *testing.T) {
	var output strings.Builder
	_, _, err := Resolve([]string{"-h"}, envFrom(nil), &output)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("err = %v, want flag.ErrHelp", err)
	}
	for _, s := range settings {
		if !strings.Contains(output.String(), s.env) {
			t.Errorf("help text does not mention %s", s.env)
		}
	}
}
