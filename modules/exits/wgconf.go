package exits

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Server is one WireGuard server: everything needed to bring a tunnel up.
type Server struct {
	// Name identifies the server within its provider: the .conf file name
	// without extension, or the server list's name (e.g. "SE#12").
	Name       string
	PrivateKey Key
	// Addresses are the tunnel's own addresses (the [Interface] Address
	// lines, without prefix lengths).
	Addresses []netip.Addr
	// DNS servers, reached through the tunnel.
	DNS []netip.Addr
	MTU int

	PeerPublicKey PublicKey
	PresharedKey  *Key
	// Endpoint is host:port; a host name is resolved when the tunnel opens.
	Endpoint   string
	AllowedIPs []netip.Prefix
	Keepalive  int // seconds; zero is off

	Location ServerLocation
}

const defaultMTU = 1420

// ignoredKeys are wg-quick settings that act on the host's network. There is
// no host network here: each tunnel is its own network stack inside the
// process, so they don't apply.
var ignoredKeys = map[string]bool{
	"preup": true, "postup": true, "predown": true, "postdown": true,
	"table": true, "saveconfig": true, "listenport": true, "fwmark": true,
}

// ParseConf reads a wg-quick .conf file. It accepts exactly one [Peer], as
// client configs have. Warnings list settings that were ignored.
func ParseConf(name string, reader io.Reader) (server Server, warnings []string, err error) {
	server = Server{Name: name, MTU: defaultMTU}
	var (
		section    string
		peers      int
		sawKey     bool
		sawPeerKey bool
	)
	scanner := bufio.NewScanner(reader)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if comment := strings.IndexByte(line, '#'); comment >= 0 {
			line = strings.TrimSpace(line[:comment])
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			switch section {
			case "interface":
			case "peer":
				peers++
			default:
				return Server{}, nil, fmt.Errorf("line %d: unknown section [%s]", lineNumber, section)
			}
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			return Server{}, nil, fmt.Errorf("line %d: expected key = value", lineNumber)
		}
		key, value = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(value)
		if ignoredKeys[key] {
			warnings = append(warnings, fmt.Sprintf("line %d: %s ignored; tunnels don't touch the host's network", lineNumber, key))
			continue
		}

		// Errors below never include value for key lines: they are secrets.
		switch section + "." + key {
		case "interface.privatekey":
			if server.PrivateKey, err = ParseKey(value); err != nil {
				return Server{}, nil, fmt.Errorf("line %d: PrivateKey: %w", lineNumber, err)
			}
			sawKey = true
		case "interface.address":
			for _, item := range splitList(value) {
				prefix, err := netip.ParsePrefix(item)
				if err != nil {
					address, addressErr := netip.ParseAddr(item)
					if addressErr != nil {
						return Server{}, nil, fmt.Errorf("line %d: Address %q is not an IP address", lineNumber, item)
					}
					prefix = netip.PrefixFrom(address, address.BitLen())
				}
				server.Addresses = append(server.Addresses, prefix.Addr())
			}
		case "interface.dns":
			for _, item := range splitList(value) {
				address, err := netip.ParseAddr(item)
				if err != nil {
					// wg-quick also takes search domains here.
					warnings = append(warnings, fmt.Sprintf("line %d: DNS search domain %q ignored", lineNumber, item))
					continue
				}
				server.DNS = append(server.DNS, address)
			}
		case "interface.mtu":
			mtu, err := strconv.Atoi(value)
			if err != nil || mtu < 576 || mtu > 9000 {
				return Server{}, nil, fmt.Errorf("line %d: MTU %q is not between 576 and 9000", lineNumber, value)
			}
			server.MTU = mtu
		case "peer.publickey":
			if server.PeerPublicKey, err = ParsePublicKey(value); err != nil {
				return Server{}, nil, fmt.Errorf("line %d: PublicKey: %w", lineNumber, err)
			}
			sawPeerKey = true
		case "peer.presharedkey":
			presharedKey, err := ParseKey(value)
			if err != nil {
				return Server{}, nil, fmt.Errorf("line %d: PresharedKey: %w", lineNumber, err)
			}
			server.PresharedKey = &presharedKey
		case "peer.endpoint":
			if _, _, err := splitEndpoint(value); err != nil {
				return Server{}, nil, fmt.Errorf("line %d: Endpoint: %w", lineNumber, err)
			}
			server.Endpoint = value
		case "peer.allowedips":
			for _, item := range splitList(value) {
				prefix, err := netip.ParsePrefix(item)
				if err != nil {
					return Server{}, nil, fmt.Errorf("line %d: AllowedIPs %q is not a CIDR", lineNumber, item)
				}
				server.AllowedIPs = append(server.AllowedIPs, prefix)
			}
		case "peer.persistentkeepalive":
			if strings.EqualFold(value, "off") {
				continue
			}
			seconds, err := strconv.Atoi(value)
			if err != nil || seconds < 0 || seconds > 65535 {
				return Server{}, nil, fmt.Errorf("line %d: PersistentKeepalive %q is not a number of seconds", lineNumber, value)
			}
			server.Keepalive = seconds
		default:
			if section == "" {
				return Server{}, nil, fmt.Errorf("line %d: %s outside a section", lineNumber, key)
			}
			warnings = append(warnings, fmt.Sprintf("line %d: unknown setting %s ignored", lineNumber, key))
		}
	}
	if err := scanner.Err(); err != nil {
		return Server{}, nil, err
	}

	switch {
	case !sawKey:
		return Server{}, nil, errors.New("[Interface] has no PrivateKey")
	case len(server.Addresses) == 0:
		return Server{}, nil, errors.New("[Interface] has no Address")
	case peers != 1:
		return Server{}, nil, fmt.Errorf("expected exactly one [Peer], found %d", peers)
	case !sawPeerKey:
		return Server{}, nil, errors.New("[Peer] has no PublicKey")
	case server.Endpoint == "":
		return Server{}, nil, errors.New("[Peer] has no Endpoint")
	}
	if len(server.AllowedIPs) == 0 {
		warnings = append(warnings, "no AllowedIPs; assuming all traffic (0.0.0.0/0, ::/0)")
		server.AllowedIPs = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}
	}
	if len(server.DNS) == 0 {
		// Lookups would otherwise go out through the host's resolver and
		// leak, or pick a CDN edge for the wrong region.
		warnings = append(warnings, "no DNS; names can't be resolved through this tunnel")
	}
	return server, warnings, nil
}

// LoadConfs reads a wireguard provider's .conf files: the listed ones, or
// every *.conf in the directory. Locations come from the provider's
// declarations. A file that can't be read or parsed is reported and skipped.
func LoadConfs(provider Provider) (servers []Server, problems []string) {
	files := provider.ConfigFiles
	if provider.ConfigDir != "" {
		matches, err := filepath.Glob(filepath.Join(provider.ConfigDir, "*.conf"))
		if err != nil {
			return nil, []string{fmt.Sprintf("list %s: %v", provider.ConfigDir, err)}
		}
		sort.Strings(matches)
		files = matches
		if len(files) == 0 {
			problems = append(problems, "no .conf files in "+provider.ConfigDir)
		}
	}

	for _, path := range files {
		name := serverName(path)
		file, err := os.Open(path)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		server, warnings, err := ParseConf(name, file)
		file.Close()
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		for _, warning := range warnings {
			problems = append(problems, name+": "+warning)
		}
		server.Location = provider.Locations[name]
		servers = append(servers, server)
	}
	return servers, problems
}

func splitList(value string) []string {
	var items []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}

// splitEndpoint checks host:port, including [IPv6]:port.
func splitEndpoint(endpoint string) (string, int, error) {
	host, portText, err := net.SplitHostPort(endpoint)
	if err != nil {
		return "", 0, fmt.Errorf("%q is not host:port", endpoint)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("%q has no valid port", endpoint)
	}
	return host, port, nil
}
