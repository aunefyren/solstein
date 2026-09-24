package exits

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"aunefyren/solstein/logger"
)

const (
	protonCacheFile = "vpn/protonvpn.json"
	// protonRefreshInterval is how often the list is fetched. gluetun's
	// maintainers update it about monthly; daily keeps the lag short.
	protonRefreshInterval = 24 * time.Hour
	// protonFirstRefresh waits a little after start-up, so fetching the list
	// never slows starting Solstein.
	protonFirstRefresh = time.Minute
	maxProtonListBytes = 20 << 20
)

// loadProtonList picks the list to start with: the cached copy from an
// earlier refresh if it is valid and newer than the embedded snapshot,
// otherwise the snapshot. It also reports when the cache was written, zero
// if there is none.
func loadProtonList(configDir string) (protonList, time.Time, string) {
	embedded := embeddedProtonList()
	path := filepath.Join(configDir, protonCacheFile)
	info, err := os.Stat(path)
	if err != nil {
		return embedded, time.Time{}, "built-in list"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return embedded, time.Time{}, "built-in list (cached copy unreadable: " + err.Error() + ")"
	}
	cached, err := parseProtonList(data)
	if err != nil {
		return embedded, time.Time{}, "built-in list (cached copy unusable: " + err.Error() + ")"
	}
	if cached.Timestamp <= embedded.Timestamp {
		return embedded, info.ModTime(), "built-in list (newer than the cached copy)"
	}
	return cached, info.ModTime(), "cached list from " + path
}

// SetFetchClient gives the module the HTTP client to refresh server lists
// with. main.go passes the core's "direct" client, so the refresh gets the
// same safeguards as every other outgoing request.
func (module *Module) SetFetchClient(client *http.Client) {
	module.mutex.Lock()
	defer module.mutex.Unlock()
	module.fetchClient = client
}

// runProtonRefresh refreshes the Proton list daily until ctx is cancelled.
func (module *Module) runProtonRefresh(ctx context.Context, cachedAt time.Time) {
	wait := protonFirstRefresh
	if since := module.now().Sub(cachedAt); !cachedAt.IsZero() && since < protonRefreshInterval {
		wait = protonRefreshInterval - since
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if err := module.refreshProton(ctx); err != nil && ctx.Err() == nil {
			logger.Log.Warn("Failed to refresh the Proton server list; keeping the current one. Error: " + err.Error())
		}
		timer.Reset(protonRefreshInterval)
	}
}

var errNotNewer = errors.New("not newer than the current list")

// refreshProton fetches the list, and if it is valid and newer than the one
// in use, saves it and rebuilds every Proton provider's servers. Tunnels
// already open keep running; they close when idle as usual.
func (module *Module) refreshProton(ctx context.Context) error {
	module.mutex.Lock()
	client, current := module.fetchClient, module.protonList
	module.mutex.Unlock()
	if client == nil {
		return errors.New("no HTTP client set")
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, module.protonListURL, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("server list host answered %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxProtonListBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxProtonListBytes {
		return errors.New("server list is implausibly large")
	}
	list, err := parseProtonList(data)
	if err != nil {
		return err
	}
	if list.Timestamp <= current.Timestamp {
		logger.Log.Debug("Proton server list is up to date (" + current.UpdatedAt().UTC().Format(time.DateOnly) + ").")
		return touchCache(filepath.Join(module.configDir, protonCacheFile))
	}

	if err := writeCache(filepath.Join(module.configDir, protonCacheFile), data); err != nil {
		logger.Log.Warn("Failed to save the Proton server list; using it anyway until the next restart. Error: " + err.Error())
	}
	module.useProtonList(list)
	logger.Log.Info("Updated the Proton server list to the version of " + list.UpdatedAt().UTC().Format(time.DateOnly) + ".")
	return nil
}

// useProtonList rebuilds the Proton providers' servers from a list.
func (module *Module) useProtonList(list protonList) {
	module.mutex.Lock()
	defer module.mutex.Unlock()
	module.protonList = list
	for name, provider := range module.config.Providers {
		if provider.Type != TypeProtonVPN {
			continue
		}
		servers, warnings := protonServers(list, provider)
		for _, warning := range warnings {
			logger.Log.Warn("VPN: provider '" + name + "': " + warning)
		}
		module.servers[name] = servers
	}
}

// writeCache saves the list atomically, so a crash can't leave half a file
// that the next start would then reject.
func writeCache(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".protonvpn.*.json")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}

// touchCache marks the cached list as checked now, so a restart doesn't
// fetch again straight away.
func touchCache(path string) error {
	now := time.Now()
	if err := os.Chtimes(path, now, now); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return nil
}
