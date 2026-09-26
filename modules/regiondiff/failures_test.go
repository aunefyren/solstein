package regiondiff

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"aunefyren/solstein/episodes"
	"aunefyren/solstein/models"

	"github.com/google/uuid"
)

func TestCutsLabelsBreakMarkers(t *testing.T) {
	second := time.Second
	result := Result{
		Kept:    []Segment{{Start: 0, End: 10, Duration: 10 * second}, {Start: 12, End: 20, Duration: 8 * second}},
		Removed: []Segment{{Start: 10, End: 12, Duration: 2 * second}, {Start: 20, End: 30, Duration: 10 * second}},
		Markers: []Segment{{Start: 10, End: 12, Duration: 2 * second}},
	}
	list := cuts(result)
	if len(list) != 2 {
		t.Fatalf("cuts = %+v", list)
	}
	if list[0].at != 10*second || list[0].label != " (break marker)" {
		t.Errorf("first cut = %+v", list[0])
	}
	if list[1].at != 20*second || list[1].label != "" {
		t.Errorf("second cut = %+v", list[1])
	}
}

func TestKeepDownloadsFailuresAreOnlyLogged(t *testing.T) {
	job := episodes.Job{
		Feed:    models.Feed{Base: models.Base{ID: uuid.New()}},
		Episode: models.Episode{Base: models.Base{ID: uuid.New()}},
	}
	downloads := map[string]checkedDownload{"norway": {}}

	// The root is a file: nothing can be kept under it.
	root := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(root, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	keepDownloads(root, "note.txt", "a test", job, "", downloads)

	// The note's name is a directory, so the note can't be written.
	root = t.TempDir()
	keepDownloads(root, ".", "a test", job, "", downloads)
	if _, err := os.Stat(filepath.Join(root, job.Feed.ID.String(), job.Episode.ID.String(), "norway.mp3")); err != nil {
		t.Errorf("download not kept: %v", err)
	}
}

func TestPruneKept(t *testing.T) {
	pruneKept(filepath.Join(t.TempDir(), "missing"), time.Now()) // no root: nothing to do

	root := t.TempDir()
	stray := filepath.Join(root, "stray.txt")
	os.WriteFile(stray, nil, 0o600)
	emptyFeed := filepath.Join(root, "empty-feed")
	os.MkdirAll(emptyFeed, 0o750)
	pruneKept(root, time.Now())
	if _, err := os.Stat(stray); err != nil {
		t.Errorf("a file in the root was removed: %v", err)
	}
	if _, err := os.Stat(emptyFeed); !os.IsNotExist(err) {
		t.Errorf("empty feed directory kept: %v", err)
	}
}

func TestWithoutQuery(t *testing.T) {
	if got := withoutQuery("https://host.example/e.mp3?token=secret"); got != "https://host.example/e.mp3" {
		t.Errorf("withoutQuery = %q", got)
	}
	if got := withoutQuery("http://[::1"); got != "(unparseable URL)" {
		t.Errorf("withoutQuery of a bad URL = %q", got)
	}
}
