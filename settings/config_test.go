package settings

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
	if !reflect.DeepEqual(onDisk, cfg) {
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

// validConfig is a config with every default applied.
func validConfig() Config {
	var cfg Config
	cfg.applyDefaults()
	return cfg
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		modify  func(cfg *Config)
		wantErr bool
		check   func(t *testing.T, cfg Config)
	}{
		{name: "defaults", modify: func(cfg *Config) {}},
		{name: "port zero", modify: func(cfg *Config) { cfg.Port = 0 }, wantErr: true},
		{name: "port too high", modify: func(cfg *Config) { cfg.Port = 70000 }, wantErr: true},
		{name: "bad log level", modify: func(cfg *Config) { cfg.LogLevel = "loud" }, wantErr: true},
		{name: "log level normalised", modify: func(cfg *Config) { cfg.LogLevel = "WARNING" }, check: func(t *testing.T, cfg Config) {
			if cfg.LogLevel != "warning" {
				t.Errorf("log level = %q", cfg.LogLevel)
			}
		}},
		{name: "external URL trailing slash trimmed", modify: func(cfg *Config) { cfg.ExternalURL = " http://solstein:8080/ " }, check: func(t *testing.T, cfg Config) {
			if cfg.ExternalURL != "http://solstein:8080" {
				t.Errorf("external URL = %q", cfg.ExternalURL)
			}
		}},
		{name: "external URL without scheme", modify: func(cfg *Config) { cfg.ExternalURL = "solstein:8080" }, wantErr: true},
		{name: "external URL with other scheme", modify: func(cfg *Config) { cfg.ExternalURL = "ftp://solstein" }, wantErr: true},
		{name: "valid time zone", modify: func(cfg *Config) { cfg.Timezone = "Europe/Oslo" }},
		{name: "invalid time zone", modify: func(cfg *Config) { cfg.Timezone = "Mars/Olympus" }, wantErr: true},
		{name: "empty token", modify: func(cfg *Config) { cfg.AuthToken = "" }, wantErr: true},
		{name: "short token", modify: func(cfg *Config) { cfg.AuthToken = "short" }, wantErr: true},
		{name: "token with slash", modify: func(cfg *Config) { cfg.AuthToken = "abcdefgh/ijklmnopq" }, wantErr: true},
		{name: "empty signing key", modify: func(cfg *Config) { cfg.URLSigningKey = " " }, wantErr: true},
		{name: "networks normalised", modify: func(cfg *Config) {
			cfg.AllowedClientNetworks = []string{" 172.18.0.5/16 ", "192.168.1.10", "", "fd00::1"}
		}, check: func(t *testing.T, cfg Config) {
			want := []string{"172.18.0.0/16", "192.168.1.10/32", "fd00::1/128"}
			if !reflect.DeepEqual(cfg.AllowedClientNetworks, want) {
				t.Errorf("networks = %v, want %v", cfg.AllowedClientNetworks, want)
			}
		}},
		{name: "bad network", modify: func(cfg *Config) { cfg.AllowedClientNetworks = []string{"lan"} }, wantErr: true},
		{name: "bad trusted proxy", modify: func(cfg *Config) { cfg.TrustedProxies = []string{"10.0.0.0/33"} }, wantErr: true},
		{name: "source hosts normalised", modify: func(cfg *Config) { cfg.AllowedSourceHosts = []string{" Feeds.Acast.com. ", ""} }, check: func(t *testing.T, cfg Config) {
			if !reflect.DeepEqual(cfg.AllowedSourceHosts, []string{"feeds.acast.com"}) {
				t.Errorf("hosts = %v", cfg.AllowedSourceHosts)
			}
		}},
		{name: "source host with path", modify: func(cfg *Config) { cfg.AllowedSourceHosts = []string{"acast.com/feeds"} }, wantErr: true},
		{name: "delivery mode normalised", modify: func(cfg *Config) { cfg.DeliveryMode = " Stream " }, check: func(t *testing.T, cfg Config) {
			if cfg.DeliveryMode != "stream" {
				t.Errorf("delivery mode = %q", cfg.DeliveryMode)
			}
		}},
		{name: "bad delivery mode", modify: func(cfg *Config) { cfg.DeliveryMode = "carrier-pigeon" }, wantErr: true},
		{name: "bad poll interval", modify: func(cfg *Config) { cfg.PollIntervalMinutes = -1 }, wantErr: true},
		{name: "bad retention", modify: func(cfg *Config) { cfg.CacheRetentionDays = -3 }, wantErr: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := validConfig()
			c.modify(&cfg)
			err := cfg.Validate()
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if c.check != nil && err == nil {
				c.check(t, cfg)
			}
		})
	}
}

func TestDefaultsGenerateSecrets(t *testing.T) {
	first, second := validConfig(), validConfig()
	if len(first.AuthToken) < 16 || len(first.URLSigningKey) < 32 {
		t.Errorf("secrets too short: token %q, key %q", first.AuthToken, first.URLSigningKey)
	}
	if first.AuthToken == second.AuthToken || first.URLSigningKey == second.URLSigningKey {
		t.Error("generated secrets repeat")
	}
	if err := first.Validate(); err != nil {
		t.Errorf("generated defaults don't validate: %v", err)
	}

	// Existing secrets are kept.
	kept := Config{AuthToken: "existing-token-1234", URLSigningKey: "existing-key"}
	kept.applyDefaults()
	if kept.AuthToken != "existing-token-1234" || kept.URLSigningKey != "existing-key" {
		t.Errorf("existing secrets replaced: %+v", kept)
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
