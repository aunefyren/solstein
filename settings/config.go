// Package settings loads Solstein's settings. config.json in the config directory
// is the source of truth. Command-line flags and environment variables are
// ways to change it: at start-up they are applied on top of the file (flag
// wins over environment variable) and the result is written back, so the file
// always holds the configuration Solstein is actually running with.
package settings

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	configFileName = "config.json"

	defaultPort               = 8080
	defaultLogLevel           = "info"
	defaultDeliveryMode       = "cache"
	defaultPollInterval       = 15
	defaultCacheRetentionDays = 14
)

// DeliveryModes are the valid values for delivery_mode; see docs/design.md.
var DeliveryModes = []string{"cache", "stream", "original"}

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
	// AllowPrivateDestinations lets Solstein fetch from loopback, private and
	// other non-public addresses. Off by default, so the proxy can't be used
	// to reach the operator's internal network.
	AllowPrivateDestinations bool `json:"allow_private_destinations"`

	// DisableAuth turns off the subscribe token and URL signatures, for
	// private-network-only setups. Named as a negative so that a missing field
	// means auth stays on.
	DisableAuth bool `json:"disable_auth"`
	// AuthToken is the subscribe token; generated on first run.
	AuthToken string `json:"auth_token"`
	// URLSigningKey signs the feed and episode URLs Solstein writes out;
	// generated on first run. Changing it invalidates every subscribed URL.
	URLSigningKey string `json:"url_signing_key"`
	// AllowedClientNetworks are the CIDRs allowed to use Solstein at all;
	// empty allows any address. Checked in addition to the token.
	AllowedClientNetworks []string `json:"allowed_client_networks"`
	// TrustedProxies are the CIDRs of reverse proxies whose X-Forwarded-For
	// is believed when working out the client address.
	TrustedProxies []string `json:"trusted_proxies"`
	// AllowedSourceHosts limits which hosts feeds can be subscribed from; a
	// name also allows its subdomains. Empty allows any host.
	AllowedSourceHosts []string `json:"allowed_source_hosts"`

	// DeliveryMode is the default for feeds that don't set their own.
	DeliveryMode        string `json:"delivery_mode"`
	PollIntervalMinutes int    `json:"poll_interval_minutes"`
	CacheRetentionDays  int    `json:"cache_retention_days"`
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
	// Generated secrets are persisted by the first Save, so they survive
	// restarts; a new signing key would break every subscribed URL.
	if cfg.AuthToken == "" {
		cfg.AuthToken = rand.Text()
	}
	if cfg.URLSigningKey == "" {
		cfg.URLSigningKey = rand.Text() + rand.Text()
	}
	// Empty lists are written as [] rather than null, so config.json shows
	// the setting exists.
	if cfg.AllowedClientNetworks == nil {
		cfg.AllowedClientNetworks = []string{}
	}
	if cfg.TrustedProxies == nil {
		cfg.TrustedProxies = []string{}
	}
	if cfg.AllowedSourceHosts == nil {
		cfg.AllowedSourceHosts = []string{}
	}
	if cfg.DeliveryMode == "" {
		cfg.DeliveryMode = defaultDeliveryMode
	}
	if cfg.PollIntervalMinutes == 0 {
		cfg.PollIntervalMinutes = defaultPollInterval
	}
	if cfg.CacheRetentionDays == 0 {
		cfg.CacheRetentionDays = defaultCacheRetentionDays
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

	if strings.TrimSpace(cfg.AuthToken) == "" || strings.TrimSpace(cfg.URLSigningKey) == "" {
		return errors.New("auth token and URL signing key must not be empty")
	}
	if len(cfg.AuthToken) < 16 {
		return errors.New("auth token must be at least 16 characters")
	}
	if strings.ContainsAny(cfg.AuthToken, "/?#%& ") {
		return errors.New("auth token must not contain / ? # % & or spaces, since it is part of URLs")
	}

	if cfg.AllowedClientNetworks, err = normaliseNetworks(cfg.AllowedClientNetworks); err != nil {
		return fmt.Errorf("allowed client networks: %w", err)
	}
	if cfg.TrustedProxies, err = normaliseNetworks(cfg.TrustedProxies); err != nil {
		return fmt.Errorf("trusted proxies: %w", err)
	}
	hosts := make([]string, 0, len(cfg.AllowedSourceHosts))
	for _, host := range cfg.AllowedSourceHosts {
		host = strings.Trim(strings.ToLower(strings.TrimSpace(host)), ".")
		if host == "" {
			continue
		}
		if strings.ContainsAny(host, "/:?# ") {
			return fmt.Errorf("allowed source host %q must be a host name only", host)
		}
		hosts = append(hosts, host)
	}
	cfg.AllowedSourceHosts = hosts

	cfg.DeliveryMode = strings.ToLower(strings.TrimSpace(cfg.DeliveryMode))
	if !slices.Contains(DeliveryModes, cfg.DeliveryMode) {
		return fmt.Errorf("delivery mode %q must be one of %s", cfg.DeliveryMode, strings.Join(DeliveryModes, ", "))
	}
	if cfg.PollIntervalMinutes < 1 {
		return fmt.Errorf("poll interval must be at least 1 minute, got %d", cfg.PollIntervalMinutes)
	}
	if cfg.CacheRetentionDays < 1 {
		return fmt.Errorf("cache retention must be at least 1 day, got %d", cfg.CacheRetentionDays)
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

// normaliseNetworks validates CIDRs, accepting bare IPs as single-address
// networks, and returns them in canonical form.
func normaliseNetworks(networks []string) ([]string, error) {
	result := make([]string, 0, len(networks))
	for _, network := range networks {
		network = strings.TrimSpace(network)
		if network == "" {
			continue
		}
		if ip, err := netip.ParseAddr(network); err == nil {
			result = append(result, netip.PrefixFrom(ip, ip.BitLen()).String())
			continue
		}
		prefix, err := netip.ParsePrefix(network)
		if err != nil {
			return nil, fmt.Errorf("%q is not an IP address or CIDR", network)
		}
		result = append(result, prefix.Masked().String())
	}
	return result, nil
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
