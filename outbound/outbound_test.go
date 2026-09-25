package outbound

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
)

// fakeDialer resolves made-up hostnames to chosen IPs, then connects every
// dial to one local test server whatever IP was chosen. That lets tests
// pretend a host is public (or private) without any real network.
type fakeDialer struct {
	hosts  map[string][]netip.Addr
	target string // host:port of the local test server

	mutex  sync.Mutex
	dialed []netip.AddrPort
}

func (dialer *fakeDialer) LookupIP(ctx context.Context, host string) ([]netip.Addr, error) {
	ips, ok := dialer.hosts[host]
	if !ok {
		return nil, fmt.Errorf("no such host %q", host)
	}
	return ips, nil
}

func (dialer *fakeDialer) Dial(ctx context.Context, network string, address netip.AddrPort) (net.Conn, error) {
	dialer.mutex.Lock()
	dialer.dialed = append(dialer.dialed, address)
	dialer.mutex.Unlock()
	var d net.Dialer
	return d.DialContext(ctx, "tcp", dialer.target)
}

// fakeProvider offers exits backed by fixed dialers, or unavailable ones.
type fakeProvider struct {
	dialers map[string]Dialer
	down    map[string]bool
}

func (provider fakeProvider) Exits() []string {
	var exits []string
	for exit := range provider.dialers {
		exits = append(exits, exit)
	}
	for exit := range provider.down {
		exits = append(exits, exit)
	}
	return exits
}

func (provider fakeProvider) Dialer(exit string) (Dialer, error) {
	if provider.down[exit] {
		return nil, ErrExitUnavailable
	}
	return provider.dialers[exit], nil
}

func readBody(t *testing.T, response *http.Response) string {
	t.Helper()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func mustIP(text string) netip.Addr { return netip.MustParseAddr(text) }

// newTestSetup starts a server and a Manager whose "test" exit resolves
// public.example to a public IP and private.example to a private one.
func newTestSetup(t *testing.T, handler http.Handler, allowPrivate bool) (*Manager, *fakeDialer) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	dialer := &fakeDialer{
		hosts: map[string][]netip.Addr{
			"public.example":  {mustIP("93.184.216.34")},
			"private.example": {mustIP("10.1.2.3")},
			"mixed.example":   {mustIP("192.168.1.10"), mustIP("93.184.216.35")},
		},
		target: strings.TrimPrefix(server.URL, "http://"),
	}
	manager, err := New(Options{
		UserAgent:                "Solstein/test",
		AllowPrivateDestinations: allowPrivate,
		Providers:                []Provider{fakeProvider{dialers: map[string]Dialer{"test": dialer}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager, dialer
}

func get(t *testing.T, manager *Manager, exit, url string) (*http.Response, error) {
	t.Helper()
	client, err := manager.Client(exit)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return client.Do(request)
}

func TestPublicDestinationAllowed(t *testing.T) {
	manager, dialer := newTestSetup(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fmt.Fprint(writer, request.Header.Get("User-Agent"))
	}), false)

	response, err := get(t, manager, "test", "http://public.example/feed")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer response.Body.Close()
	body := readBody(t, response)
	if body != "Solstein/test" {
		t.Errorf("User-Agent seen by server = %q", body)
	}
	if len(dialer.dialed) != 1 || dialer.dialed[0].Addr() != mustIP("93.184.216.34") || dialer.dialed[0].Port() != 80 {
		t.Errorf("dialed = %v", dialer.dialed)
	}
}

func TestPrivateDestinationBlocked(t *testing.T) {
	manager, dialer := newTestSetup(t, http.NotFoundHandler(), false)

	cases := []string{
		"http://private.example/feed",
		"http://127.0.0.1/feed",
		"http://[::1]/feed",
		"http://169.254.169.254/latest/meta-data/",
	}
	for _, url := range cases {
		_, err := get(t, manager, "test", url)
		if !errors.Is(err, ErrDestinationBlocked) {
			t.Errorf("%s: err = %v, want ErrDestinationBlocked", url, err)
		}
	}
	if len(dialer.dialed) != 0 {
		t.Errorf("blocked destinations were dialled: %v", dialer.dialed)
	}
}

func TestMixedResolutionSkipsPrivate(t *testing.T) {
	manager, dialer := newTestSetup(t, http.NotFoundHandler(), false)

	response, err := get(t, manager, "test", "http://mixed.example/")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	response.Body.Close()
	if len(dialer.dialed) != 1 || dialer.dialed[0].Addr() != mustIP("93.184.216.35") {
		t.Errorf("dialed = %v, want only the public address", dialer.dialed)
	}
}

func TestRedirectToPrivateBlocked(t *testing.T) {
	manager, _ := newTestSetup(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Host == "public.example" {
			http.Redirect(writer, request, "http://private.example/secret", http.StatusFound)
			return
		}
		fmt.Fprint(writer, "secret")
	}), false)

	_, err := get(t, manager, "test", "http://public.example/feed")
	if !errors.Is(err, ErrDestinationBlocked) {
		t.Errorf("err = %v, want ErrDestinationBlocked after redirect", err)
	}
}

func TestRedirectToOtherSchemeBlocked(t *testing.T) {
	manager, _ := newTestSetup(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "file:///etc/passwd", http.StatusFound)
	}), false)

	_, err := get(t, manager, "test", "http://public.example/feed")
	if !errors.Is(err, ErrDestinationBlocked) {
		t.Errorf("err = %v, want ErrDestinationBlocked", err)
	}
}

func TestTooManyRedirects(t *testing.T) {
	manager, _ := newTestSetup(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "/again", http.StatusFound)
	}), false)

	_, err := get(t, manager, "test", "http://public.example/start")
	if err == nil || !strings.Contains(err.Error(), "redirects") {
		t.Errorf("err = %v, want a redirect-limit error", err)
	}
}

func TestAllowPrivateDestinations(t *testing.T) {
	manager, _ := newTestSetup(t, http.NotFoundHandler(), true)

	response, err := get(t, manager, "test", "http://private.example/feed")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	response.Body.Close()
}

func TestDirectExitBlocksLoopback(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	manager, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := get(t, manager, "", server.URL); !errors.Is(err, ErrDestinationBlocked) {
		t.Errorf("direct to loopback: err = %v, want ErrDestinationBlocked", err)
	}

	manager, err = New(Options{AllowPrivateDestinations: true})
	if err != nil {
		t.Fatal(err)
	}
	response, err := get(t, manager, DirectExit, server.URL)
	if err != nil {
		t.Fatalf("direct with private allowed: %v", err)
	}
	response.Body.Close()
}

func TestCallerUserAgentKept(t *testing.T) {
	manager, _ := newTestSetup(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fmt.Fprint(writer, request.Header.Get("User-Agent"))
	}), false)
	client, _ := manager.Client("test")

	request, _ := http.NewRequest(http.MethodGet, "http://public.example/", nil)
	request.Header.Set("User-Agent", "Custom/1.0")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body := readBody(t, response)
	if body != "Custom/1.0" {
		t.Errorf("User-Agent = %q, want the caller's", body)
	}
	if request.Header.Get("User-Agent") != "Custom/1.0" {
		t.Error("caller's request was modified")
	}
}

func TestClientExits(t *testing.T) {
	provider := fakeProvider{dialers: map[string]Dialer{"sweden": &fakeDialer{}}, down: map[string]bool{"germany": true}}
	manager, err := New(Options{Providers: []Provider{provider}})
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(manager.Exits(), ","); got != "direct,germany,sweden" {
		t.Errorf("Exits = %s", got)
	}
	if _, err := manager.Client("nowhere"); !errors.Is(err, ErrUnknownExit) {
		t.Errorf("unknown exit: err = %v, want ErrUnknownExit", err)
	}

	first, _ := manager.Client("sweden")
	second, _ := manager.Client("sweden")
	if first != second {
		t.Error("client not reused for the same exit")
	}
	empty, _ := manager.Client("")
	direct, _ := manager.Client(DirectExit)
	if empty != direct {
		t.Error("empty exit name is not the direct client")
	}

	// A down exit still has a client; its requests fail as unavailable.
	if _, err := get(t, manager, "germany", "http://public.example/"); !errors.Is(err, ErrExitUnavailable) {
		t.Errorf("down exit: err = %v, want ErrExitUnavailable", err)
	}
}

func TestDuplicateExitNames(t *testing.T) {
	cases := []Options{
		{Providers: []Provider{fakeProvider{dialers: map[string]Dialer{"direct": &fakeDialer{}}}}},
		{Providers: []Provider{
			fakeProvider{dialers: map[string]Dialer{"sweden": &fakeDialer{}}},
			fakeProvider{dialers: map[string]Dialer{"sweden": &fakeDialer{}}},
		}},
	}
	for _, options := range cases {
		if _, err := New(options); err == nil {
			t.Errorf("expected an error for duplicate exit names in %+v", options)
		}
	}
}

func TestDefaultExit(t *testing.T) {
	_, dialer := newTestSetup(t, http.NotFoundHandler(), false)
	withDefault, err := New(Options{
		DefaultExit: "test",
		Providers:   []Provider{fakeProvider{dialers: map[string]Dialer{"test": dialer}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if withDefault.DefaultExit() != "test" {
		t.Errorf("DefaultExit = %s", withDefault.DefaultExit())
	}
	// A request naming no exit goes through the default: the fake dialer
	// sees the dial.
	if _, err := get(t, withDefault, "", "http://public.example/"); err != nil {
		t.Fatalf("request through the default exit: %v", err)
	}
	if len(dialer.dialed) != 1 {
		t.Errorf("default exit not used: dialed %v", dialer.dialed)
	}
	// Direct still exists when not disabled.
	if _, err := withDefault.Client(DirectExit); err != nil {
		t.Errorf("direct with a default exit: %v", err)
	}

	if _, err := New(Options{DefaultExit: "nowhere"}); err == nil || !strings.Contains(err.Error(), "not an available exit") {
		t.Errorf("unknown default exit: err = %v", err)
	}
}

func TestDisableDirect(t *testing.T) {
	provider := fakeProvider{dialers: map[string]Dialer{"norway": &fakeDialer{}}}
	manager, err := New(Options{DefaultExit: "norway", DisableDirect: true, Providers: []Provider{provider}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(manager.Exits(), ","); got != "norway" {
		t.Errorf("Exits = %s, want direct left out", got)
	}
	if _, err := manager.Client(DirectExit); !errors.Is(err, ErrDirectDisabled) || !errors.Is(err, ErrUnknownExit) {
		t.Errorf("direct when disabled: err = %v", err)
	}
	if client, err := manager.Client(""); err != nil || client == nil {
		t.Errorf("no exit named: %v", err)
	}

	if _, err := New(Options{DisableDirect: true}); err == nil || !strings.Contains(err.Error(), "needs a default_exit") {
		t.Errorf("disable_direct without default_exit: err = %v", err)
	}
	if _, err := New(Options{DisableDirect: true, DefaultExit: DirectExit}); err == nil {
		t.Error("disable_direct with default_exit direct accepted")
	}
}

func TestTransportIgnoresProxyEnvironment(t *testing.T) {
	manager, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	client, _ := manager.Client(DirectExit)
	transport := client.Transport.(userAgentTransport).next.(*http.Transport)
	if transport.Proxy != nil {
		t.Error("transport uses a proxy function; HTTP_PROXY would bypass the exit")
	}
}

// locatingProvider is a fakeProvider that knows its exits' countries.
type locatingProvider struct {
	fakeProvider
	countries map[string][]string
}

func (provider locatingProvider) ExitCountries(exit string) ([]string, bool) {
	countries, ok := provider.countries[exit]
	return countries, ok
}

func (provider locatingProvider) ExitCountry(exit string) (string, bool) {
	if countries := provider.countries[exit]; len(countries) > 0 {
		return countries[0], true
	}
	return "", false
}

func TestExitCountries(t *testing.T) {
	located := locatingProvider{fakeProvider: fakeProvider{dialers: map[string]Dialer{"sweden": nil}}, countries: map[string][]string{"sweden": {"SE"}}}
	plain := fakeProvider{dialers: map[string]Dialer{"vps": nil}}
	manager, err := New(Options{Providers: []Provider{located, plain}})
	if err != nil {
		t.Fatal(err)
	}
	if countries, known := manager.ExitCountries("sweden"); !known || len(countries) != 1 || countries[0] != "SE" {
		t.Errorf("sweden: %v, %v", countries, known)
	}
	if country, known := manager.ExitCountry("sweden"); !known || country != "SE" {
		t.Errorf("sweden now: %q, %v", country, known)
	}
	for _, exit := range []string{DirectExit, "vps", "nowhere"} {
		if _, known := manager.ExitCountries(exit); known {
			t.Errorf("%s: countries reported as known", exit)
		}
		if _, known := manager.ExitCountry(exit); known {
			t.Errorf("%s: country reported as known", exit)
		}
	}
}
