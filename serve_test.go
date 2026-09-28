package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"aunefyren/solstein/logger"
)

// freePort is a port nothing listens on right now.
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// startSolstein runs serve in the background with these arguments until the
// returned stop is called, which waits for it and reports its exit code. It
// returns once the HTTP server answers.
func startSolstein(t *testing.T, args []string, env map[string]string) (base string, stop func() int) {
	t.Helper()
	// serve sets the logger for the process: put it back. (It sets the time
	// zone too, but only when one is configured, which these tests don't.)
	savedLogger := logger.Log
	t.Cleanup(func() { logger.Log = savedLogger })

	port := freePort(t)
	args = append(args, "-port", strconv.Itoa(port))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var output strings.Builder
	go func() {
		done <- serve(ctx, args, func(name string) string { return env[name] }, io.Discard, &output)
	}()

	base = "http://127.0.0.1:" + strconv.Itoa(port)
	deadline := time.Now().Add(15 * time.Second)
	for {
		response, err := http.Get(base + "/api/health")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case code := <-done:
			cancel()
			t.Fatalf("Solstein exited with %d before serving: %s", code, output.String())
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("Solstein didn't start serving")
		}
		time.Sleep(20 * time.Millisecond)
	}
	var once sync.Once
	exitCode := -1
	return base, func() int {
		once.Do(func() { exitCode = waitForExit(t, cancel, done, args) })
		return exitCode
	}
}

// waitForExit stops a started Solstein and returns its exit code.
func waitForExit(t *testing.T, cancel context.CancelFunc, done <-chan int, args []string) int {
	t.Helper()
	cancel()
	select {
	case code := <-done:
		return code
	case <-time.After(40 * time.Second):
		logged, _ := os.ReadFile(filepath.Join(configDirOf(args), "solstein.log"))
		t.Fatalf("Solstein didn't stop:\n%s", logged)
		return -1
	}
}

// configDirOf is the -configdir among serve's arguments.
func configDirOf(args []string) string {
	for i, arg := range args {
		if arg == "-configdir" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// localFeed serves a small feed with one episode, whose audio is served too.
func localFeed(t *testing.T) string {
	t.Helper()
	var host *httptest.Server
	host = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/ep-1.mp3" {
			writer.Header().Set("Content-Type", "audio/mpeg")
			fmt.Fprint(writer, "ID3fake-audio")
			return
		}
		fmt.Fprintf(writer, `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>Local Show</title>
<item><title>One</title><guid>ep-1</guid><pubDate>Mon, 21 Sep 2026 06:00:00 +0000</pubDate>
<enclosure url="%s/ep-1.mp3" type="audio/mpeg" length="13"/></item>
</channel></rss>`, host.URL)
	}))
	t.Cleanup(host.Close)
	return host.URL + "/feed"
}

// TestServeRunsAndStops starts Solstein the way the binary does, with the
// web UI and a user, subscribes to a feed through the API, and stops it;
// then starts it again preparing ahead, which queues that feed's backlog.
func TestServeRunsAndStops(t *testing.T) {
	dir := t.TempDir()
	common := []string{"-configdir", dir, "-allowprivatedestinations", "-loglevel", "debug"}

	// A user first, on the console, as an operator would.
	var console strings.Builder
	if code := serve(context.Background(), []string{"user", "add", "alice", "-configdir", dir}, func(string) string { return "" }, &console, io.Discard); code != 0 || !strings.Contains(console.String(), "One-time password") {
		t.Fatalf("user add = %d: %s", code, console.String())
	}

	env := map[string]string{"SOLSTEIN_WEB_UI": "true", "SOLSTEIN_PROCESSING_WAIT": "60", "SOLSTEIN_SKIP_TRACKING_REDIRECTS": "true"}
	base, stop := startSolstein(t, common, env)

	config, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		AuthToken string `json:"auth_token"`
		WebUI     struct {
			Enabled bool `json:"enabled"`
		} `json:"web_ui"`
	}
	if err := json.Unmarshal(config, &saved); err != nil || saved.AuthToken == "" || !saved.WebUI.Enabled {
		t.Fatalf("config.json: %+v, %v", saved, err)
	}
	request, _ := http.NewRequest(http.MethodPost, base+"/api/v1/feeds", strings.NewReader(`{"source_url": "`+localFeed(t)+`"}`))
	request.Header.Set("Authorization", "Bearer "+saved.AuthToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusCreated {
		t.Fatalf("subscribe: %v, %v", response, err)
	}
	response.Body.Close()

	// The web UI is there, and sends a stranger to sign in.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err = client.Get(base + "/ui/feeds")
	if err != nil || response.StatusCode != http.StatusSeeOther || !strings.HasPrefix(response.Header.Get("Location"), "/ui/login") {
		t.Fatalf("web UI: %v, %v", response, err)
	}
	response.Body.Close()
	if code := stop(); code != 0 {
		t.Fatalf("exit code %d", code)
	}

	// Again, preparing everything ahead: the feed's backlog is queued at
	// start-up, and cached before long.
	base, stop = startSolstein(t, append(common, "-prepareahead"), env)
	defer stop()
	logFile := filepath.Join(dir, "solstein.log")
	deadline := time.Now().Add(15 * time.Second)
	for {
		logged, _ := os.ReadFile(logFile)
		if strings.Contains(string(logged), "Queued 1 episodes of 'Local Show' to be prepared ahead") && strings.Contains(string(logged), "Cached episode 'One' of 'Local Show'") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the backlog wasn't queued and cached at start-up:\n%s", logged)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if code := stop(); code != 0 {
		t.Fatalf("second run's exit code %d", code)
	}
}

func TestServeCommandLine(t *testing.T) {
	var stdout, stderr strings.Builder
	noEnv := func(string) string { return "" }
	if code := serve(context.Background(), []string{"-version"}, noEnv, &stdout, &stderr); code != 0 || !strings.HasPrefix(stdout.String(), "solstein ") {
		t.Errorf("-version = %d %q", code, stdout.String())
	}
	if code := serve(context.Background(), []string{"-h"}, noEnv, io.Discard, &stderr); code != 0 || !strings.Contains(stderr.String(), "SOLSTEIN_") {
		t.Errorf("-h = %d", code)
	}
	stderr.Reset()
	if code := serve(context.Background(), []string{"-configdir", t.TempDir(), "-port", "0"}, noEnv, io.Discard, &stderr); code != 1 || !strings.Contains(stderr.String(), "Failed to load configuration") {
		t.Errorf("a bad port = %d %q", code, stderr.String())
	}
	stderr.Reset()
	if code := serve(context.Background(), []string{"-configdir", t.TempDir(), "-timezone", "Mars/Olympus"}, noEnv, io.Discard, &stderr); code != 1 || !strings.Contains(stderr.String(), "Failed to load configuration") {
		t.Errorf("a bad time zone = %d %q", code, stderr.String())
	}
	if code := serve(context.Background(), []string{"user"}, noEnv, io.Discard, io.Discard); code != 2 {
		t.Errorf("user without a command = %d, want the usage", code)
	}
}

func TestSentence(t *testing.T) {
	if got := sentence("comparing norway with germany"); got != "Comparing norway with germany." {
		t.Errorf("sentence = %q", got)
	}
	if sentence("") != "" {
		t.Error("an empty summary isn't left empty")
	}
}

// TestServeWithVPNAndRegionDiff starts Solstein with a Proton provider and
// region diff, as the one-key setup has them. Nothing connects: tunnels open
// on first use, and the server list's first refresh is a minute away.
func TestServeWithVPNAndRegionDiff(t *testing.T) {
	dir := t.TempDir()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	config := `{
  "vpn": {
    "providers": {"proton": {"type": "protonvpn", "private_keys": ["env:TEST_PROTON_KEY"]}},
    "exits": {
      "mine": {"provider": "proton", "locations": ["NO"], "strict": true},
      "abroad": {"provider": "proton", "locations": ["DE"], "strict": true}
    }
  },
  "default_exit": "mine",
  "region_diff": {"enabled": true, "exits": ["mine", "abroad"], "on_failure": "hide"}
}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"TEST_PROTON_KEY": base64.StdEncoding.EncodeToString(key)}
	_, stop := startSolstein(t, []string{"-configdir", dir}, env)
	if code := stop(); code != 0 {
		t.Fatalf("exit code %d", code)
	}

	logged, err := os.ReadFile(filepath.Join(dir, "solstein.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"VPN module on: exits abroad, mine over providers proton",
		"Exits available: abroad, mine; default: mine.",
		"The direct exit is off",
		"Region diff on: comparing mine (home) with abroad",
		// One key for two exits in use at once.
		"can hold 1 tunnel at once",
		"Region diff: for every feed, episodes that can't be cleaned are kept out of the feed",
	} {
		if !strings.Contains(string(logged), want) {
			t.Errorf("the start-up log lacks %q:\n%s", want, logged)
		}
	}
	if strings.Contains(string(logged), base64.StdEncoding.EncodeToString(key)) {
		t.Error("the log shows the private key")
	}
}
