package exits

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"aunefyren/solstein/logger"

	"github.com/sirupsen/logrus"
	logTest "github.com/sirupsen/logrus/hooks/test"
)

// newRefreshModule builds a module with one Proton provider whose list is
// fetched from host.
func newRefreshModule(t *testing.T, host *httptest.Server, now func() time.Time) *Module {
	t.Helper()
	provider := protonProvider("plus", Filter{Countries: []string{"SE"}})
	config := Config{Providers: map[string]Provider{"proton": provider}, Exits: map[string]Exit{}}
	servers, _ := protonServers(embeddedProtonList(), provider)
	module := New(config, map[string][]Server{"proton": servers}, now)
	module.configDir, module.protonList, module.protonListURL = t.TempDir(), embeddedProtonList(), host.URL
	module.SetFetchClient(host.Client())
	return module
}

// TestRunProtonRefresh checks the refresh loop: its first refresh comes
// when the cached copy turns a day old, and a failed refresh of a stale list
// is warned about.
func TestRunProtonRefresh(t *testing.T) {
	quietLogs(t)
	hook := logTest.NewLocal(logger.Log)

	requested := make(chan struct{}, 1)
	host := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case requested <- struct{}{}:
		default:
		}
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	defer host.Close()

	now := embeddedProtonList().UpdatedAt().Add(90 * 24 * time.Hour)
	module := newRefreshModule(t, host, func() time.Time { return now })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		module.runProtonRefresh(ctx, now.Add(-protonRefreshInterval+time.Millisecond))
		close(done)
	}()

	select {
	case <-requested:
	case <-time.After(10 * time.Second):
		t.Fatal("no refresh a day after the cached copy was written")
	}
	deadline := time.Now().Add(10 * time.Second)
	for !slices.ContainsFunc(hook.AllEntries(), func(entry *logrus.Entry) bool { return strings.Contains(entry.Message, "days old") }) {
		if time.Now().After(deadline) {
			t.Fatal("no warning about the stale list")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestRunProtonRefreshStopsBeforeFirstRefresh(t *testing.T) {
	quietLogs(t)
	host := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("refreshed after being stopped")
	}))
	defer host.Close()
	module := newRefreshModule(t, host, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	module.runProtonRefresh(ctx, time.Time{})
}

func TestRefreshProtonEdgeCases(t *testing.T) {
	quietLogs(t)
	var body []byte
	host := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Write(body)
	}))
	defer host.Close()
	ctx := context.Background()

	module := newRefreshModule(t, host, nil)
	module.SetFetchClient(nil)
	if err := module.refreshProton(ctx); err == nil {
		t.Error("refresh without a client: no error")
	}

	// Up to date with a cached copy: the copy is marked as checked now.
	module = newRefreshModule(t, host, nil)
	cache := filepath.Join(module.configDir, protonCacheFile)
	if err := writeCache(cache, rawSnapshot(t)); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(cache, old, old)
	body = rawSnapshot(t)
	if err := module.refreshProton(ctx); err != nil {
		t.Fatalf("same list: %v", err)
	}
	if info, err := os.Stat(cache); err != nil || info.ModTime().Before(time.Now().Add(-time.Hour)) {
		t.Errorf("cached copy not touched: %v", err)
	}

	// A newer list that can't be saved is used anyway.
	module = newRefreshModule(t, host, nil)
	blocker := filepath.Join(module.configDir, "vpn")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	body = newerList(t, 24*time.Hour)
	if err := module.refreshProton(ctx); err != nil {
		t.Fatalf("unsaveable newer list: %v", err)
	}
	if module.protonList.Timestamp <= embeddedProtonList().Timestamp {
		t.Error("newer list not used when it couldn't be saved")
	}
}

func TestWriteCacheErrors(t *testing.T) {
	directory := t.TempDir()
	blocker := filepath.Join(directory, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeCache(filepath.Join(blocker, "list.json"), []byte("{}")); err == nil {
		t.Error("writeCache under a file: no error")
	}
	// The target is a non-empty directory, so the rename fails.
	target := filepath.Join(directory, "target")
	os.MkdirAll(filepath.Join(target, "inside"), 0o750)
	if err := writeCache(target, []byte("{}")); err == nil {
		t.Error("writeCache over a directory: no error")
	}
	if err := touchCache(filepath.Join(blocker, "list.json")); err == nil {
		t.Error("touchCache under a file: no error")
	}
}

func TestLoadProtonListUnreadable(t *testing.T) {
	configDir := t.TempDir()
	// A directory where the cached copy belongs can be found but not read.
	os.MkdirAll(filepath.Join(configDir, protonCacheFile), 0o750)
	if list, _, source := loadProtonList(configDir); list.Timestamp != embeddedProtonList().Timestamp || !strings.Contains(source, "unreadable") {
		t.Errorf("unreadable cache: %s", source)
	}
}

func TestServerNames(t *testing.T) {
	module := New(Config{}, map[string][]Server{"local": {{Name: "b"}, {Name: "a"}}}, nil)
	if names := module.ServerNames("local"); !slices.Equal(names, []string{"a", "b"}) {
		t.Errorf("ServerNames = %v", names)
	}
	if names := module.ServerNames("none"); len(names) != 0 {
		t.Errorf("ServerNames of an unknown provider = %v", names)
	}
}
