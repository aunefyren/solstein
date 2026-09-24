package exits

import (
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

func testServer(name, country, city string) Server {
	return Server{Name: name, Location: ServerLocation{Country: country, City: city}}
}

var europeServers = []Server{
	testServer("no-osl-1", "NO", "Oslo"),
	testServer("se-sto-2", "SE", "Stockholm"),
	testServer("se-sto-1", "SE", "Stockholm"),
	testServer("se-got-1", "SE", "Gothenburg"),
	testServer("dk-cph-1", "DK", "Copenhagen"),
	testServer("de-fra-1", "DE", "Frankfurt"),
	testServer("us-nyc-1", "US", "New York"),
	testServer("br-sao-1", "BR", "São Paulo"),
	testServer("unlocated", "", ""),
}

func names(servers []Server) string {
	var result []string
	for _, server := range servers {
		result = append(result, server.Name)
	}
	return strings.Join(result, ",")
}

func mustLocations(t *testing.T, texts ...string) []Location {
	t.Helper()
	var locations []Location
	for _, text := range texts {
		location, err := ParseLocation(text)
		if err != nil {
			t.Fatal(err)
		}
		locations = append(locations, location)
	}
	return locations
}

func TestMatches(t *testing.T) {
	stockholm := testServer("se-sto-1", "SE", "Stockholm")
	stockholm.Hostname = "se-01.example.net"
	cases := []struct {
		location string
		want     bool
	}{
		{"SE", true},
		{"NO", false},
		{"SE/stockholm", true},
		{"SE/Gothenburg", false},
		{"server:SE-STO-1", true},
		{"server:se-01.example.net", true},
		{"server:se-sto-2", false},
		{"area:northern-europe", true},
		{"area:western-europe", false},
		{"continent:europe", true},
		{"continent:asia", false},
	}
	for _, c := range cases {
		if got := matches(stockholm, mustLocations(t, c.location)[0]); got != c.want {
			t.Errorf("matches(%s) = %v, want %v", c.location, got, c.want)
		}
	}

	if !matches(testServer("x", "BR", ""), mustLocations(t, "continent:south-america")[0]) ||
		!matches(testServer("x", "MX", ""), mustLocations(t, "continent:north-america")[0]) ||
		matches(testServer("x", "BR", ""), mustLocations(t, "continent:north-america")[0]) {
		t.Error("continent aliases wrong")
	}
	if matches(testServer("x", "", ""), mustLocations(t, "continent:europe")[0]) {
		t.Error("server without a location matched a continent")
	}
}

func TestTiers(t *testing.T) {
	exit := Exit{Locations: mustLocations(t, "SE/Stockholm", "SE", "area:northern-europe"), Exclude: []string{"NO"}}
	got := tiers(exit, europeServers)
	want := []string{"se-sto-1,se-sto-2", "se-got-1", "dk-cph-1"}
	if len(got) != 3 {
		t.Fatalf("tiers = %d", len(got))
	}
	for i := range want {
		if names(got[i]) != want[i] {
			t.Errorf("tier %d = %s, want %s", i, names(got[i]), want[i])
		}
	}

	exit.Strict = true
	if got := tiers(exit, europeServers); len(got) != 1 || names(got[0]) != "se-sto-1,se-sto-2" {
		t.Errorf("strict tiers = %v", got)
	}

	all := tiers(Exit{Exclude: []string{"US"}}, europeServers)
	if len(all) != 1 || strings.Contains(names(all[0]), "us-nyc") || !strings.Contains(names(all[0]), "unlocated") {
		t.Errorf("no locations: %s", names(all[0]))
	}
}

// selector runs choose with its own state, health and clock.
type selector struct {
	exit   Exit
	state  exitState
	health map[string]*health
	now    time.Time
	random *rand.Rand
}

func newSelector(exit Exit) *selector {
	if exit.Selection == "" {
		exit.Selection = SelectionSticky
	}
	return &selector{exit: exit, health: map[string]*health{}, now: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC), random: rand.New(rand.NewPCG(1, 2))}
}

func (s *selector) choose(t *testing.T) string {
	t.Helper()
	server, err := choose(s.exit, europeServers, &s.state, func(name string) *health { return s.health[name] }, s.now, s.random)
	if err != nil {
		t.Fatalf("choose: %v", err)
	}
	return server.Name
}

func (s *selector) fail(name string) time.Duration {
	if s.health[name] == nil {
		s.health[name] = &health{}
	}
	return s.health[name].recordFailure(s.now, errors.New("test failure"))
}

func TestStickyKeepsAndFailsOver(t *testing.T) {
	s := newSelector(Exit{Locations: mustLocations(t, "SE/Stockholm", "DK")})

	if got := s.choose(t); got != "se-sto-1" {
		t.Fatalf("first choice = %s, want the first by name", got)
	}
	if got := s.choose(t); got != "se-sto-1" {
		t.Errorf("sticky moved to %s", got)
	}

	s.fail("se-sto-1")
	if got := s.choose(t); got != "se-sto-2" {
		t.Errorf("after failure = %s, want the other Stockholm server", got)
	}
	s.fail("se-sto-2")
	if got := s.choose(t); got != "dk-cph-1" {
		t.Errorf("Stockholm down = %s, want the next location", got)
	}

	// Once Stockholm recovers, the exit goes back to its preferred location.
	s.now = s.now.Add(2 * time.Minute)
	if got := s.choose(t); got != "se-sto-1" {
		t.Errorf("after recovery = %s, want back to Stockholm", got)
	}
}

func TestStrictDoesNotFallBack(t *testing.T) {
	s := newSelector(Exit{Locations: mustLocations(t, "SE/Stockholm", "DK"), Strict: true})
	s.fail("se-sto-1")
	s.fail("se-sto-2")
	_, err := choose(s.exit, europeServers, &s.state, func(name string) *health { return s.health[name] }, s.now, s.random)
	if !errors.Is(err, ErrNoServer) || !strings.Contains(err.Error(), "all 2 matching servers are failing") {
		t.Errorf("strict exit fell back or gave a poor error: %v", err)
	}
}

func TestBenchGrowsAndResets(t *testing.T) {
	s := newSelector(Exit{})
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i, expected := range want {
		if got := s.fail("se-sto-1"); got != expected {
			t.Errorf("failure %d benched for %s, want %s", i+1, got, expected)
		}
	}
	s.health["se-sto-1"].recordSuccess()
	if s.health["se-sto-1"].benched(s.now) || s.fail("se-sto-1") != time.Minute {
		t.Error("success didn't reset the bench")
	}
}

func TestRandomRotates(t *testing.T) {
	s := newSelector(Exit{Locations: mustLocations(t, "SE"), Selection: SelectionRandom})
	first := s.choose(t)
	if got := s.choose(t); got != first {
		t.Errorf("random changed server within the hour: %s → %s", first, got)
	}
	previous, changes := first, 0
	for range 10 {
		s.now = s.now.Add(rotation)
		next := s.choose(t)
		if next != previous {
			changes++
		}
		previous = next
	}
	// With three Swedish servers and the current one avoided at rotation,
	// every rotation picks a different server.
	if changes != 10 {
		t.Errorf("random rotated %d of 10 times, want every time", changes)
	}
}

func TestLeastFailedPrefersTheReliable(t *testing.T) {
	s := newSelector(Exit{Locations: mustLocations(t, "SE"), Selection: SelectionLeastFailed})
	// se-got-1 failed three times, se-sto-1 once; both are off the bench now.
	for range 3 {
		s.fail("se-got-1")
	}
	s.fail("se-sto-1")
	s.now = s.now.Add(40 * time.Minute)
	if got := s.choose(t); got != "se-sto-2" {
		t.Errorf("least-failed chose %s, want the one that never failed", got)
	}
	// Failures older than an hour stop counting: after an hour everyone is
	// equal again and the first by name wins.
	s.now = s.now.Add(time.Hour)
	if count := s.health["se-got-1"].recentFailures(s.now); count != 0 {
		t.Errorf("failures older than an hour still count: %d", count)
	}
	s.state = exitState{}
	if got := s.choose(t); got != "se-got-1" {
		t.Errorf("with no recent failures chose %s, want se-got-1 (first by name)", got)
	}
}

func TestNoServerErrors(t *testing.T) {
	cases := []struct {
		exit    Exit
		servers []Server
		want    string
	}{
		{Exit{Provider: "p"}, nil, "has no servers"},
		{Exit{Provider: "p", Locations: mustLocations(t, "JP")}, europeServers, "no server of provider 'p' is in JP"},
		{Exit{Provider: "p", Exclude: []string{"NO", "SE", "DK", "DE", "US", "BR", ""}}, europeServers[:8], "every server of provider 'p' is excluded"},
	}
	for _, c := range cases {
		_, err := choose(c.exit, c.servers, &exitState{}, func(string) *health { return nil }, time.Now(), rand.New(rand.NewPCG(1, 1)))
		if !errors.Is(err, ErrNoServer) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("err = %v, want %q", err, c.want)
		}
	}
}

func TestGeographyTable(t *testing.T) {
	if len(countries) < 249 {
		t.Errorf("only %d countries", len(countries))
	}
	for _, code := range []string{"NO", "SE", "GB", "US", "TW", "XK"} {
		if !knownCountry(code) {
			t.Errorf("%s missing", code)
		}
	}
	for _, area := range []string{"northern-europe", "western-europe", "south-eastern-asia", "south-america", "caribbean"} {
		if !validArea(area) {
			t.Errorf("area %s missing", area)
		}
	}
	for _, continent := range []string{"europe", "americas", "asia", "africa", "oceania", "north-america", "south-america"} {
		if !validContinent(continent) {
			t.Errorf("continent %s missing", continent)
		}
	}
	if !inArea("NO", "northern-europe") || inArea("DE", "northern-europe") || !inArea("DE", "western-europe") {
		t.Error("area membership wrong")
	}
	if _, err := ParseLocation("area:atlantis"); err == nil || !strings.Contains(err.Error(), "northern-europe") {
		t.Errorf("unknown area error should list valid areas: %v", err)
	}
	if _, err := ParseLocation("XX"); err == nil {
		t.Error("XX accepted as a country")
	}
}
