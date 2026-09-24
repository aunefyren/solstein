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
	return published
}
