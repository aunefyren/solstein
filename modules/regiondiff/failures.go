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

// keptRetention is how long kept downloads stay.
const keptRetention = 14 * 24 * time.Hour

// unsafeName matches what may not go into a file name made from an exit's.
var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// keepFailed saves the downloads of an attempt whose diff failed, with a
// note of what failed, in FailureDir/{feedID}/{episodeID}/, replacing an
// earlier failure of the same episode. It does nothing when FailureDir is
// empty.
func (processor *Processor) keepFailed(job episodes.Job, reason error, downloads map[string]checkedDownload) {
	if processor.options.FailureDir == "" {
		return
	}
	keepDownloads(processor.options.FailureDir, "failure.txt", "the failed attempt", job, "Error: "+reason.Error()+"\n", downloads)
}

// keepSuccessful saves the downloads of a diff that worked, with its note
// and where it cut (result is nil when the downloads were identical), in
// SuccessDir/{feedID}/{episodeID}/, replacing an earlier set of the same
// episode. It does nothing when SuccessDir is empty.
func (processor *Processor) keepSuccessful(job episodes.Job, note string, result *Result, downloads map[string]checkedDownload) {
	if processor.options.SuccessDir == "" {
		return
	}
	var body strings.Builder
	fmt.Fprintf(&body, "Result: %s\n", note)
	if result != nil {
		fmt.Fprintf(&body, "Output duration: %s\n\nRemoved from the home download (times in the home download):\n", result.Duration.Round(time.Millisecond))
		for _, cut := range cuts(*result) {
			fmt.Fprintf(&body, "- at %s, %s%s\n", formatOffset(cut.at), cut.segment.Duration.Round(time.Millisecond), cut.label)
		}
	}
	keepDownloads(processor.options.SuccessDir, "result.txt", "the diff", job, body.String(), downloads)
}

// cut is a removed segment and where it starts in the home download.
type cut struct {
	segment Segment
	at      time.Duration
	label   string
}

// cuts places the removed segments in time: Kept and Removed together
// cover the home download's frames, so a segment starts where the ones
// before it end.
func cuts(result Result) []cut {
	segments := append(append([]Segment{}, result.Kept...), result.Removed...)
	sort.Slice(segments, func(i, j int) bool { return segments[i].Start < segments[j].Start })
	removed := map[int]bool{}
	for _, segment := range result.Removed {
		removed[segment.Start] = true
	}
	markers := map[int]bool{}
	for _, marker := range result.Markers {
		markers[marker.Start] = true
	}
	var list []cut
	var at time.Duration
	for _, segment := range segments {
		if removed[segment.Start] {
			label := ""
			if markers[segment.Start] {
				label = " (break marker)"
			}
			list = append(list, cut{segment: segment, at: at, label: label})
		}
		at += segment.Duration
	}
	return list
}

// formatOffset writes a position as h:mm:ss.mmm.
func formatOffset(offset time.Duration) string {
	offset = offset.Round(time.Millisecond)
	return fmt.Sprintf("%d:%02d:%02d.%03d", int(offset.Hours()), int(offset.Minutes())%60, int(offset.Seconds())%60, int(offset.Milliseconds())%1000)
}

// keepDownloads saves a set of downloads with a note in
// root/{feedID}/{episodeID}/, replacing an earlier set of the same episode,
// and removes sets older than keptRetention. It does nothing when root is
// empty, and a failure to save is only logged: it mustn't turn into the
// episode's error. what names the attempt in the log.
func keepDownloads(root, noteName, what string, job episodes.Job, details string, downloads map[string]checkedDownload) {
	if root == "" {
		return
	}
	pruneKept(root, time.Now())

	directory := filepath.Join(root, job.Feed.ID.String(), job.Episode.ID.String())
	if err := os.RemoveAll(directory); err != nil {
		logger.Log.Warn("Region diff: failed to replace the kept downloads in " + directory + ". Error: " + err.Error())
		return
	}
	if err := os.MkdirAll(directory, 0o750); err != nil {
		logger.Log.Warn("Region diff: failed to keep the downloads of " + what + ". Error: " + err.Error())
		return
	}

	exits := make([]string, 0, len(downloads))
	for exit := range downloads {
		exits = append(exits, exit)
	}
	sort.Strings(exits)
	var note strings.Builder
	fmt.Fprintf(&note, "Episode: %s\nFeed: %s\nSource: %s\nAttempt: %s\nStated duration: %s\n%s\nDownloads:\n",
		job.Episode.Title, job.Feed.Title, withoutQuery(job.Episode.SourceURL), time.Now().Format(time.RFC3339), job.ExpectedDuration, details)
	for _, exit := range exits {
		download := downloads[exit]
		name := unsafeName.ReplaceAllString(exit, "_") + ".mp3"
		if err := os.WriteFile(filepath.Join(directory, name), download.Data, 0o640); err != nil {
			logger.Log.Warn("Region diff: failed to keep a download of " + what + ". Error: " + err.Error())
			return
		}
		fmt.Fprintf(&note, "- %s: %s, %s (%s)\n", exit, name, download.describe(), download.ContentType)
	}
	if err := os.WriteFile(filepath.Join(directory, noteName), []byte(note.String()), 0o640); err != nil {
		logger.Log.Warn("Region diff: failed to write the note of " + what + ". Error: " + err.Error())
		return
	}
	logger.Log.Info(fmt.Sprintf("Region diff: kept the downloads of %s for '%s' in %s.", what, job.Episode.Title, directory))
}

// pruneKept removes kept sets older than keptRetention, and feed
// directories left empty.
func pruneKept(root string, now time.Time) {
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
			if err == nil && now.Sub(info.ModTime()) > keptRetention {
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
