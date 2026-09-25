package settings

import (
	"fmt"
	"slices"
	"strings"
)

// RegionDiffFailurePolicies are the valid values for region_diff.on_failure:
// publish an episode that can't be processed with its ads, or keep it out
// of the feed.
var RegionDiffFailurePolicies = []string{"publish", "hide"}

const (
	defaultMinSharedSeconds = 2
	defaultMaxRemovedShare  = 0.3
	defaultOnFailure        = "publish"
)

// RegionDiff is the region-diff module's block in config.json. Like VPN it
// is plain data; the module checks the exits against the ones that exist.
type RegionDiff struct {
	// Enabled switches region diff on for every feed that doesn't set its
	// own region_diff. With it off, feeds can still switch it on one by one,
	// as long as Exits is set.
	Enabled bool `json:"enabled"`
	// Exits are the pair every episode is downloaded through: the home
	// region first (its download is kept), then another ad market.
	Exits []string `json:"exits"`
	// FallbackExits are tried in turn when the pair's downloads are the same.
	FallbackExits []string `json:"fallback_exits"`
	// MinSharedSeconds is the shortest shared stretch counted as show audio.
	MinSharedSeconds float64 `json:"min_shared_seconds"`
	// MaxRemovedShare is the most of an episode the diff may remove before
	// the result is rejected as implausible.
	MaxRemovedShare float64 `json:"max_removed_share"`
	// OnFailure is "publish" or "hide"; see RegionDiffFailurePolicies.
	OnFailure string `json:"on_failure"`
	// Backlog is how many of a new feed's newest existing episodes are
	// processed right away; the rest are processed when first requested.
	Backlog int `json:"backlog"`
	// TrimBreakMarkers also removes the short chimes or stings a host
	// splices in around ad breaks, where they can be cut cleanly. Off by
	// default: a show's own sting at breaks would go too.
	TrimBreakMarkers bool `json:"trim_break_markers"`
}

func (regionDiff *RegionDiff) applyDefaults() {
	// Empty lists are written as [] so config.json shows the settings exist.
	if regionDiff.Exits == nil {
		regionDiff.Exits = []string{}
	}
	if regionDiff.FallbackExits == nil {
		regionDiff.FallbackExits = []string{}
	}
	if regionDiff.MinSharedSeconds == 0 {
		regionDiff.MinSharedSeconds = defaultMinSharedSeconds
	}
	if regionDiff.MaxRemovedShare == 0 {
		regionDiff.MaxRemovedShare = defaultMaxRemovedShare
	}
	if regionDiff.OnFailure == "" {
		regionDiff.OnFailure = defaultOnFailure
	}
}

// validate normalises the block and rejects values region diff can't run
// with. The exits are only trimmed: whether they exist and form a usable
// pair is checked by the module at start-up, which stays off with a warning
// rather than stopping Solstein (see docs/region-diff.md).
func (regionDiff *RegionDiff) validate() error {
	regionDiff.Exits = trimNames(regionDiff.Exits)
	regionDiff.FallbackExits = trimNames(regionDiff.FallbackExits)
	if regionDiff.MinSharedSeconds < 0.5 || regionDiff.MinSharedSeconds > 60 {
		return fmt.Errorf("region_diff.min_shared_seconds must be between 0.5 and 60, got %g", regionDiff.MinSharedSeconds)
	}
	if regionDiff.MaxRemovedShare <= 0 || regionDiff.MaxRemovedShare > 1 {
		return fmt.Errorf("region_diff.max_removed_share must be above 0 and at most 1, got %g", regionDiff.MaxRemovedShare)
	}
	regionDiff.OnFailure = strings.ToLower(strings.TrimSpace(regionDiff.OnFailure))
	if !slices.Contains(RegionDiffFailurePolicies, regionDiff.OnFailure) {
		return fmt.Errorf("region_diff.on_failure %q must be one of %s", regionDiff.OnFailure, strings.Join(RegionDiffFailurePolicies, ", "))
	}
	if regionDiff.Backlog < 0 {
		return fmt.Errorf("region_diff.backlog must not be negative, got %d", regionDiff.Backlog)
	}
	return nil
}

func trimNames(names []string) []string {
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, strings.TrimSpace(name))
	}
	return result
}
