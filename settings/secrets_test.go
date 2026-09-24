package settings

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveSecret(t *testing.T) {
	directory := t.TempDir()
	keyFile := filepath.Join(directory, "key")
	os.WriteFile(keyFile, []byte("  from-file\n"), 0o600)
	emptyFile := filepath.Join(directory, "empty")
	os.WriteFile(emptyFile, []byte("\n"), 0o600)
	env := envFrom(map[string]string{"PROTON_KEY_1": " from-env ", "EMPTY": ""})

	cases := []struct{ reference, want string }{
		{"env:PROTON_KEY_1", "from-env"},
		{"file:" + keyFile, "from-file"},
		{"literal-value", "literal-value"},
	}
	for _, c := range cases {
		if got, err := ResolveSecret(c.reference, env); err != nil || got != c.want {
			t.Errorf("ResolveSecret(%q) = %q, %v; want %q", c.reference, got, err, c.want)
		}
	}

	for _, reference := range []string{"env:MISSING", "env:EMPTY", "env:", "file:" + filepath.Join(directory, "missing"), "file:" + emptyFile, "file:", "", "   "} {
		_, err := ResolveSecret(reference, env)
		if !errors.Is(err, ErrSecretUnavailable) {
			t.Errorf("ResolveSecret(%q): err = %v, want ErrSecretUnavailable", reference, err)
		}
	}

	// Errors name where the secret was looked for, never what it is.
	os.WriteFile(keyFile, []byte(""), 0o600)
	_, err := ResolveSecret("env:EMPTY", env)
	if err == nil || !strings.Contains(err.Error(), "EMPTY") {
		t.Errorf("error doesn't name the variable: %v", err)
	}
}

func TestDefaultsIncludeEmptyVPNBlock(t *testing.T) {
	cfg := validConfig()
	if cfg.VPN.Providers == nil || cfg.VPN.Exits == nil {
		t.Errorf("VPN block not defaulted: %+v", cfg.VPN)
	}
	data, err := marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"vpn": {`) || !strings.Contains(string(data), `"providers": {}`) {
		t.Errorf("config.json doesn't show the vpn block:\n%s", data)
	}
}
