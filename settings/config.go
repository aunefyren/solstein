// Package settings loads Solstein's settings. config.json in the config directory
// is the source of truth. Command-line flags and environment variables are
// ways to change it: at start-up they are applied on top of the file (flag
// wins over environment variable) and the result is written back, so the file
// always holds the configuration Solstein is actually running with.
package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	configFileName = "config.json"

	defaultPort     = 8080
	defaultLogLevel = "info"
)

// Config is the persisted configuration in config.json. Module settings get
// their own nested blocks here as the modules are built.
type Config struct {
	Port int `json:"port"`
	// ExternalURL is the address Audiobookshelf uses to reach Solstein. It is
	// the base for rewritten enclosure URLs, so it must be reachable from there.
	ExternalURL string `json:"external_url"`
	LogLevel    string `json:"log_level"`
	// Timezone is an IANA name such as "Europe/Oslo". Empty means the system
	// time zone, which in Docker comes from the standard TZ variable.
	Timezone string `json:"timezone"`
}

// Load reads config.json from configDir and fills in defaults for missing
// fields. A missing file yields the defaults. Nothing is written; see Save.
func Load(configDir string) (Config, error) {
	path := filepath.Join(configDir, configFileName)

	var cfg Config
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	if err == nil {
		if err := json.Unmarshal(existing, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse %s: %w", path, err)
		}
	}

	cfg.applyDefaults()
	return cfg, nil
}

// Save writes cfg to config.json in configDir if its content would change.
// Comparing bytes rather than tracking an "anything changed" flag means new
// fields and defaults are written out automatically, overrides that match the
// file cause no write, and an untouched file is never rewritten.
func Save(configDir string, cfg Config) error {
	path := filepath.Join(configDir, configFileName)

	updated, err := marshal(cfg)
	if err != nil {
		return err
	}
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if bytes.Equal(existing, updated) {
		return nil
	}
	return writeFileAtomic(path, updated)
}

// applyDefaults is the single source of default values, used both for a fresh
// config.json and for fields missing from an existing one.
func (cfg *Config) applyDefaults() {
	if cfg.Port == 0 {
		cfg.Port = defaultPort
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = defaultLogLevel
	}
}

// Validate normalises values and rejects ones Solstein can't run with. It runs
// after overrides, so a bad flag or environment variable stops start-up too.
func (cfg *Config) Validate() error {
	if cfg.Port < 1 || cfg.Port > 65535 {
		return fmt.Errorf("port %d is out of range 1-65535", cfg.Port)
	}

	level, err := logrus.ParseLevel(cfg.LogLevel)
	if err != nil {
		return fmt.Errorf("invalid log level %q: %w", cfg.LogLevel, err)
	}
	cfg.LogLevel = level.String()

	cfg.Timezone = strings.TrimSpace(cfg.Timezone)
	if _, err := cfg.Location(); err != nil {
		return err
	}

	cfg.ExternalURL = strings.TrimRight(strings.TrimSpace(cfg.ExternalURL), "/")
	if cfg.ExternalURL != "" {
		parsed, err := url.Parse(cfg.ExternalURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("external URL %q must be an absolute http(s) URL", cfg.ExternalURL)
		}
	}

	return nil
}

// Location returns the configured time zone, or time.Local when none is set.
func (cfg Config) Location() (*time.Location, error) {
	if cfg.Timezone == "" {
		return time.Local, nil
	}
	location, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return nil, fmt.Errorf("invalid time zone %q: %w", cfg.Timezone, err)
	}
	return location, nil
}

func marshal(cfg Config) ([]byte, error) {
	data, err := json.MarshalIndent(cfg, "", "\t")
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	return append(data, '\n'), nil
}

// writeFileAtomic writes through a temporary file and a rename, so a crash
// mid-write can't leave a truncated config.json behind. The file is 0600
// because it will hold secrets such as WireGuard private keys.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after a successful rename

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod %s: %w", tmpPath, err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
