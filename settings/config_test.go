package settings

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadMissingFileGivesDefaults(t *testing.T) {
	configDir := t.TempDir()

	cfg, err := Load(configDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != defaultPort || cfg.LogLevel != defaultLogLevel {
		t.Errorf("got %+v, want defaults", cfg)
	}
	if _, err := os.Stat(filepath.Join(configDir, configFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Error("Load wrote config.json; only Save should")
	}
}

func TestLoadKeepsExistingValuesAndFillsMissing(t *testing.T) {
	configDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(configDir, configFileName), []byte(`{"port": 9000}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(configDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 9000 {
		t.Errorf("port = %d, want 9000 from file", cfg.Port)
	}
	if cfg.LogLevel != defaultLogLevel {
		t.Errorf("log level = %q, want default filled in", cfg.LogLevel)
	}
}

func TestSaveWritesFile(t *testing.T) {
	configDir := t.TempDir()
	cfg := Config{Port: 9000, LogLevel: "debug"}

	if err := Save(configDir, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	path := filepath.Join(configDir, configFileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("config file not created: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config file permissions = %o, want 600", perm)
	}

	var onDisk Config
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatalf("parse written config: %v", err)
	}
	if onDisk != cfg {
		t.Errorf("on disk %+v, saved %+v", onDisk, cfg)
	}
}

func TestSaveSkipsUnchangedFile(t *testing.T) {
	configDir := t.TempDir()
	cfg := Config{Port: 9000, LogLevel: "info"}
	if err := Save(configDir, cfg); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configDir, configFileName)

	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := Save(configDir, cfg); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(old) {
		t.Errorf("config file was rewritten although nothing changed")
	}
}

func TestLoadRejectsInvalidJSON(t *testing.T) {
	configDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(configDir, configFileName), []byte(`{"port":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(configDir); err == nil {
		t.Error("expected an error for truncated JSON")
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name            string
		cfg             Config
		wantErr         bool
		wantExternalURL string
		wantLogLevel    string
	}{
		{name: "defaults", cfg: Config{Port: 8080, LogLevel: "info"}, wantLogLevel: "info"},
		{name: "port zero", cfg: Config{Port: 0, LogLevel: "info"}, wantErr: true},
		{name: "port too high", cfg: Config{Port: 70000, LogLevel: "info"}, wantErr: true},
		{name: "bad log level", cfg: Config{Port: 8080, LogLevel: "loud"}, wantErr: true},
		{name: "log level normalised", cfg: Config{Port: 8080, LogLevel: "WARNING"}, wantLogLevel: "warning"},
		{name: "external URL trailing slash trimmed", cfg: Config{Port: 8080, LogLevel: "info", ExternalURL: " http://solstein:8080/ "}, wantExternalURL: "http://solstein:8080", wantLogLevel: "info"},
		{name: "external URL without scheme", cfg: Config{Port: 8080, LogLevel: "info", ExternalURL: "solstein:8080"}, wantErr: true},
		{name: "external URL with other scheme", cfg: Config{Port: 8080, LogLevel: "info", ExternalURL: "ftp://solstein"}, wantErr: true},
		{name: "valid time zone", cfg: Config{Port: 8080, LogLevel: "info", Timezone: "Europe/Oslo"}, wantLogLevel: "info"},
		{name: "invalid time zone", cfg: Config{Port: 8080, LogLevel: "info", Timezone: "Mars/Olympus"}, wantErr: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := c.cfg
			err := cfg.Validate()
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if c.wantErr {
				return
			}
			if cfg.ExternalURL != c.wantExternalURL {
				t.Errorf("external URL = %q, want %q", cfg.ExternalURL, c.wantExternalURL)
			}
			if cfg.LogLevel != c.wantLogLevel {
				t.Errorf("log level = %q, want %q", cfg.LogLevel, c.wantLogLevel)
			}
		})
	}
}

func TestLocation(t *testing.T) {
	location, err := Config{}.Location()
	if err != nil || location != time.Local {
		t.Errorf("empty time zone = %v, %v; want time.Local", location, err)
	}

	location, err = Config{Timezone: "Europe/Oslo"}.Location()
	if err != nil || location.String() != "Europe/Oslo" {
		t.Errorf("Europe/Oslo = %v, %v", location, err)
	}
}
