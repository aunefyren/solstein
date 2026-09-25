package regiondiff

import (
	"path/filepath"
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
			processor, warnings := Setup(c.config, c.available, nil, "")
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
	processor, warnings := Setup(regionDiffConfig([]string{"norway", "sweden"}, []string{"germany", "sweden", "atlantis", "denmark"}), []string{"direct", "norway", "sweden", "germany", "denmark"}, nil, "")
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
		[]string{"norway", "sweden", "germany", "denmark"}, nil, "")
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

// fakeLocator knows each exit's possible countries; the first is the one in
// use now.
type fakeLocator map[string][]string

func (locator fakeLocator) ExitCountries(exit string) ([]string, bool) {
	countries, ok := locator[exit]
	return countries, ok
}

func (locator fakeLocator) ExitCountry(exit string) (string, bool) {
	if countries := locator[exit]; len(countries) > 0 {
		return countries[0], true
	}
	return "", false
}

func TestSetupSameCountry(t *testing.T) {
	available := []string{"direct", "norway", "oslo", "sweden", "nordic", "germany"}
	locator := fakeLocator{
		"norway":  {"NO"},
		"oslo":    {"NO"},
		"sweden":  {"SE"},
		"nordic":  {"SE", "NO", "DK"}, // loose: can fall back to NO
		"germany": {"DE"},
	}
	// Both only in Norway: off.
	processor, warnings := Setup(regionDiffConfig([]string{"norway", "oslo"}, nil), available, locator, "")
	if processor != nil || len(warnings) != 1 || !strings.Contains(warnings[0], `"norway" and "oslo" both come out in NO only`) {
		t.Errorf("same country: processor %v, warnings %q", processor != nil, warnings)
	}
	// Can overlap through fallback locations: on, with a warning.
	processor, warnings = Setup(regionDiffConfig([]string{"norway", "nordic"}, nil), available, locator, "")
	if processor == nil || len(warnings) != 1 || !strings.Contains(warnings[0], `can both come out in NO`) {
		t.Errorf("possible overlap: processor %v, warnings %q", processor != nil, warnings)
	}
	// A fallback only in the home country is dropped.
	processor, warnings = Setup(regionDiffConfig([]string{"norway", "sweden"}, []string{"oslo", "germany"}), available, locator, "")
	if processor == nil || len(warnings) != 1 || !strings.Contains(warnings[0], `"oslo" both come out in NO only; skipped`) ||
		!reflect.DeepEqual(processor.options.FallbackExits, []string{"germany"}) {
		t.Errorf("fallback in the home country: warnings %q", warnings)
	}
	// direct's country is unknown: never flagged.
	if processor, warnings = Setup(regionDiffConfig([]string{"direct", "norway"}, nil), available, locator, ""); processor == nil || len(warnings) != 0 {
		t.Errorf("direct: processor %v, warnings %q", processor != nil, warnings)
	}
}

func TestRecipe(t *testing.T) {
	config := regionDiffConfig([]string{"norway", "sweden"}, []string{"germany"})
	processor, _ := Setup(config, []string{"norway", "sweden", "germany", "denmark"}, nil, "")
	recipes := map[string]string{
		"global":         processor.Recipe(models.Feed{}),
		"feed pair":      processor.Recipe(models.Feed{RegionDiffExits: []string{"norway", "denmark"}}),
		"markers":        processor.Recipe(models.Feed{RegionDiffTrimBreakMarkers: "on"}),
		"failure policy": processor.Recipe(models.Feed{RegionDiffOnFailure: "publish"}),
	}
	if want := "v3 norway→sweden, fallback germany, shared ≥3s, removed ≤40%"; recipes["global"] != want {
		t.Errorf("recipe = %q, want %q", recipes["global"], want)
	}
	// Anything that changes the cleaned file changes the recipe; the failure
	// policy doesn't change a cleaned file.
	if recipes["feed pair"] == recipes["global"] || recipes["markers"] == recipes["global"] {
		t.Errorf("recipes don't tell settings apart: %q", recipes)
	}
	if recipes["failure policy"] != recipes["global"] {
		t.Errorf("failure policy changed the recipe: %q", recipes["failure policy"])
	}
}

func TestSetupKeepsFailedDownloadsWhenAsked(t *testing.T) {
	config := regionDiffConfig([]string{"norway", "sweden"}, nil)
	processor, _ := Setup(config, []string{"norway", "sweden"}, nil, "/config")
	if processor.options.FailureDir != "" {
		t.Errorf("kept without being asked: %q", processor.options.FailureDir)
	}
	config.KeepFailedDownloads = true
	processor, _ = Setup(config, []string{"norway", "sweden"}, nil, "/config")
	if want := filepath.Join("/config", "regiondiff-failures"); processor.options.FailureDir != want || !strings.Contains(processor.Summary(), want) {
		t.Errorf("failure dir %q, summary %q", processor.options.FailureDir, processor.Summary())
	}
	if processor.options.SuccessDir != "" {
		t.Errorf("successes kept without being asked: %q", processor.options.SuccessDir)
	}
	config.KeepSuccessfulDownloads = true
	processor, _ = Setup(config, []string{"norway", "sweden"}, nil, "/config")
	if want := filepath.Join("/config", "regiondiff-successes"); processor.options.SuccessDir != want || !strings.Contains(processor.Summary(), want) {
		t.Errorf("success dir %q, summary %q", processor.options.SuccessDir, processor.Summary())
	}
}
