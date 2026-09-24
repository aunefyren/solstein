package exits

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// protonSnapshot is the Proton server list Solstein ships with; see
// data/README.md for where it comes from and how to update it.
//
//go:embed data/protonvpn.json.gz
var protonSnapshot []byte

const (
	// protonListURL is where the list is refreshed from: gluetun-servers,
	// which gluetun's maintainers update monthly.
	protonListURL = "https://raw.githubusercontent.com/qdm12/gluetun-servers/main/pkg/servers/protonvpn.json"
	// protonSchemaVersion is the list format this code understands.
	protonSchemaVersion = 4
	// minimumProtonServers guards against accepting a truncated or broken
	// download in place of a working list.
	minimumProtonServers = 100
)

// Proton's WireGuard settings, the same for every server and key.
var (
	protonAddress = netip.MustParseAddr("10.2.0.2")
	protonDNS     = netip.MustParseAddr("10.2.0.1")
)

const protonPort = 51820

// protonCountryNames maps the country names Proton's list uses where they
// differ from the UN names in the countries table. Every name in the
// embedded list is checked to map somewhere by a test.
var protonCountryNames = map[string]string{
	"Bolivia":             "BO",
	"Cote d'Ivoire":       "CI",
	"Czech Republic":      "CZ",
	"Hong Kong":           "HK",
	"Korea":               "KR",
	"Macao":               "MO",
	"Macedonia":           "MK",
	"Moldova":             "MD",
	"Netherlands":         "NL",
	"Palestine, State of": "PS",
	"Tanzania":            "TZ",
	"Turkey":              "TR",
	"United Kingdom":      "GB",
	"United States":       "US",
	"Venezuela":           "VE",
}

var nonLetters = regexp.MustCompile(`[^a-z]`)

func normaliseName(name string) string {
	return nonLetters.ReplaceAllString(strings.ToLower(name), "")
}

// countryCodesByName maps normalised UN names to codes, built once.
var countryCodesByName = sync.OnceValue(func() map[string]string {
	byName := make(map[string]string, len(countries))
	for code, country := range countries {
		byName[normaliseName(country.name)] = code
	}
	return byName
})

// countryByName finds the ISO code for a country name from a server list.
func countryByName(name string) (string, bool) {
	if code, ok := protonCountryNames[name]; ok {
		return code, true
	}
	code, ok := countryCodesByName()[normaliseName(name)]
	return code, ok
}

// protonList is gluetun-servers' protonvpn.json.
type protonList struct {
	Version   int            `json:"version"`
	Timestamp int64          `json:"timestamp"`
	Servers   []protonServer `json:"servers"`
}

type protonServer struct {
	VPN        string   `json:"vpn"`
	Country    string   `json:"country"`
	City       string   `json:"city"`
	ServerName string   `json:"server_name"`
	Hostname   string   `json:"hostname"`
	PublicKey  string   `json:"wgpubkey"`
	IPs        []string `json:"ips"`
	Free       bool     `json:"free"`
	SecureCore bool     `json:"secure_core"`
	Tor        bool     `json:"tor"`
}

// UpdatedAt is when gluetun's maintainers generated the list.
func (list protonList) UpdatedAt() time.Time {
	return time.Unix(list.Timestamp, 0)
}

// parseProtonList reads a list, gzip-compressed or not, and refuses one
// whose format this code doesn't know or that looks broken.
func parseProtonList(data []byte) (protonList, error) {
	if bytes.HasPrefix(data, []byte{0x1f, 0x8b}) {
		reader, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return protonList{}, fmt.Errorf("decompress Proton server list: %w", err)
		}
		if data, err = io.ReadAll(reader); err != nil {
			return protonList{}, fmt.Errorf("decompress Proton server list: %w", err)
		}
	}
	var list protonList
	if err := json.Unmarshal(data, &list); err != nil {
		return protonList{}, fmt.Errorf("parse Proton server list: %w", err)
	}
	if list.Version != protonSchemaVersion {
		return protonList{}, fmt.Errorf("Proton server list has format version %d; this Solstein understands version %d", list.Version, protonSchemaVersion)
	}
	wireguard := 0
	for _, server := range list.Servers {
		if server.VPN == "wireguard" {
			wireguard++
		}
	}
	if wireguard < minimumProtonServers {
		return protonList{}, fmt.Errorf("Proton server list has only %d WireGuard servers; refusing it as broken", wireguard)
	}
	return list, nil
}

// embeddedProtonList is the snapshot shipped in the binary, parsed once.
var embeddedProtonList = sync.OnceValue(func() protonList {
	list, err := parseProtonList(protonSnapshot)
	if err != nil {
		// A test guarantees the snapshot parses.
		panic("embedded Proton server list: " + err.Error())
	}
	return list
})

var errFilteredOut = errors.New("filtered out")

// protonServers turns the list into servers a provider may use, after its
// tier and filter. Keys are left empty: the pool gives each tunnel one of
// the provider's keys when it opens.
func protonServers(list protonList, provider Provider) ([]Server, []string) {
	var warnings []string
	candidates := make([]protonServer, 0, len(list.Servers))
	nameCount := map[string]int{}
	for _, entry := range list.Servers {
		if entry.VPN != "wireguard" {
			continue
		}
		candidates = append(candidates, entry)
		nameCount[entry.ServerName]++
	}

	unmapped := map[string]bool{}
	var servers []Server
	for _, entry := range candidates {
		code, ok := countryByName(entry.Country)
		if !ok {
			unmapped[entry.Country] = true
			continue
		}
		if err := protonFilter(entry, code, provider); err != nil {
			continue
		}
		server, err := protonServerFrom(entry, code, nameCount[entry.ServerName] > 1)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("server %s skipped: %v", entry.ServerName, err))
			continue
		}
		servers = append(servers, server)
	}
	for _, name := range slices.Sorted(maps.Keys(unmapped)) {
		warnings = append(warnings, fmt.Sprintf("servers in %q skipped: country not recognised", name))
	}
	if len(servers) == 0 {
		warnings = append(warnings, "no Proton server matches the provider's tier and filter")
	}
	return servers, warnings
}

func protonFilter(entry protonServer, code string, provider Provider) error {
	filter := provider.Filter
	if provider.Tier == "free" && !entry.Free {
		return errFilteredOut
	}
	if !modeAllows(filter.SecureCore, entry.SecureCore) || !modeAllows(filter.Tor, entry.Tor) {
		return errFilteredOut
	}
	if len(filter.Countries) > 0 && !slices.Contains(filter.Countries, code) {
		return errFilteredOut
	}
	if len(filter.Cities) > 0 && !slices.ContainsFunc(filter.Cities, func(city string) bool { return strings.EqualFold(city, entry.City) }) {
		return errFilteredOut
	}
	if len(filter.Servers) > 0 && !slices.ContainsFunc(filter.Servers, func(name string) bool {
		return strings.EqualFold(name, entry.ServerName) || strings.EqualFold(name, entry.Hostname)
	}) {
		return errFilteredOut
	}
	return nil
}

// modeAllows applies an exclude/include/only filter to a server feature.
func modeAllows(mode string, has bool) bool {
	switch mode {
	case "only":
		return has
	case "include":
		return true
	default: // exclude
		return !has
	}
}

func protonServerFrom(entry protonServer, code string, nameShared bool) (Server, error) {
	if len(entry.IPs) == 0 {
		return Server{}, errors.New("no IP address")
	}
	address, err := netip.ParseAddr(entry.IPs[0])
	if err != nil {
		return Server{}, fmt.Errorf("IP address %q: %w", entry.IPs[0], err)
	}
	publicKey, err := ParsePublicKey(entry.PublicKey)
	if err != nil {
		return Server{}, fmt.Errorf("public key: %w", err)
	}
	// Proton reuses names across Secure Core routes and hostnames across
	// servers; the address is unique, so it tells shared names apart.
	name := entry.ServerName
	if nameShared {
		name += "@" + address.String()
	}
	return Server{
		Name:          name,
		ServerName:    entry.ServerName,
		Hostname:      entry.Hostname,
		Addresses:     []netip.Addr{protonAddress},
		DNS:           []netip.Addr{protonDNS},
		MTU:           defaultMTU,
		PeerPublicKey: publicKey,
		Endpoint:      netip.AddrPortFrom(address, protonPort).String(),
		AllowedIPs:    []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")},
		Location:      ServerLocation{Country: code, City: entry.City},
	}, nil
}
