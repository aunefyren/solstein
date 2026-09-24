package exits

import (
	"encoding/base64"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func sampleConf(t *testing.T) (string, Key, Key) {
	t.Helper()
	private, peer := generateKey(t), generateKey(t)
	preshared := generateKey(t)
	conf := `# Exported from a VPN provider
[Interface]
PrivateKey = ` + encodeKey(private) + `
Address = 10.2.0.2/32, fd00::2/128
DNS = 10.2.0.1, search.example
MTU = 1380
PostUp = iptables -A FORWARD -j ACCEPT
Table = off

[Peer]
PublicKey = ` + encodeKey(peer) + `
PresharedKey = ` + encodeKey(preshared) + `
AllowedIPs = 0.0.0.0/0, ::/0
Endpoint = se-sto-wg-001.example.net:51820
PersistentKeepalive = 25
`
	return conf, private, peer
}

// encodeKey writes a key the way a .conf file holds it. Tests only: real
// keys are never turned back into text.
func encodeKey(key Key) string { return base64.StdEncoding.EncodeToString(key.bytes[:]) }

func TestParseConf(t *testing.T) {
	conf, private, peer := sampleConf(t)
	server, warnings, err := ParseConf("se-sto-wg-001", strings.NewReader(conf))
	if err != nil {
		t.Fatalf("ParseConf: %v", err)
	}
	if server.Name != "se-sto-wg-001" || server.PrivateKey != private || server.PeerPublicKey.bytes != peer.bytes {
		t.Errorf("keys or name wrong for %s", server.Name)
	}
	if !reflect.DeepEqual(server.Addresses, []netip.Addr{netip.MustParseAddr("10.2.0.2"), netip.MustParseAddr("fd00::2")}) {
		t.Errorf("addresses = %v", server.Addresses)
	}
	if !reflect.DeepEqual(server.DNS, []netip.Addr{netip.MustParseAddr("10.2.0.1")}) || server.MTU != 1380 || server.Keepalive != 25 {
		t.Errorf("DNS/MTU/keepalive = %v %d %d", server.DNS, server.MTU, server.Keepalive)
	}
	if server.Endpoint != "se-sto-wg-001.example.net:51820" || server.PresharedKey == nil || len(server.AllowedIPs) != 2 {
		t.Errorf("peer = %+v", server)
	}
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{"postup ignored", "table ignored", "search domain \"search.example\""} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings lack %q:\n%s", want, joined)
		}
	}
}

func TestParseConfDefaultsAndWarnings(t *testing.T) {
	key, peer := generateKey(t), generateKey(t)
	conf := "[Interface]\nPrivateKey=" + encodeKey(key) + "\nAddress=10.0.0.2\n[Peer]\nPublicKey=" + encodeKey(peer) + "\nEndpoint=198.51.100.1:51820\nPersistentKeepalive = off\n"
	server, warnings, err := ParseConf("minimal", strings.NewReader(conf))
	if err != nil {
		t.Fatal(err)
	}
	if server.MTU != defaultMTU || server.Keepalive != 0 || len(server.AllowedIPs) != 2 || server.Addresses[0] != netip.MustParseAddr("10.0.0.2") {
		t.Errorf("defaults = %+v", server)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "no AllowedIPs") || !strings.Contains(joined, "no DNS") {
		t.Errorf("warnings = %v", warnings)
	}
}

func TestParseConfErrors(t *testing.T) {
	key, peer := encodeKey(generateKey(t)), encodeKey(generateKey(t))
	valid := "[Interface]\nPrivateKey=" + key + "\nAddress=10.0.0.2/32\n[Peer]\nPublicKey=" + peer + "\nEndpoint=198.51.100.1:51820\n"
	cases := []struct {
		name, conf, wantErr string
	}{
		{"no private key", strings.Replace(valid, "PrivateKey="+key+"\n", "", 1), "no PrivateKey"},
		{"bad private key", strings.Replace(valid, key, "c2VjcmV0", 1), "PrivateKey"},
		{"no address", strings.Replace(valid, "Address=10.0.0.2/32\n", "", 1), "no Address"},
		{"bad address", strings.Replace(valid, "10.0.0.2/32", "ten.zero", 1), "Address"},
		{"no peer", valid[:strings.Index(valid, "[Peer]")], "exactly one [Peer]"},
		{"two peers", valid + "[Peer]\nPublicKey=" + peer + "\n", "exactly one [Peer]"},
		{"no peer key", strings.Replace(valid, "PublicKey="+peer+"\n", "", 1), "no PublicKey"},
		{"no endpoint", strings.Replace(valid, "Endpoint=198.51.100.1:51820\n", "", 1), "no Endpoint"},
		{"bad endpoint", strings.Replace(valid, "198.51.100.1:51820", "198.51.100.1", 1), "Endpoint"},
		{"bad port", strings.Replace(valid, ":51820", ":99999", 1), "port"},
		{"bad MTU", valid + "[Interface]\nMTU=12\n", "MTU"},
		{"unknown section", "[Wireguard]\n" + valid, "unknown section"},
		{"outside section", "PrivateKey=" + key + "\n" + valid, "outside a section"},
		{"no equals", "[Interface]\nPrivateKey\n", "key = value"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := ParseConf("x", strings.NewReader(c.conf))
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("err = %v, want %q", err, c.wantErr)
			}
			if err != nil && (strings.Contains(err.Error(), key) || strings.Contains(err.Error(), "c2VjcmV0")) {
				t.Errorf("key material in error: %v", err)
			}
		})
	}
}

func TestLoadConfs(t *testing.T) {
	directory := t.TempDir()
	conf, _, _ := sampleConf(t)
	os.WriteFile(filepath.Join(directory, "se-sto-wg-001.conf"), []byte(conf), 0o600)
	os.WriteFile(filepath.Join(directory, "broken.conf"), []byte("[Interface]\n"), 0o600)
	os.WriteFile(filepath.Join(directory, "notes.txt"), []byte("not a conf"), 0o600)

	servers, problems := LoadConfs(Provider{Name: "mullvad", ConfigDir: directory, Locations: map[string]ServerLocation{"se-sto-wg-001": {Country: "SE", City: "Stockholm"}}})
	if len(servers) != 1 || servers[0].Name != "se-sto-wg-001" || servers[0].Location != (ServerLocation{Country: "SE", City: "Stockholm"}) {
		t.Errorf("servers = %+v", servers)
	}
	joined := strings.Join(problems, "\n")
	if !strings.Contains(joined, "broken: [Interface] has no PrivateKey") || !strings.Contains(joined, "se-sto-wg-001: line") {
		t.Errorf("problems:\n%s", joined)
	}

	servers, problems = LoadConfs(Provider{Name: "one", ConfigFiles: []string{filepath.Join(directory, "missing.conf")}})
	if len(servers) != 0 || len(problems) != 1 || !strings.Contains(problems[0], "missing") {
		t.Errorf("missing file: %+v, %v", servers, problems)
	}
	if _, problems := LoadConfs(Provider{Name: "empty", ConfigDir: t.TempDir()}); len(problems) != 1 || !strings.Contains(problems[0], "no .conf files") {
		t.Errorf("empty directory: %v", problems)
	}
}
