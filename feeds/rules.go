package feeds

import (
	"context"
	"fmt"
	"regexp"
	"slices"

	"aunefyren/solstein/logger"
	"aunefyren/solstein/models"

	"github.com/google/uuid"
)

// Rule actions.
const (
	RuleHide = "hide"
	RuleTag  = "tag"
)

// EpisodeTypes are the values <itunes:episodeType> takes, and so what the
// tag action can set.
var EpisodeTypes = []string{"full", "bonus", "trailer"}

// maxRules caps a feed's rules; a feed needing more than a handful is
// better split some other way.
const maxRules = 50

// Rule is one of a feed's rules as the API takes and returns it (see
// models.FeedRule): what it is stored as, without the row's ID and times,
// which mean nothing when every change replaces the whole list.
type Rule struct {
	TitleMatches string `json:"title_matches"`
	MinSeconds   int    `json:"min_seconds"`
	MaxSeconds   int    `json:"max_seconds"`
	Action       string `json:"action"`
	EpisodeType  string `json:"episode_type,omitempty"`
}

func ruleOf(stored models.FeedRule) Rule {
	return Rule{
		TitleMatches: stored.TitleMatches, MinSeconds: stored.MinSeconds, MaxSeconds: stored.MaxSeconds,
		Action: stored.Action, EpisodeType: stored.EpisodeType,
	}
}

func (rule Rule) stored() models.FeedRule {
	return models.FeedRule{
		TitleMatches: rule.TitleMatches, MinSeconds: rule.MinSeconds, MaxSeconds: rule.MaxSeconds,
		Action: rule.Action, EpisodeType: rule.EpisodeType,
	}
}

// rules are a feed's rules, ready to match episodes against.
type rules []compiledRule

type compiledRule struct {
	models.FeedRule
	title *regexp.Regexp
}

// compileRules checks rules and prepares them for matching. An invalid rule
// returns ErrInvalidSettings, saying which rule and why.
func compileRules(list []models.FeedRule) (rules, error) {
	if len(list) > maxRules {
		return nil, fmt.Errorf("%w: at most %d rules per feed", ErrInvalidSettings, maxRules)
	}
	compiled := make(rules, 0, len(list))
	for i, rule := range list {
		invalid := func(format string, args ...any) error {
			return fmt.Errorf("%w: rule %d: %s", ErrInvalidSettings, i+1, fmt.Sprintf(format, args...))
		}
		switch rule.Action {
		case RuleHide:
			if rule.EpisodeType != "" {
				return nil, invalid("episode_type only goes with action %q", RuleTag)
			}
		case RuleTag:
			if !slices.Contains(EpisodeTypes, rule.EpisodeType) {
				return nil, invalid("episode_type must be one of %v", EpisodeTypes)
			}
		default:
			return nil, invalid("action must be %q or %q", RuleHide, RuleTag)
		}
		if rule.MinSeconds < 0 || rule.MaxSeconds < 0 {
			return nil, invalid("min_seconds and max_seconds must not be negative")
		}
		if rule.MaxSeconds > 0 && rule.MinSeconds > rule.MaxSeconds {
			return nil, invalid("min_seconds is more than max_seconds")
		}
		entry := compiledRule{FeedRule: rule}
		if rule.TitleMatches != "" {
			title, err := regexp.Compile("(?i)" + rule.TitleMatches)
			if err != nil {
				return nil, invalid("title_matches: %v", err)
			}
			entry.title = title
		}
		compiled = append(compiled, entry)
	}
	return compiled, nil
}

// match returns the first rule the episode matches, or nil.
func (list rules) match(episode models.Episode) *models.FeedRule {
	for i := range list {
		rule := &list[i]
		if rule.title != nil && !rule.title.MatchString(episode.Title) {
			continue
		}
		if (rule.MinSeconds > 0 || rule.MaxSeconds > 0) && episode.SourceSeconds <= 0 {
			continue // no stated duration to compare
		}
		if rule.MinSeconds > 0 && episode.SourceSeconds < rule.MinSeconds {
			continue
		}
		if rule.MaxSeconds > 0 && episode.SourceSeconds > rule.MaxSeconds {
			continue
		}
		return &rule.FeedRule
	}
	return nil
}

// hides reports whether the rules leave the episode out of the feed.
func (list rules) hides(episode models.Episode) bool {
	rule := list.match(episode)
	return rule != nil && rule.Action == RuleHide
}

// episodeType is what the rules tag the episode as, or empty.
func (list rules) episodeType(episode models.Episode) string {
	if rule := list.match(episode); rule != nil && rule.Action == RuleTag {
		return rule.EpisodeType
	}
	return ""
}

// ValidateRules checks a feed's rules without saving them.
func ValidateRules(list []Rule) error {
	_, err := compileRules(storedRules(list))
	return err
}

func storedRules(list []Rule) []models.FeedRule {
	stored := make([]models.FeedRule, len(list))
	for i, rule := range list {
		stored[i] = rule.stored()
	}
	return stored
}

// Rules returns a feed's rules in the order they are tried.
func (service *Service) Rules(ctx context.Context, feedID uuid.UUID) ([]Rule, error) {
	if _, err := service.store.GetFeed(ctx, feedID); err != nil {
		return nil, err
	}
	stored, err := service.store.ListFeedRules(ctx, feedID)
	if err != nil {
		return nil, err
	}
	list := make([]Rule, len(stored))
	for i, rule := range stored {
		list[i] = ruleOf(rule)
	}
	return list, nil
}

// SetRules replaces a feed's rules and hides or shows its episodes to
// match, the ones already stored included. It returns how many episodes are
// no longer hidden, which may need preparing now. Invalid rules return
// ErrInvalidSettings, and nothing is saved.
func (service *Service) SetRules(ctx context.Context, feedID uuid.UUID, list []Rule) (shown int, err error) {
	stored := storedRules(list)
	compiled, err := compileRules(stored)
	if err != nil {
		return 0, err
	}
	return service.store.ReplaceFeedRules(ctx, feedID, stored, compiled.hides)
}

// loadRules returns a feed's rules ready for matching. Rules are checked
// before they are saved, so one that no longer compiles (a change in what is
// accepted) is logged and the feed treated as having none, rather than
// failing every poll and render.
func (service *Service) loadRules(ctx context.Context, feed models.Feed) (rules, error) {
	list, err := service.store.ListFeedRules(ctx, feed.ID)
	if err != nil {
		return nil, err
	}
	compiled, err := compileRules(list)
	if err != nil {
		logger.Log.Warn("Ignoring the rules of feed '" + feed.Title + "', which aren't valid any more. Error: " + err.Error())
		return nil, nil
	}
	return compiled, nil
}
