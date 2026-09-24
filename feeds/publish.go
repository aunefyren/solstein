package feeds

import (
	"aunefyren/solstein/models"

	"github.com/google/uuid"
)

// publishedEpisodes decides which episodes appear in the served feed.
// episodes must be in publish order (oldest first), as ListEpisodes returns
// them.
//
// In cache mode an episode appears once it is ready, and episodes are
// published strictly in order: one still pending holds back every newer one.
// ABS only picks up episodes newer than the newest it already has, so
// publishing Tuesday's episode before Monday's would make it skip Monday's for
// good (see docs/design.md, Client compatibility).
//
// Backlog episodes (there when the feed was added) are always published;
// they are fetched on demand. Failed episodes are published too, and served
// by streaming from the source instead of from the cache, so one bad
// download can't hold back a feed forever.
//
// In stream and original mode nothing needs preparing, so everything is
// published at once.
//
// A feed is never left without episodes: ABS counts a feed with no items as
// a failed check and turns auto-download off after 24 of them. If nothing
// else would be published, the oldest episode is, and is streamed from the
// source until it is cached.
func publishedEpisodes(episodes []models.Episode, mode string) map[uuid.UUID]bool {
	published := make(map[uuid.UUID]bool, len(episodes))
	holding := false
	for _, episode := range episodes {
		switch {
		case mode != "cache", episode.Backlog:
			published[episode.ID] = true
		case holding:
		case episode.State == models.EpisodeReady, episode.State == models.EpisodeFailed:
			published[episode.ID] = true
		default:
			holding = true
		}
	}
	if len(published) == 0 && len(episodes) > 0 {
		published[episodes[0].ID] = true
	}
	return published
}
