package regiondiff

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"aunefyren/solstein/outbound"
	"aunefyren/solstein/settings"
)

// Setup builds the processor from config.json's region_diff block, given
// the exits that exist. It returns nil when region diff is off: not set up,
// or unable to run. A module that can't run doesn't stop Solstein (see
// docs/design.md): the warnings say why it is off, or which fallback exits
// were dropped.
func Setup(config settings.RegionDiff, available []string) (*Processor, []string) {
	if len(config.Exits) == 0 {
		if config.Enabled {
			return nil, []string{"region_diff.enabled is on but region_diff.exits is empty; region diff stays off. Name two exits, the home region first, e.g. [\"norway\", \"sweden\"]."}
		}
		return nil, nil
	}
	off := func(reason string) (*Processor, []string) {
		return nil, []string{reason + "; region diff stays off and episodes are served with their ads."}
	}
	if len(config.Exits) != 2 {
		return off(fmt.Sprintf("region_diff.exits must name exactly two exits, the home region first (it names %d)", len(config.Exits)))
	}
	if config.Exits[0] == config.Exits[1] {
		return off(fmt.Sprintf("region_diff.exits names %q twice: two downloads through one exit come from one region", config.Exits[0]))
	}
	for _, exit := range config.Exits {
		if reason := unavailable(exit, available); reason != "" {
			return off("region_diff.exits: " + reason)
		}
	}

	var warnings []string
	var fallbacks []string
	for _, exit := range config.FallbackExits {
		switch {
		case slices.Contains(config.Exits, exit) || slices.Contains(fallbacks, exit):
			warnings = append(warnings, fmt.Sprintf("region_diff.fallback_exits: %q is listed already; skipped.", exit))
		case unavailable(exit, available) != "":
			warnings = append(warnings, "region_diff.fallback_exits: "+unavailable(exit, available)+"; skipped.")
		default:
			fallbacks = append(fallbacks, exit)
		}
	}

	diff := DefaultOptions()
	diff.MinShared = time.Duration(config.MinSharedSeconds * float64(time.Second))
	diff.MaxRemovedShare = config.MaxRemovedShare
	processor, err := NewProcessor(ProcessorOptions{
		Enabled:       config.Enabled,
		Exits:         [2]string{config.Exits[0], config.Exits[1]},
		FallbackExits: fallbacks,
		Diff:          diff,
		HideOnFailure: config.OnFailure == "hide",
	})
	if err != nil {
		return off(err.Error())
	}
	return processor, warnings
}

// unavailable explains why an exit can't be used, or returns "".
func unavailable(exit string, available []string) string {
	switch {
	case slices.Contains(available, exit):
		return ""
	case exit == outbound.DirectExit:
		return "the direct exit is disabled (disable_direct); use a VPN exit in the home country instead"
	default:
		return fmt.Sprintf("exit %q doesn't exist (available: %s)", exit, strings.Join(available, ", "))
	}
}

// Summary describes the running processor for the start-up log.
func (processor *Processor) Summary() string {
	options := processor.options
	summary := fmt.Sprintf("comparing %s (home) with %s", options.Exits[0], options.Exits[1])
	if len(options.FallbackExits) > 0 {
		summary += ", then " + strings.Join(options.FallbackExits, ", ") + " if they agree"
	}
	if options.Enabled {
		summary += "; on for every feed that doesn't switch it off"
	} else {
		summary += "; only for feeds that switch it on"
	}
	if options.HideOnFailure {
		return summary + "; episodes that can't be processed are kept out of the feed"
	}
	return summary + "; episodes that can't be processed are published with their ads"
}
