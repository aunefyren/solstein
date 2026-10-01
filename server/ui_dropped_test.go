package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"aunefyren/solstein/database"
	"aunefyren/solstein/models"

	"github.com/google/uuid"
)

// TestUIDroppedEpisodes: an episode the source no longer lists says so on
// the feed page, with its serve setting and a Delete button.
func TestUIDroppedEpisodes(t *testing.T) {
	router, store := newUITestRouter(t, nil)
	id := createFeed(t, router, startPodcastHost(t).URL+"/feed")
	feedID := uuid.MustParse(id)
	ctx := context.Background()
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}

	list, _ := store.ListEpisodes(ctx, feedID)
	episode := list[0]
	episodePath := "/ui/feeds/" + id + "/episodes/" + episode.ID.String()

	// Still in the source: it can't be deleted.
	if recorder := do(router, http.MethodPost, episodePath+"/delete", "", form); recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "The source still lists that episode") {
		t.Errorf("delete an episode in the source: %d", recorder.Code)
	}

	// The next poll doesn't list it.
	listed := []models.Episode{{GUID: "new", SourceURL: "https://cdn.example.com/new.mp3", Title: "New", State: models.EpisodeReady}}
	if _, err := store.SyncEpisodes(ctx, feedID, listed, database.Hiding{Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetEpisodeServe(ctx, feedID, episode.ID, "off", "Its source answered 404 Not Found on 1 Oct 2026 14:05: the audio is gone, so it is no longer served."); err != nil {
		t.Fatal(err)
	}
	page := do(router, http.MethodGet, "/ui/feeds/"+id, "", nil).Body.String()
	for _, want := range []string{
		"no longer in the source since", "Not in the served feed.", "1 episode no longer in the source.",
		`<span class="badge badge--warn">Check</span> Its source answered 404 Not Found`,
		`name="serve"`, `<option value="off" selected>Off</option>`, "Default (off)",
		`action="/ui/feeds/` + id + `/episodes/` + episode.ID.String() + `/delete"`,
		`name="serve_dropped"`, `name="delete_dropped"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the feed page lacks %q", want)
		}
	}
	if strings.Contains(page, "or with Prepare") {
		t.Error("a dropped episode is offered Prepare")
	}
	if problems := do(router, http.MethodGet, "/ui/feeds/"+id+"?show=problems", "", nil).Body.String(); !strings.Contains(problems, "Its source answered 404") {
		t.Error("a dropped episode with a warning isn't among the problems")
	}

	// Serving it by hand clears the warning.
	recorder := do(router, http.MethodPost, episodePath+"/serve", url.Values{"serve": {"on"}}.Encode(), form)
	location := recorder.Header().Get("Location")
	if recorder.Code != http.StatusSeeOther || !strings.HasPrefix(location, "/ui/feeds/"+id+"?done=serve&episode="+episode.ID.String()) {
		t.Fatalf("serve: %d to %q", recorder.Code, location)
	}
	stored, _ := store.GetEpisode(ctx, feedID, episode.ID)
	if stored.Serve != "on" || stored.ServeWarning != "" {
		t.Errorf("after serving by hand: serve %q, warning %q", stored.Serve, stored.ServeWarning)
	}
	page, _, _ = strings.Cut(location, "#")
	if body := do(router, http.MethodGet, page, "", nil).Body.String(); !strings.Contains(body, "Saved whether &#39;One&#39; is served.") || !strings.Contains(body, "Still in the served feed, and kept for good.") || !strings.Contains(body, "Fetched when a client asks for it.") {
		t.Errorf("after serving by hand:\n%s", body)
	}
	if code := do(router, http.MethodPost, episodePath+"/serve", url.Values{"serve": {"sometimes"}}.Encode(), form).Code; code != http.StatusBadRequest {
		t.Errorf("serve with a bad value = %d, want 400", code)
	}

	// Deleting it.
	recorder = do(router, http.MethodPost, episodePath+"/delete", "", form)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/ui/feeds/"+id+"?done=deleted" {
		t.Fatalf("delete: %d to %q", recorder.Code, recorder.Header().Get("Location"))
	}
	if _, err := store.GetEpisode(ctx, feedID, episode.ID); err == nil {
		t.Error("the episode is still there")
	}
	if code := do(router, http.MethodPost, episodePath+"/delete", "", form).Code; code != http.StatusNotFound {
		t.Errorf("delete it again = %d, want 404", code)
	}

	// The feed's own settings.
	recorder = do(router, http.MethodPost, "/ui/feeds/"+id, url.Values{"serve_dropped": {"on"}, "delete_dropped": {"off"}, "from": {"feed"}}.Encode(), form)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("save: %d", recorder.Code)
	}
	feed, _ := store.GetFeed(ctx, feedID)
	if feed.ServeDropped != "on" || feed.DeleteDropped != "off" {
		t.Errorf("feed settings = %q, %q", feed.ServeDropped, feed.DeleteDropped)
	}
}
