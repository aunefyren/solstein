// Package exits is the VPN module: it runs WireGuard tunnels inside the
// process and offers them to the core as exits, so feeds can be fetched from
// other regions. Any WireGuard VPN works through .conf files; Proton is a
// convenience provider on top. The core never imports this package; main.go
// wires it in when VPN providers are configured.
package exits

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"aunefyren/solstein/settings"

	"golang.org/x/crypto/curve25519"
)

// Provider types.
const (
	TypeWireGuard = "wireguard"
	TypeProtonVPN = "protonvpn"
)

// Selection strategies within an exit's matching servers.
const (
	SelectionSticky      = "sticky"
	SelectionRandom      = "random"
	SelectionLeastFailed = "least-failed"
)

var (
	namePattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	countryPattern  = regexp.MustCompile(`^[A-Z]{2}$`)
	regionPattern   = regexp.MustCompile(`^[a-z][a-z-]*$`)
	errInvalid      = errors.New("invalid VPN configuration")
	filterModes     = []string{"exclude", "include", "only"}
	selectionModes  = []string{SelectionSticky, SelectionRandom, SelectionLeastFailed}
	countryAliases  = map[string]string{"UK": "GB"}
	reservedExitIDs = map[string]bool{"direct": true}
)

// Key is a WireGuard private key. It prints as "[redacted]" whatever verb
// or encoder is used, so it can't leak into a log line or API response by
// accident.
type Key struct {
	bytes [32]byte
}

func (Key) String() string               { return "[redacted]" }
func (Key) GoString() string             { return "[redacted]" }
func (Key) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }
func (Key) MarshalText() ([]byte, error) { return []byte("[redacted]"), nil }
func (key Key) Bytes() [32]byte          { return key.bytes }

// Public derives the public key. Used for tests and for logging which peer
// a tunnel talks to; the public key isn't secret.
func (key Key) Public() PublicKey {
	public, err := curve25519.X25519(key.bytes[:], curve25519.Basepoint)
	if err != nil {
		// Only fails for low-order points, which a private key can't give.
		panic("derive WireGuard public key: " + err.Error())
	}
	var result PublicKey
	copy(result.bytes[:], public)
	return result
}

// hex is the form the WireGuard device's configuration interface takes.
func (key Key) hex() string { return hex.EncodeToString(key.bytes[:]) }

// PublicKey is a WireGuard public key. Unlike Key it may be printed.
type PublicKey struct {
	bytes [32]byte
}

func (key PublicKey) String() string { return base64.StdEncoding.EncodeToString(key.bytes[:]) }
func (key PublicKey) hex() string    { return hex.EncodeToString(key.bytes[:]) }

// ParsePublicKey reads a base64 WireGuard public key.
func ParsePublicKey(text string) (PublicKey, error) {
	key, err := ParseKey(text)
	if err != nil {
		return PublicKey{}, err
	}
	return PublicKey{bytes: key.bytes}, nil
}

// ParseKey reads a base64 WireGuard key, as in a .conf file.
func ParseKey(text string) (Key, error) {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(text))
	if err != nil || len(decoded) != 32 {
		// Never include the text: it's a secret.
		return Key{}, errors.New("not a WireGuard key (32 bytes, base64)")
	}
	var key Key
	copy(key.bytes[:], decoded)
	return key, nil
}

// Location kinds, in order of precision.
const (
	LocationServer    = "server"
	LocationCity      = "city"
	LocationCountry   = "country"
	LocationArea      = "area"
	LocationContinent = "continent"
)

// Location is one entry of an exit's preference list.
type Location struct {
	Kind string
	// Country is the ISO 3166-1 alpha-2 code, for country and city.
	Country string
	// City, for city.
	City string
	// Name is the server name, area or continent.
	Name string
}

func (location Location) String() string {
	switch location.Kind {
	case LocationCountry:
		return location.Country
	case LocationCity:
		return location.Country + "/" + location.City
	default:
		return location.Kind + ":" + location.Name
	}
}

// ParseLocation reads "SE", "SE/Stockholm", "server:SE#12",
// "area:northern-europe" or "continent:europe".
func ParseLocation(text string) (Location, error) {
	text = strings.TrimSpace(text)
	if kind, rest, ok := strings.Cut(text, ":"); ok {
		rest = strings.TrimSpace(rest)
		switch strings.ToLower(kind) {
		case LocationServer:
			if rest == "" {
				return Location{}, errors.New("server: needs a server name")
			}
			return Location{Kind: LocationServer, Name: rest}, nil
		case LocationArea:
			name := strings.ToLower(rest)
			if !regionPattern.MatchString(name) || !validArea(name) {
				return Location{}, fmt.Errorf("unknown area %q; areas are UN M49 regions: %s", rest, strings.Join(areas(), ", "))
			}
			return Location{Kind: LocationArea, Name: name}, nil
		case LocationContinent:
			name := strings.ToLower(rest)
			if !regionPattern.MatchString(name) || !validContinent(name) {
				return Location{}, fmt.Errorf("unknown continent %q; one of %s", rest, strings.Join(continents(), ", "))
			}
			return Location{Kind: LocationContinent, Name: name}, nil
		default:
			return Location{}, fmt.Errorf("unknown location kind %q in %q", kind, text)
		}
	}
	countryText, city, hasCity := strings.Cut(text, "/")
	country, err := parseCountry(countryText)
	if err != nil {
		return Location{}, err
	}
	if hasCity {
		city = strings.TrimSpace(city)
		if city == "" {
			return Location{}, fmt.Errorf("%q has no city after the /", text)
		}
		return Location{Kind: LocationCity, Country: country, City: city}, nil
	}
	return Location{Kind: LocationCountry, Country: country}, nil
}

// parseCountry normalises an ISO 3166-1 alpha-2 code and checks it exists.
func parseCountry(text string) (string, error) {
	code := strings.ToUpper(strings.TrimSpace(text))
	if alias, ok := countryAliases[code]; ok {
		code = alias
	}
	if !countryPattern.MatchString(code) {
		return "", fmt.Errorf("%q is not a two-letter country code (ISO 3166-1, e.g. SE)", text)
	}
	if !knownCountry(code) {
		return "", fmt.Errorf("%q is not an ISO 3166-1 country code", code)
	}
	return code, nil
}

// ServerLocation is where a WireGuard server from a .conf file is.
type ServerLocation struct {
	Country string
	City    string
}

// Filter narrows a server-list provider's pool.
type Filter struct {
	Countries  []string
	Cities     []string
	Servers    []string
	SecureCore string
	Tor        string
}

// Provider is a validated provider.
type Provider struct {
	Name string
	Type string
	// protonvpn
	PrivateKeys []Key
	Tier        string
	Filter      Filter
	// wireguard: explicit files, or a directory scanned for *.conf.
	ConfigFiles []string
	ConfigDir   string
	// wireguard: server locations by .conf file name without extension.
	Locations  map[string]ServerLocation
	MaxTunnels int // zero means no limit
	// FallbackDNS are asked through the tunnel when its own DNS fails.
	FallbackDNS []netip.Addr
}

// Exit is a validated exit.
type Exit struct {
	Name      string
	Provider  string
	Locations []Location
	Strict    bool
	Exclude   []string
	Selection string
}

// Config is the module's validated configuration.
type Config struct {
	Providers map[string]Provider
	Exits     map[string]Exit
}

// Problem is one thing wrong with the VPN configuration. It disables the
// provider or exit it names; everything else keeps working.
type Problem struct {
	Subject string // e.g. "provider 'proton'"
	Err     error
}

func (problem Problem) Error() string {
	return problem.Subject + ": " + problem.Err.Error()
}

func (problem Problem) Unwrap() error {
	return problem.Err
}

// Load validates the vpn block of config.json and resolves its secrets.
// A provider or exit with a problem is left out and reported; the rest is
// returned ready to use. getenv is os.Getenv outside tests.
func Load(vpn settings.VPN, getenv func(string) string) (Config, []Problem) {
	config := Config{Providers: map[string]Provider{}, Exits: map[string]Exit{}}
	var problems []Problem

	for _, name := range slices.Sorted(maps.Keys(vpn.Providers)) {
		provider, err := loadProvider(name, vpn.Providers[name], getenv)
		if err != nil {
			problems = append(problems, Problem{Subject: "provider '" + name + "'", Err: err})
			continue
		}
		config.Providers[name] = provider
	}

	for _, name := range slices.Sorted(maps.Keys(vpn.Exits)) {
		exit, err := loadExit(name, vpn.Exits[name])
		if err == nil {
			if _, ok := config.Providers[exit.Provider]; !ok {
				if _, configured := vpn.Providers[exit.Provider]; configured {
					err = fmt.Errorf("its provider '%s' is disabled by an error above", exit.Provider)
				} else {
					err = fmt.Errorf("%w: provider '%s' is not configured", errInvalid, exit.Provider)
				}
			}
		}
		if err != nil {
			problems = append(problems, Problem{Subject: "exit '" + name + "'", Err: err})
			continue
		}
		config.Exits[name] = exit
	}
	return config, problems
}

func loadProvider(name string, raw settings.VPNProvider, getenv func(string) string) (Provider, error) {
	if !namePattern.MatchString(name) {
		return Provider{}, fmt.Errorf("%w: names are lowercase letters, digits, - and _, up to 32 characters", errInvalid)
	}
	if raw.MaxTunnels < 0 {
		return Provider{}, fmt.Errorf("%w: max_tunnels must not be negative", errInvalid)
	}
	provider := Provider{Name: name, Type: strings.ToLower(strings.TrimSpace(raw.Type)), MaxTunnels: raw.MaxTunnels}
	fallbackDNS, err := loadFallbackDNS(raw.FallbackDNS)
	if err != nil {
		return Provider{}, err
	}
	provider.FallbackDNS = fallbackDNS

	switch provider.Type {
	case TypeProtonVPN:
		if raw.ConfigFile != "" || len(raw.ConfigFiles) > 0 || raw.ConfigDir != "" || len(raw.Servers) > 0 {
			return Provider{}, fmt.Errorf("%w: config_file, config_files, config_dir and servers are for type wireguard", errInvalid)
		}
		if len(raw.PrivateKeys) == 0 {
			return Provider{}, fmt.Errorf("%w: private_keys needs at least one key", errInvalid)
		}
		for i, reference := range raw.PrivateKeys {
			value, err := settings.ResolveSecret(reference, getenv)
			if err != nil {
				return Provider{}, fmt.Errorf("private key %d: %w", i+1, err)
			}
			key, err := ParseKey(value)
			if err != nil {
				return Provider{}, fmt.Errorf("%w: private key %d: %w", errInvalid, i+1, err)
			}
			provider.PrivateKeys = append(provider.PrivateKeys, key)
		}
		if provider.MaxTunnels == 0 {
			provider.MaxTunnels = len(provider.PrivateKeys)
		}
		provider.Tier = strings.ToLower(strings.TrimSpace(raw.Tier))
		if provider.Tier == "" {
			provider.Tier = "plus"
		}
		if provider.Tier != "plus" && provider.Tier != "free" {
			return Provider{}, fmt.Errorf("%w: tier must be plus or free", errInvalid)
		}
		filter, err := loadFilter(raw.Filter)
		if err != nil {
			return Provider{}, err
		}
		provider.Filter = filter

	case TypeWireGuard:
		if len(raw.PrivateKeys) > 0 || raw.Tier != "" || raw.Filter != nil {
			return Provider{}, fmt.Errorf("%w: private_keys, tier and filter are for server-list providers; a .conf file carries its own key", errInvalid)
		}
		sources := 0
		for _, set := range []bool{raw.ConfigFile != "", len(raw.ConfigFiles) > 0, raw.ConfigDir != ""} {
			if set {
				sources++
			}
		}
		if sources != 1 {
			return Provider{}, fmt.Errorf("%w: set exactly one of config_file, config_files or config_dir", errInvalid)
		}
		provider.Locations = map[string]ServerLocation{}
		switch {
		case raw.ConfigFile != "":
			provider.ConfigFiles = []string{raw.ConfigFile}
			if raw.Country != "" {
				location, err := loadServerLocation(settings.VPNServerLocation{Country: raw.Country, City: raw.City})
				if err != nil {
					return Provider{}, err
				}
				provider.Locations[serverName(raw.ConfigFile)] = location
			}
		default:
			if raw.Country != "" || raw.City != "" {
				return Provider{}, fmt.Errorf("%w: country and city are for a single config_file; use servers for several", errInvalid)
			}
			provider.ConfigFiles = raw.ConfigFiles
			provider.ConfigDir = raw.ConfigDir
		}
		for server, raw := range raw.Servers {
			location, err := loadServerLocation(raw)
			if err != nil {
				return Provider{}, fmt.Errorf("server '%s': %w", server, err)
			}
			provider.Locations[server] = location
		}

	case "":
		return Provider{}, fmt.Errorf("%w: type is missing (wireguard or protonvpn)", errInvalid)
	default:
		return Provider{}, fmt.Errorf("%w: unknown type %q (wireguard or protonvpn)", errInvalid, raw.Type)
	}
	return provider, nil
}

func loadServerLocation(raw settings.VPNServerLocation) (ServerLocation, error) {
	country, err := parseCountry(raw.Country)
	if err != nil {
		return ServerLocation{}, fmt.Errorf("%w: %w", errInvalid, err)
	}
	return ServerLocation{Country: country, City: strings.TrimSpace(raw.City)}, nil
}

func loadFilter(raw *settings.VPNServerFilter) (Filter, error) {
	filter := Filter{SecureCore: "exclude", Tor: "exclude"}
	if raw == nil {
		return filter, nil
	}
	for _, text := range raw.Countries {
		country, err := parseCountry(text)
		if err != nil {
			return Filter{}, fmt.Errorf("%w: filter: %w", errInvalid, err)
		}
		filter.Countries = append(filter.Countries, country)
	}
	filter.Cities = trimAll(raw.Cities)
	filter.Servers = trimAll(raw.Servers)
	for _, mode := range []struct {
		name  string
		value string
		into  *string
	}{{"secure_core", raw.SecureCore, &filter.SecureCore}, {"tor", raw.Tor, &filter.Tor}} {
		value := strings.ToLower(strings.TrimSpace(mode.value))
		if value == "" {
			continue
		}
		if !slices.Contains(filterModes, value) {
			return Filter{}, fmt.Errorf("%w: filter %s must be exclude, include or only", errInvalid, mode.name)
		}
		*mode.into = value
	}
	return filter, nil
}

func loadExit(name string, raw settings.VPNExit) (Exit, error) {
	if !namePattern.MatchString(name) {
		return Exit{}, fmt.Errorf("%w: names are lowercase letters, digits, - and _, up to 32 characters", errInvalid)
	}
	if reservedExitIDs[name] {
		return Exit{}, fmt.Errorf("%w: '%s' is the built-in exit and can't be redefined", errInvalid, name)
	}
	exit := Exit{Name: name, Provider: strings.TrimSpace(raw.Provider), Strict: raw.Strict}
	if exit.Provider == "" {
		return Exit{}, fmt.Errorf("%w: provider is missing", errInvalid)
	}
	for _, text := range raw.Locations {
		location, err := ParseLocation(text)
		if err != nil {
			return Exit{}, fmt.Errorf("%w: location: %w", errInvalid, err)
		}
		exit.Locations = append(exit.Locations, location)
	}
	if exit.Strict && len(exit.Locations) == 0 {
		return Exit{}, fmt.Errorf("%w: strict needs at least one location", errInvalid)
	}
	for _, text := range raw.Exclude {
		country, err := parseCountry(text)
		if err != nil {
			return Exit{}, fmt.Errorf("%w: exclude: %w", errInvalid, err)
		}
		exit.Exclude = append(exit.Exclude, country)
	}
	exit.Selection = strings.ToLower(strings.TrimSpace(raw.Selection))
	if exit.Selection == "" {
		exit.Selection = SelectionSticky
	}
	if !slices.Contains(selectionModes, exit.Selection) {
		return Exit{}, fmt.Errorf("%w: selection must be sticky, random or least-failed", errInvalid)
	}
	return exit, nil
}

// serverName is a .conf file's server name: its file name without extension.
func serverName(path string) string {
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

func trimAll(values []string) []string {
	var result []string
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
}

// defaultFallbackDNS are public resolvers that answered every lookup through
// Proton tunnels in the US and Norway that Proton's own resolver failed
// (docs/exits.md).
var defaultFallbackDNS = []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("9.9.9.9")}

// loadFallbackDNS reads fallback_dns: unset is the default, an empty list
// none.
func loadFallbackDNS(raw *[]string) ([]netip.Addr, error) {
	if raw == nil {
		return defaultFallbackDNS, nil
	}
	addresses := make([]netip.Addr, 0, len(*raw))
	for _, value := range *raw {
		address, err := netip.ParseAddr(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("%w: fallback_dns: %q is not an IP address", errInvalid, value)
		}
		if slices.Contains(addresses, address) {
			return nil, fmt.Errorf("%w: fallback_dns: %s is listed twice", errInvalid, address)
		}
		addresses = append(addresses, address)
	}
	return addresses, nil
}
