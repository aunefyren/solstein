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
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	configFileName = "config.json"

	defaultPort         = 8080
	defaultLogLevel     = "info"
	defaultDeliveryMode = "cache"
	// defaultProcessingWaitSeconds matches episodes' own default: long
	// enough for a diff of a normal episode, inside ABS's 30-second timeout.
	defaultProcessingWaitSeconds = 20
	// maxProcessingWaitSeconds caps the wait at an hour: no client holds a
	// request that long, so a larger value is a mistake.
	maxProcessingWaitSeconds  = 3600
	defaultPollInterval       = 15
	defaultCacheRetentionDays = 14
)

// DeliveryModes are the valid values for delivery_mode; see docs/episodes.md.
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

	// DefaultExit is the exit for feeds (and other requests) that don't name
	// one; empty means direct, or with the direct exit off, the first VPN
	// exit by name.
	DefaultExit string `json:"default_exit"`
	// DirectExit is whether the direct exit (this host's own connection)
	// exists: "on", "off", or "auto" (the default), which is off as soon as
	// config.json sets up VPN exits, so nothing leaves from the host's own
	// address once there is a VPN to use; see DirectExitOff.
	DirectExit string `json:"direct_exit"`
	// DisableDirect is the setting direct_exit replaced. Read from older
	// config.json files and migrated (true becomes "off"; false, the old
	// default, becomes "auto"), then dropped from the file.
	DisableDirect *bool `json:"disable_direct,omitempty"`
	// HomeCountry is the country (ISO 3166-1 alpha-2) this host's own
	// connection comes out in, as the operator declares it; empty means
	// unknown. Solstein can't find it out itself without an outside
	// geolocation service. With it set, region diff's same-country checks
	// treat the direct exit as coming out there.
	HomeCountry string `json:"home_country"`

	// SkipTrackingRedirects fetches episodes from the audio host directly,
	// skipping the tracking redirects (Podtrac, Chartable, …) chained in
	// front of their URLs. Off by default: shows count their downloads
	// through them. A tracking redirect that fails is skipped either way.
	SkipTrackingRedirects bool `json:"skip_tracking_redirects"`

	// DeliveryMode is the default for feeds that don't set their own.
	DeliveryMode        string `json:"delivery_mode"`
	PollIntervalMinutes int    `json:"poll_interval_minutes"`
	CacheRetentionDays  int    `json:"cache_retention_days"`

	// PrepareAhead keeps every episode that needs preparing — a download in
	// cache mode, a processor's work — out of the feed until its file exists,
	// and queues a new feed's whole backlog at once, instead of preparing
	// episodes when a client asks for them. For a client that downloads each
	// episode once (Audiobookshelf) nothing is then ever waited for, which is
	// what makes region_diff.pair_downloads "in_turn" usable. Off by default:
	// on demand is what a client streaming every play needs. Per feed:
	// models.Feed.PrepareAhead.
	PrepareAhead bool `json:"prepare_ahead"`

	// ProcessingWaitSeconds is how long a client waiting for an episode that
	// is being prepared on request is held before it gets 503 and
	// Retry-After, while the work carries on. The default, 20, stays inside
	// Audiobookshelf's own 30-second download timeout. Raise it only
	// together with the client's timeout (ABS:
	// PODCAST_DOWNLOAD_TIMEOUT) — held longer than the client allows, the
	// request fails on its side instead.
	ProcessingWaitSeconds int `json:"processing_wait_seconds"`

	// VPN configures the exits module; validated by the module itself.
	VPN VPN `json:"vpn"`
	// RegionDiff configures the region-diff module.
	RegionDiff RegionDiff `json:"region_diff"`
}

// DirectExitSettings are the valid values for direct_exit.
var DirectExitSettings = []string{"auto", "on", "off"}

// DirectExitOff reports whether the direct exit is off: set to "off", or
// "auto" with VPN exits set up in config.json. Whether those exits load
// doesn't matter: a VPN that fails must never mean traffic quietly going
// out directly instead.
func (cfg Config) DirectExitOff() bool {
	return cfg.DirectExit == "off" || (cfg.DirectExit != "on" && len(cfg.VPN.Exits) > 0)
}

// directExitReason explains an "auto" that turned the direct exit off, for
// messages; empty otherwise.
func (cfg Config) directExitReason() string {
	if cfg.DirectExit != "off" && cfg.DirectExitOff() {
		return ", because config.json sets up VPN exits"
	}
	return ""
}

// DirectExitSummary says whether and why the direct exit is off, for the
// start-up log.
func (cfg Config) DirectExitSummary() string {
	switch {
	case !cfg.DirectExitOff():
		return ""
	case cfg.DirectExit == "off":
		return "The direct exit is off (direct_exit: off): nothing goes out on this host's own connection except the VPN tunnels themselves."
	default:
		return "The direct exit is off (direct_exit: auto, because config.json sets up VPN exits): nothing goes out on this host's own connection except the VPN tunnels themselves. Set direct_exit to on to allow it."
	}
}

// countryCode is an ISO 3166-1 alpha-2 code, upper-cased.
var countryCode = regexp.MustCompile(`^[A-Z]{2}$`)

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
	if cfg.DisableDirect != nil {
		if cfg.DirectExit == "" {
			cfg.DirectExit = "auto"
			if *cfg.DisableDirect {
				cfg.DirectExit = "off"
			}
		}
		cfg.DisableDirect = nil
	}
	if cfg.DirectExit == "" {
		cfg.DirectExit = "auto"
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
	if cfg.ProcessingWaitSeconds == 0 {
		cfg.ProcessingWaitSeconds = defaultProcessingWaitSeconds
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
	// Written as {} so config.json shows where VPN settings go.
	if cfg.VPN.Providers == nil {
		cfg.VPN.Providers = map[string]VPNProvider{}
	}
	if cfg.VPN.Exits == nil {
		cfg.VPN.Exits = map[string]VPNExit{}
	}
	cfg.RegionDiff.applyDefaults()
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

	cfg.DefaultExit = strings.TrimSpace(cfg.DefaultExit)
	cfg.DirectExit = strings.ToLower(strings.TrimSpace(cfg.DirectExit))
	if !slices.Contains(DirectExitSettings, cfg.DirectExit) {
		return fmt.Errorf("direct_exit %q must be one of %s", cfg.DirectExit, strings.Join(DirectExitSettings, ", "))
	}
	if cfg.DirectExitOff() && cfg.DefaultExit == "direct" {
		return fmt.Errorf("default_exit is direct, but the direct exit is off (direct_exit: %s%s); set direct_exit to on to use it", cfg.DirectExit, cfg.directExitReason())
	}

	cfg.HomeCountry = strings.ToUpper(strings.TrimSpace(cfg.HomeCountry))
	if cfg.HomeCountry != "" && !countryCode.MatchString(cfg.HomeCountry) {
		return fmt.Errorf("home_country %q must be a two-letter country code, such as NO", cfg.HomeCountry)
	}

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
	// An hour is far more than any client allows; beyond it the setting is
	// more likely a mistake (milliseconds, say) than an intention.
	if cfg.ProcessingWaitSeconds < 1 || cfg.ProcessingWaitSeconds > maxProcessingWaitSeconds {
		return fmt.Errorf("processing wait must be between 1 and %d seconds, got %d", maxProcessingWaitSeconds, cfg.ProcessingWaitSeconds)
	}

	if err := cfg.RegionDiff.validate(); err != nil {
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
