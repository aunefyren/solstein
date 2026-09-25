package regiondiff

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"aunefyren/solstein/models"
	"aunefyren/solstein/settings"
)

func regionDiffConfig(exits, fallbacks []string) settings.RegionDiff {
	return settings.RegionDiff{Enabled: true, Exits: exits, FallbackExits: fallbacks, MinSharedSeconds: 3, MaxRemovedShare: 0.4, OnFailure: "hide"}
}

func TestSetupOff(t *testing.T) {
	available := []string{"direct", "norway", "sweden"}
	cases := []struct {
		name      string
		config    settings.RegionDiff
		available []string
		warning   string
	}{
		{"not set up", settings.RegionDiff{}, available, ""},
		{"enabled without exits", settings.RegionDiff{Enabled: true}, available, "region_diff.exits is empty"},
		{"one exit", regionDiffConfig([]string{"sweden"}, nil), available, "exactly two"},
		{"same exit twice", regionDiffConfig([]string{"sweden", "sweden"}, nil), available, "twice"},
		{"unknown exit", regionDiffConfig([]string{"norway", "atlantis"}, nil), available, `"atlantis" doesn't exist (available: direct, norway, sweden)`},
		{"direct disabled", regionDiffConfig([]string{"direct", "sweden"}, nil), []string{"norway", "sweden"}, "disable_direct"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			processor, warnings := Setup(c.config, c.available)
			if processor != nil {
				t.Fatal("region diff is on")
			}
			if c.warning == "" && len(warnings) > 0 || c.warning != "" && (len(warnings) != 1 || !strings.Contains(warnings[0], c.warning)) {
				t.Errorf("warnings = %q, want one mentioning %q", warnings, c.warning)
			}
		})
	}
}

func TestSetupOn(t *testing.T) {
	processor, warnings := Setup(regionDiffConfig([]string{"norway", "sweden"}, []string{"germany", "sweden", "atlantis", "denmark"}), []string{"direct", "norway", "sweden", "germany", "denmark"})
	if processor == nil {
		t.Fatalf("off: %q", warnings)
	}
	if len(warnings) != 2 || !strings.Contains(warnings[0], `"sweden" is listed already`) || !strings.Contains(warnings[1], `"atlantis" doesn't exist`) {
		t.Errorf("warnings = %q", warnings)
	}
	options := processor.options
	if !options.Enabled || !options.HideOnFailure || !reflect.DeepEqual(options.FallbackExits, []string{"germany", "denmark"}) ||
		options.Diff.MinShared != 3*time.Second || options.Diff.MaxRemovedShare != 0.4 || options.Diff.DurationTolerance != 0.05 {
		t.Errorf("options = %+v", options)
	}
	if summary := processor.Summary(); summary != "comparing norway (home) with sweden, then germany, denmark if they agree; on for every feed that doesn't switch it off; episodes that can't be processed are kept out of the feed" {
		t.Errorf("summary = %q", summary)
	}
}

func TestProcessorFeedSettings(t *testing.T) {
	processor, _ := Setup(settings.RegionDiff{Exits: []string{"norway", "sweden"}, FallbackExits: []string{"germany", "denmark"}, MinSharedSeconds: 2, MaxRemovedShare: 0.3, OnFailure: "publish"},
		[]string{"norway", "sweden", "germany", "denmark"})
	if processor == nil {
		t.Fatal("off")
	}
	// Off globally: only feeds that switch it on.
	if processor.Handles(models.Feed{}) || !processor.Handles(models.Feed{RegionDiff: "on"}) || processor.Handles(models.Feed{RegionDiff: "off"}) {
		t.Error("switches wrong with region diff off globally")
	}
	processor.options.Enabled = true
	if !processor.Handles(models.Feed{}) || processor.Handles(models.Feed{RegionDiff: "off"}) {
		t.Error("switches wrong with region diff on globally")
	}
	if processor.HideOnFailure(models.Feed{}) || !processor.HideOnFailure(models.Feed{RegionDiffOnFailure: "hide"}) {
		t.Error("failure policy override wrong")
	}

	pair, fallbacks := processor.exitsFor(models.Feed{})
	if pair != [2]string{"norway", "sweden"} || !reflect.DeepEqual(fallbacks, []string{"germany", "denmark"}) {
		t.Errorf("global exits = %v, %v", pair, fallbacks)
	}
	// A feed's own pair; a fallback in it isn't tried again.
	pair, fallbacks = processor.exitsFor(models.Feed{RegionDiffExits: []string{"norway", "germany"}})
	if pair != [2]string{"norway", "germany"} || !reflect.DeepEqual(fallbacks, []string{"denmark"}) {
		t.Errorf("feed exits = %v, %v", pair, fallbacks)
	}
}
