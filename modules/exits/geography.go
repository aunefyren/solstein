package exits

import (
	"slices"
	"sort"
)

// country is one row of the countries table.
type country struct {
	name               string
	continent          string
	subRegion          string
	intermediateRegion string
}

// continentAliases are continents people name that M49 splits or merges
// differently: M49 has one "americas".
var continentAliases = map[string]func(country) bool{
	"north-america": func(c country) bool {
		return c.subRegion == "northern-america" || c.intermediateRegion == "central-america" || c.intermediateRegion == "caribbean"
	},
	"south-america": func(c country) bool { return c.intermediateRegion == "south-america" },
}

func knownCountry(code string) bool {
	_, ok := countries[code]
	return ok
}

// inArea reports whether a country is in an M49 sub-region or intermediate
// region, e.g. "northern-europe" or "south-america".
func inArea(code, area string) bool {
	c, ok := countries[code]
	return ok && area != "" && (c.subRegion == area || c.intermediateRegion == area)
}

// inContinent reports whether a country is on an M49 region ("europe") or
// one of the aliases ("north-america").
func inContinent(code, continent string) bool {
	c, ok := countries[code]
	if !ok {
		return false
	}
	if alias, ok := continentAliases[continent]; ok {
		return alias(c)
	}
	return continent != "" && c.continent == continent
}

// areas and continents list the valid names, for validation and messages.
func areas() []string {
	set := map[string]bool{}
	for _, c := range countries {
		for _, name := range []string{c.subRegion, c.intermediateRegion} {
			if name != "" {
				set[name] = true
			}
		}
	}
	return sortedSet(set)
}

func continents() []string {
	set := map[string]bool{}
	for _, c := range countries {
		if c.continent != "" {
			set[c.continent] = true
		}
	}
	for alias := range continentAliases {
		set[alias] = true
	}
	return sortedSet(set)
}

func sortedSet(set map[string]bool) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func validArea(name string) bool      { return slices.Contains(areas(), name) }
func validContinent(name string) bool { return slices.Contains(continents(), name) }
