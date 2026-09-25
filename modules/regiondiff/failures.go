package regiondiff

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"aunefyren/solstein/episodes"
	"aunefyren/solstein/logger"
)

// failureRetention is how long kept downloads of failed attempts stay.
const failureRetention = 14 * 24 * time.Hour

// unsafeName matches what may not go into a file name made from an exit's.
var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// keepFailed saves the downloads of an attempt whose diff failed, with a
// note of what failed, in FailureDir/{feedID}/{episodeID}/, replacing an
// earlier failure of the same episode. Sets older than failureRetention are
// removed at the same time. It does nothing when FailureDir is empty, and a
// failure to save is only logged: it mustn't turn into the episode's error.
func (processor *Processor) keepFailed(job episodes.Job, reason error, downloads map[string]checkedDownload) {
	root := processor.options.FailureDir
	if root == "" {
		return
	}
	pruneFailures(root, time.Now())

	directory := filepath.Join(root, job.Feed.ID.String(), job.Episode.ID.String())
	if err := os.RemoveAll(directory); err != nil {
		logger.Log.Warn("Region diff: failed to replace the kept downloads in " + directory + ". Error: " + err.Error())
		return
	}
	if err := os.MkdirAll(directory, 0o750); err != nil {
		logger.Log.Warn("Region diff: failed to keep the downloads of a failed attempt. Error: " + err.Error())
		return
	}

	exits := make([]string, 0, len(downloads))
	for exit := range downloads {
		exits = append(exits, exit)
	}
	sort.Strings(exits)
	var note strings.Builder
	fmt.Fprintf(&note, "Episode: %s\nFeed: %s\nSource: %s\nAttempt: %s\nStated duration: %s\nError: %s\n\nDownloads:\n",
		job.Episode.Title, job.Feed.Title, withoutQuery(job.Episode.SourceURL), time.Now().Format(time.RFC3339), job.ExpectedDuration, reason)
	for _, exit := range exits {
		download := downloads[exit]
		name := unsafeName.ReplaceAllString(exit, "_") + ".mp3"
		if err := os.WriteFile(filepath.Join(directory, name), download.Data, 0o640); err != nil {
			logger.Log.Warn("Region diff: failed to keep a download of a failed attempt. Error: " + err.Error())
			return
		}
		fmt.Fprintf(&note, "- %s: %s, %s (%s)\n", exit, name, download.describe(), download.ContentType)
	}
	if err := os.WriteFile(filepath.Join(directory, "failure.txt"), []byte(note.String()), 0o640); err != nil {
		logger.Log.Warn("Region diff: failed to write the note of a failed attempt. Error: " + err.Error())
		return
	}
	logger.Log.Info(fmt.Sprintf("Region diff: kept the downloads of the failed attempt for '%s' in %s.", job.Episode.Title, directory))
}

// pruneFailures removes kept sets older than failureRetention, and feed
// directories left empty.
func pruneFailures(root string, now time.Time) {
	feeds, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, feed := range feeds {
		if !feed.IsDir() {
			continue
		}
		feedDirectory := filepath.Join(root, feed.Name())
		sets, err := os.ReadDir(feedDirectory)
		if err != nil {
			continue
		}
		left := 0
		for _, set := range sets {
			info, err := set.Info()
			if err == nil && now.Sub(info.ModTime()) > failureRetention {
				os.RemoveAll(filepath.Join(feedDirectory, set.Name()))
				continue
			}
			left++
		}
		if left == 0 {
			os.Remove(feedDirectory)
		}
	}
}

// withoutQuery drops a URL's query string, which private feeds use for
// access tokens.
func withoutQuery(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "(unparseable URL)"
	}
	parsed.RawQuery = ""
	return parsed.String()
}
