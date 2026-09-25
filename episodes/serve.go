package episodes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"strconv"
	"sync"
	"time"

	"aunefyren/solstein/database"
	"aunefyren/solstein/feeds"
	"aunefyren/solstein/logger"
	"aunefyren/solstein/models"
	"aunefyren/solstein/outbound"

	"github.com/google/uuid"
)

// ErrSourceFailed means the episode couldn't be fetched from its source.
// Nothing has been written to the client when Serve returns it.
var ErrSourceFailed = errors.New("fetching the episode from its source failed")

// forwardedHeaders are the response headers passed on from the source when
// streaming. Anything else (cookies, caching, tracking headers) stays behind.
var forwardedHeaders = []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified"}

// Server serves episode audio to clients.
type Server struct {
	store   *database.Store
	exits   *outbound.Manager
	cache   Cache
	feeds   *feeds.Service
	options Options

	mutex sync.Mutex
	// teeing holds episodes currently being streamed into the cache, so a
	// second listener streams without starting a second copy.
	teeing map[uuid.UUID]bool
}

// NewServer builds a Server. It shares Options with the pipeline for the
// clock and idle timeout.
func NewServer(store *database.Store, exits *outbound.Manager, cache Cache, feedService *feeds.Service, options Options) *Server {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.IdleTimeout <= 0 {
		options.IdleTimeout = defaultIdleTimeout
	}
	return &Server{store: store, exits: exits, cache: cache, feeds: feedService, options: options, teeing: map[uuid.UUID]bool{}}
}

// Serve answers a request for an episode's audio:
//   - from the cache when it holds the episode, with full Range support;
//   - in original mode, by redirecting to the source;
//   - otherwise by streaming from the source through the feed's exit. In
//     cache mode, a full request for an episode the pipeline won't download
//     (backlog, or given up on) is also written into the cache on the way.
//
// An episode withheld by its processor's failure policy is not served.
//
// It returns database.ErrEpisodeNotFound / ErrFeedNotFound for unknown IDs
// and ErrSourceFailed when the source can't be reached; in both cases the
// response is still unwritten.
func (server *Server) Serve(writer http.ResponseWriter, request *http.Request, feedID, episodeID uuid.UUID) error {
	ctx := request.Context()
	episode, err := server.store.GetEpisode(ctx, feedID, episodeID)
	if err != nil {
		return err
	}
	if episode.Withheld {
		// Its failure policy keeps the unprocessed version from clients.
		return database.ErrEpisodeNotFound
	}
	feed, err := server.store.GetFeed(ctx, feedID)
	if err != nil {
		return err
	}

	if episode.CacheFile != "" {
		served, err := server.serveCached(writer, request, episode)
		if served || err != nil {
			return err
		}
		// The file is gone (deleted by hand, or the disk was swapped): forget
		// it and fall back to the source.
		logger.Log.Warn("Cached file for episode '" + episode.Title + "' is missing; streaming from the source instead.")
		episode.ForgetCache()
		if err := server.store.UpdateEpisode(ctx, &episode); err != nil {
			return err
		}
	}

	mode := server.feeds.DeliveryMode(feed)
	if mode == "original" {
		http.Redirect(writer, request, episode.SourceURL, http.StatusFound)
		return nil
	}

	// For a processed feed, only a failed episode (published unprocessed) is
	// cached this way: anything else would put the unprocessed version where
	// the processed one belongs.
	teeable := episode.State == models.EpisodeFailed ||
		(!server.feeds.Processed(feed) && (episode.Backlog || episode.State == models.EpisodeReady))
	tee := mode == "cache" && request.Method == http.MethodGet && request.Header.Get("Range") == "" &&
		teeable && server.startTee(episode.ID)
	if tee {
		defer server.endTee(episode.ID)
	}
	return server.stream(writer, request, feed, episode, tee)
}

// serveCached serves the episode from the cache. It reports false, with no
// error, when the cached file doesn't exist.
func (server *Server) serveCached(writer http.ResponseWriter, request *http.Request, episode models.Episode) (bool, error) {
	fullPath, err := server.cache.Path(episode.CacheFile)
	if err != nil {
		return false, err
	}
	file, err := os.Open(fullPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open cached episode: %w", err)
	}
	defer file.Close()

	modified := time.Time{}
	if episode.CachedAt != nil {
		modified = *episode.CachedAt
	}
	writer.Header().Set("Content-Type", contentTypeFor(fullPath))
	// ServeContent handles Range, If-Range, If-Modified-Since and HEAD, and
	// sets Content-Length and Accept-Ranges.
	http.ServeContent(writer, request, "", modified, file)
	return true, nil
}

func contentTypeFor(filePath string) string {
	switch extension := path.Ext(filePath); extension {
	case ".mp3":
		return "audio/mpeg"
	case ".m4a", ".m4b":
		return "audio/mp4"
	default:
		if contentType := mime.TypeByExtension(extension); contentType != "" {
			return contentType
		}
		return "application/octet-stream"
	}
}

// RemoveFeed deletes a deleted feed's cached audio at once, instead of
// leaving it for the hourly clean-up.
func (server *Server) RemoveFeed(feedID uuid.UUID) error {
	return server.cache.RemoveFeed(feedID)
}

func (server *Server) startTee(episodeID uuid.UUID) bool {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	if server.teeing[episodeID] {
		return false
	}
	server.teeing[episodeID] = true
	return true
}

func (server *Server) endTee(episodeID uuid.UUID) {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	delete(server.teeing, episodeID)
}

// stream passes the episode through from its source. Range requests are
// forwarded as they are. When tee is set, the audio is also written to the
// cache, and the download runs on past a listener disconnecting so the cache
// copy is completed anyway.
func (server *Server) stream(writer http.ResponseWriter, request *http.Request, feed models.Feed, episode models.Episode, tee bool) error {
	client, err := server.exits.Client(feed.Exit)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSourceFailed, err)
	}

	// Without a tee, the source request ends with the client's. With one, it
	// gets its own deadline, so the cache copy survives the listener leaving.
	parent := request.Context()
	if tee {
		parent = context.WithoutCancel(parent)
	}
	ctx, cancel := context.WithTimeout(parent, downloadTimeout)
	defer cancel()

	method := http.MethodGet
	if request.Method == http.MethodHead {
		method = http.MethodHead
	}
	upstream, err := http.NewRequestWithContext(ctx, method, episode.SourceURL, nil)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSourceFailed, err)
	}
	upstream.Header.Set("Accept", "*/*")
	if value := request.Header.Get("Range"); value != "" {
		upstream.Header.Set("Range", value)
	}

	response, err := client.Do(upstream)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSourceFailed, err)
	}
	defer response.Body.Close()
	if err := checkResponse(response); err != nil && response.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		return fmt.Errorf("%w: %w", ErrSourceFailed, err)
	}

	for _, name := range forwardedHeaders {
		if value := response.Header.Get(name); value != "" {
			writer.Header().Set(name, value)
		}
	}
	writer.WriteHeader(response.StatusCode)
	if method == http.MethodHead {
		return nil
	}

	body := newIdleReader(response.Body, server.options.IdleTimeout, cancel)
	defer body.stop()
	if !tee || response.StatusCode != http.StatusOK {
		// The source request shares the listener's context, so a disconnect
		// ends the copy; only log other failures.
		if _, err := io.Copy(writer, body); err != nil && request.Context().Err() == nil {
			logger.Log.Warn("Streaming episode '" + episode.Title + "' ended early. Error: " + err.Error())
		}
		return nil
	}
	listener := &clientWriter{writer: writer}

	extension := feeds.AudioExtension(episode.SourceURL, response.Header.Get("Content-Type"))
	file, commit, discard, err := server.cache.create(feed.ID, episode.ID, extension)
	if err != nil {
		// Can't cache; the listener still gets the audio.
		logger.Log.Warn("Failed to start caching episode '" + episode.Title + "' while streaming. Error: " + err.Error())
		io.Copy(listener, body)
		return nil
	}
	// The file is written first: a listener write error must not stop the
	// cache copy, so clientWriter swallows it.
	size, err := io.Copy(io.MultiWriter(file, listener), io.LimitReader(body, maxEpisodeBytes+1))
	switch {
	case err == nil && (size == 0 || size > maxEpisodeBytes):
		err = fmt.Errorf("unusable size %d", size)
	case err == nil && response.ContentLength > 0 && size != response.ContentLength:
		err = fmt.Errorf("ended after %d of %d bytes", size, response.ContentLength)
	}
	if err != nil {
		discard()
		logger.Log.Warn("Episode '" + episode.Title + "' was not cached while streaming. Error: " + err.Error())
		return nil
	}
	cacheFile, err := commit()
	if err != nil {
		logger.Log.Warn("Failed to store streamed episode '" + episode.Title + "' in the cache. Error: " + err.Error())
		return nil
	}

	now := server.options.Now().UTC()
	episode.CacheFile, episode.CacheSize, episode.CacheSeconds, episode.CachedAt = cacheFile, size, 0, &now
	if episode.State == models.EpisodeFailed {
		episode.State, episode.LastError = models.EpisodeReady, ""
	}
	// The listener may be gone, so don't use its context.
	if err := server.store.UpdateEpisode(context.WithoutCancel(request.Context()), &episode); err != nil {
		logger.Log.Error("Failed to record cached episode '" + episode.Title + "'. Error: " + err.Error())
		return nil
	}
	logger.Log.Info("Cached episode '" + episode.Title + "' of '" + feed.Title + "' while streaming it (" + strconv.FormatInt(size>>20, 10) + " MB).")
	return nil
}

// clientWriter writes to the listener until the first failure, then drops
// the rest silently, so a disconnect doesn't abort a cache copy.
type clientWriter struct {
	writer http.ResponseWriter
	gone   bool
}

func (client *clientWriter) Write(data []byte) (int, error) {
	if client.gone {
		return len(data), nil
	}
	if _, err := client.writer.Write(data); err != nil {
		client.gone = true
	}
	return len(data), nil
}
