package settings

import (
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func envFrom(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func writeConfig(t *testing.T, configDir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(configDir, configFileName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolvePrecedence(t *testing.T) {
	cases := []struct {
		name     string
		file     string
		env      map[string]string
		args     []string
		wantPort int
	}{
		{name: "default", wantPort: defaultPort},
		{name: "file", file: `{"port": 9000}`, wantPort: 9000},
		{name: "env over file", file: `{"port": 9000}`, env: map[string]string{"SOLSTEIN_PORT": "9100"}, wantPort: 9100},
		{name: "flag over env", file: `{"port": 9000}`, env: map[string]string{"SOLSTEIN_PORT": "9100"}, args: []string{"-port", "9200"}, wantPort: 9200},
		{name: "empty env ignored", file: `{"port": 9000}`, env: map[string]string{"SOLSTEIN_PORT": ""}, wantPort: 9000},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			configDir := t.TempDir()
			if c.file != "" {
				writeConfig(t, configDir, c.file)
			}
			args := append([]string{"-configdir", configDir}, c.args...)

			cfg, _, err := Resolve(args, envFrom(c.env), io.Discard)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if cfg.Port != c.wantPort {
				t.Errorf("port = %d, want %d", cfg.Port, c.wantPort)
			}
		})
	}
}

func TestResolvePersistsOverrides(t *testing.T) {
	configDir := t.TempDir()
	writeConfig(t, configDir, `{"port": 9000, "log_level": "info"}`)
	env := envFrom(map[string]string{"SOLSTEIN_LOG_LEVEL": "debug"})

	if _, _, err := Resolve([]string{"-configdir", configDir, "-externalurl", "http://solstein:8080/"}, env, io.Discard); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	saved, err := Load(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Port != 9000 || saved.LogLevel != "debug" || saved.ExternalURL != "http://solstein:8080" {
		t.Errorf("config.json = %+v (want overrides saved, normalised, untouched fields kept)", saved)
	}

	// With the flag and env var gone, the saved values remain, and so do the
	// generated secrets.
	cfg, _, err := Resolve([]string{"-configdir", configDir}, envFrom(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, saved) {
		t.Errorf("after removing overrides got %+v, want %+v", cfg, saved)
	}
}

func TestResolveCreatesConfigFile(t *testing.T) {
	configDir := t.TempDir()

	if _, _, err := Resolve([]string{"-configdir", configDir}, envFrom(nil), io.Discard); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	cfg, err := Load(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != defaultPort || cfg.LogLevel != defaultLogLevel {
		t.Errorf("config.json = %+v, want defaults", cfg)
	}
}

func TestResolveInvalidOverrideLeavesFileAlone(t *testing.T) {
	configDir := t.TempDir()
	original := `{"port": 9000}`
	writeConfig(t, configDir, original)

	if _, _, err := Resolve([]string{"-configdir", configDir, "-port", "70000"}, envFrom(nil), io.Discard); err == nil {
		t.Fatal("expected a validation error")
	}

	data, err := os.ReadFile(filepath.Join(configDir, configFileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != original {
		t.Errorf("config.json changed after a failed override: %s", data)
	}
}

func TestResolveConfigDirFromEnv(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "nested")

	_, startup, err := Resolve(nil, envFrom(map[string]string{configDirEnv: configDir}), io.Discard)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if startup.ConfigDir != configDir {
		t.Errorf("data dir = %q, want %q", startup.ConfigDir, configDir)
	}
	if _, err := os.Stat(filepath.Join(configDir, configFileName)); err != nil {
		t.Errorf("config.json not created in data dir from env: %v", err)
	}
}

func TestResolveVersionSkipsConfig(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "unused")

	_, startup, err := Resolve([]string{"-configdir", configDir, "-version"}, envFrom(nil), io.Discard)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !startup.ShowVersion {
		t.Error("ShowVersion not set")
	}
	if _, err := os.Stat(configDir); !errors.Is(err, os.ErrNotExist) {
		t.Error("config directory was created for -version")
	}
}

func TestResolveErrors(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		args    []string
		wantErr string
	}{
		{name: "bad port flag", args: []string{"-port", "eighty"}, wantErr: "flag -port"},
		{name: "bad port env", env: map[string]string{"SOLSTEIN_PORT": "eighty"}, wantErr: "SOLSTEIN_PORT"},
		{name: "invalid value fails validation", args: []string{"-loglevel", "loud"}, wantErr: "invalid log level"},
		{name: "unknown flag", args: []string{"-nope"}, wantErr: "not defined"},
		{name: "stray argument", args: []string{"serve"}, wantErr: "unexpected argument"},
		{name: "bad bool env", env: map[string]string{"SOLSTEIN_ALLOW_PRIVATE_DESTINATIONS": "maybe"}, wantErr: "SOLSTEIN_ALLOW_PRIVATE_DESTINATIONS"},
		{name: "bad poll interval", args: []string{"-pollinterval", "often"}, wantErr: "flag -pollinterval"},
		{name: "bad network list", env: map[string]string{"SOLSTEIN_ALLOWED_CLIENT_NETWORKS": "10.0.0.0/8,lan"}, wantErr: "allowed client networks"},
		{name: "bad delivery mode", args: []string{"-deliverymode", "fax"}, wantErr: "delivery mode"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := append([]string{"-configdir", t.TempDir()}, c.args...)
			_, _, err := Resolve(args, envFrom(c.env), io.Discard)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("err = %v, want it to contain %q", err, c.wantErr)
			}
		})
	}
}

func TestResolveHelp(t *testing.T) {
	var output strings.Builder
	_, _, err := Resolve([]string{"-h"}, envFrom(nil), &output)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("err = %v, want flag.ErrHelp", err)
	}
	for _, s := range settings {
		if !strings.Contains(output.String(), s.env) {
			t.Errorf("help text does not mention %s", s.env)
		}
	}
}

func TestResolveAllowPrivateDestinations(t *testing.T) {
	configDir := t.TempDir()

	cfg, _, err := Resolve([]string{"-configdir", configDir}, envFrom(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AllowPrivateDestinations {
		t.Error("private destinations allowed by default")
	}

	cfg, _, err = Resolve([]string{"-configdir", configDir}, envFrom(map[string]string{"SOLSTEIN_ALLOW_PRIVATE_DESTINATIONS": "true"}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AllowPrivateDestinations {
		t.Error("env var not applied")
	}

	cfg, _, err = Resolve([]string{"-configdir", configDir, "-allowprivatedestinations=false"}, envFrom(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AllowPrivateDestinations {
		t.Error("-allowprivatedestinations=false not applied")
	}

	// A bare boolean flag means true.
	cfg, _, err = Resolve([]string{"-configdir", configDir, "-allowprivatedestinations"}, envFrom(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AllowPrivateDestinations {
		t.Error("bare -allowprivatedestinations not applied")
	}
}

func TestResolveListsAndNumbers(t *testing.T) {
	env := envFrom(map[string]string{
		"SOLSTEIN_ALLOWED_CLIENT_NETWORKS": " 172.18.0.0/16 , 192.168.1.10,",
		"SOLSTEIN_ALLOWED_SOURCE_HOSTS":    "feeds.acast.com",
		"SOLSTEIN_POLL_INTERVAL":           "30",
	})
	cfg, _, err := Resolve([]string{"-configdir", t.TempDir(), "-trustedproxies", "10.0.0.1", "-deliverymode", "stream", "-disableauth"}, env, io.Discard)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !reflect.DeepEqual(cfg.AllowedClientNetworks, []string{"172.18.0.0/16", "192.168.1.10/32"}) {
		t.Errorf("client networks = %v", cfg.AllowedClientNetworks)
	}
	if !reflect.DeepEqual(cfg.TrustedProxies, []string{"10.0.0.1/32"}) || !reflect.DeepEqual(cfg.AllowedSourceHosts, []string{"feeds.acast.com"}) {
		t.Errorf("proxies = %v, hosts = %v", cfg.TrustedProxies, cfg.AllowedSourceHosts)
	}
	if cfg.PollIntervalMinutes != 30 || cfg.DeliveryMode != "stream" || !cfg.DisableAuth {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestResolveDirectExit(t *testing.T) {
	cases := []struct {
		args []string
		env  map[string]string
		want string
	}{
		{nil, nil, "auto"},
		{[]string{"-directexit", "off"}, nil, "off"},
		{nil, map[string]string{"SOLSTEIN_DIRECT_EXIT": "on"}, "on"},
		// The old switch still works: true is off, false is on.
		{[]string{"-disabledirect"}, nil, "off"},
		{nil, map[string]string{"SOLSTEIN_DISABLE_DIRECT": "false"}, "on"},
	}
	for _, c := range cases {
		cfg, _, err := Resolve(append([]string{"-configdir", t.TempDir()}, c.args...), envFrom(c.env), io.Discard)
		if err != nil || cfg.DirectExit != c.want {
			t.Errorf("%v %v: direct_exit %q, %v; want %q", c.args, c.env, cfg.DirectExit, err, c.want)
		}
	}
	if _, _, err := Resolve([]string{"-configdir", t.TempDir(), "-directexit", "maybe"}, envFrom(nil), io.Discard); err == nil {
		t.Error("a bad direct_exit was accepted")
	}
}

func TestResolveStringSettings(t *testing.T) {
	args := []string{"-configdir", t.TempDir(),
		"-authtoken", "token-from-a-flag-0123456789",
		"-defaultexit", "direct",
		"-homecountry", "NO",
		"-timezone", "Europe/Oslo",
		"-directexit", "on",
		"-deliverymode", "stream",
	}
	cfg, _, err := Resolve(args, envFrom(nil), io.Discard)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got := []string{cfg.AuthToken, cfg.DefaultExit, cfg.HomeCountry, cfg.Timezone, cfg.DirectExit, cfg.DeliveryMode}
	want := []string{"token-from-a-flag-0123456789", "direct", "NO", "Europe/Oslo", "on", "stream"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("settings = %v, want %v", got, want)
	}
}

func TestResolveConfigDirErrors(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Resolve([]string{"-configdir", filepath.Join(blocker, "config")}, envFrom(nil), io.Discard); err == nil || !strings.Contains(err.Error(), "create config directory") {
		t.Errorf("config directory under a file: err = %v", err)
	}

	configDir := t.TempDir()
	writeConfig(t, configDir, "{broken")
	if _, _, err := Resolve([]string{"-configdir", configDir}, envFrom(nil), io.Discard); err == nil {
		t.Error("broken config.json: no error")
	}

	if _, _, err := Resolve([]string{"-configdir", t.TempDir()}, envFrom(map[string]string{"SOLSTEIN_DISABLE_DIRECT": "maybe"}), io.Discard); err == nil || !strings.Contains(err.Error(), "SOLSTEIN_DISABLE_DIRECT") {
		t.Errorf("bad SOLSTEIN_DISABLE_DIRECT: err = %v", err)
	}
}

func TestWriteFileAtomicErrors(t *testing.T) {
	directory := t.TempDir()
	if err := writeFileAtomic(filepath.Join(directory, "missing", "config.json"), []byte("{}")); err == nil {
		t.Error("write into a missing directory: no error")
	}
	// The target is a non-empty directory, so the rename fails.
	target := filepath.Join(directory, "config.json")
	if err := os.MkdirAll(filepath.Join(target, "inside"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(target, []byte("{}")); err == nil {
		t.Error("replace a directory: no error")
	}
}
