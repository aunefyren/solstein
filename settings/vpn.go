package settings

// VPN is the exits module's block in config.json. It is plain data: the
// module (modules/exits) validates it and turns it into tunnels, so the core
// never depends on the module. Empty providers leave the module off.
type VPN struct {
	Providers map[string]VPNProvider `json:"providers"`
	Exits     map[string]VPNExit     `json:"exits"`
}

// VPNProvider is a pool of WireGuard servers and the credentials to reach
// them. Which fields apply depends on Type; see docs/exits.md.
type VPNProvider struct {
	// Type is "wireguard" (servers from wg-quick .conf files) or "protonvpn"
	// (servers from the published Proton server list).
	Type string `json:"type"`

	// protonvpn: WireGuard private keys (literal, env:NAME or file:PATH);
	// each concurrent tunnel uses its own key.
	PrivateKeys []string `json:"private_keys,omitempty"`
	// protonvpn: "plus" (default) or "free", which limits the pool to free
	// servers.
	Tier   string           `json:"tier,omitempty"`
	Filter *VPNServerFilter `json:"filter,omitempty"`

	// wireguard: exactly one of these.
	ConfigFile  string   `json:"config_file,omitempty"`
	ConfigFiles []string `json:"config_files,omitempty"`
	ConfigDir   string   `json:"config_dir,omitempty"`
	// wireguard: where a single config_file's server is.
	Country string `json:"country,omitempty"`
	City    string `json:"city,omitempty"`
	// wireguard: where each server of config_files / config_dir is, by
	// .conf file name without the extension.
	Servers map[string]VPNServerLocation `json:"servers,omitempty"`

	// MaxTunnels caps concurrent tunnels, to respect plan limits on
	// simultaneous connections. Zero means one per private key for
	// protonvpn, and no limit for wireguard.
	MaxTunnels int `json:"max_tunnels,omitempty"`

	// FallbackDNS are resolvers asked, over TCP through the same tunnel,
	// when the tunnel's own DNS server fails on a name. Unset means the
	// default (1.1.1.1 and 9.9.9.9); an empty list turns it off.
	FallbackDNS *[]string `json:"fallback_dns,omitempty"`
}

// VPNServerLocation is where a WireGuard server is.
type VPNServerLocation struct {
	Country string `json:"country"`
	City    string `json:"city,omitempty"`
}

// VPNServerFilter narrows a server-list provider's pool.
type VPNServerFilter struct {
	Countries []string `json:"countries,omitempty"`
	Cities    []string `json:"cities,omitempty"`
	// Servers are server names or hostnames, e.g. "SE#12".
	Servers []string `json:"servers,omitempty"`
	// SecureCore and Tor are "exclude" (default), "include" or "only".
	SecureCore string `json:"secure_core,omitempty"`
	Tor        string `json:"tor,omitempty"`
}

// VPNExit is a named route that feeds and region diff use.
type VPNExit struct {
	Provider string `json:"provider"`
	// Locations is an ordered preference list: "SE", "SE/Stockholm",
	// "server:SE#12", "area:northern-europe" or "continent:europe".
	// Empty means anywhere the provider allows.
	Locations []string `json:"locations,omitempty"`
	// Strict uses only the first location; otherwise the list is worked
	// down until one has a healthy server.
	Strict bool `json:"strict,omitempty"`
	// Exclude lists country codes never to use, whatever Locations says.
	Exclude []string `json:"exclude,omitempty"`
	// Selection is "sticky" (default), "random" or "least-failed".
	Selection string `json:"selection,omitempty"`
}
