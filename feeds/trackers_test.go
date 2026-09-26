package feeds

import "testing"

func TestEmbeddedURL(t *testing.T) {
	cases := []struct {
		raw, want string
	}{
		// The Always Sunny chain, hop by hop.
		{"https://www.podtrac.com/pts/redirect.mp3/pdst.fm/e/pfx.vpixl.com/usvBQ/traffic.megaphone.fm/TPC1.mp3?updated=1688948785",
			"https://pdst.fm/e/pfx.vpixl.com/usvBQ/traffic.megaphone.fm/TPC1.mp3?updated=1688948785"},
		{"https://chrt.fm/track/9DD8D/arttrk.com/p/PRGN3/traffic.megaphone.fm/TPC1.mp3?updated=1",
			"https://arttrk.com/p/PRGN3/traffic.megaphone.fm/TPC1.mp3?updated=1"},
		{"https://arttrk.com/p/PRGN3/traffic.megaphone.fm/TPC1.mp3", "https://traffic.megaphone.fm/TPC1.mp3"},
		// Darknet Diaries: Podtrac in front of Dovetail.
		{"https://www.podtrac.com/pts/redirect.mp3/dovetail.prxu.org/7057/9d4c/darknet-diaries-ep169-mod.mp3",
			"https://dovetail.prxu.org/7057/9d4c/darknet-diaries-ep169-mod.mp3"},
		// A scheme in the path, whole or collapsed by a proxy.
		{"https://dts.podtrac.com/redirect.mp3/http://cdn.example.com/a.mp3", "http://cdn.example.com/a.mp3"},
		{"https://dts.podtrac.com/redirect.mp3/https:/cdn.example.com/a.mp3", "https://cdn.example.com/a.mp3"},
		// Audio hosts' own URLs: nothing embedded.
		{"https://traffic.megaphone.fm/TPC1.mp3?updated=1", ""},
		{"https://dovetail.prxu.org/7057/9d4c/darknet-diaries-ep169-mod.mp3", ""},
		{"https://sphinx.acast.com/p/open/s/5f1c/e/6a2b/media.mp3", ""},
		{"https://audio4.redcircle.com/episodes/1cfe3643/stream.mp3", ""},
		{"https://cdn.example.com/show/episode.m4a", ""},
		{"https://cdn.example.com/1.0.2/episode.mp3", ""}, // a version number isn't a host
		{"ftp://www.podtrac.com/pts/redirect.mp3/cdn.example.com/a.mp3", ""},
		{"not a url\x7f", ""},
	}
	for _, c := range cases {
		got, ok := EmbeddedURL(c.raw)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("EmbeddedURL(%q) = %q, %v; want %q", c.raw, got, ok, c.want)
		}
	}
}

func TestWithoutTrackers(t *testing.T) {
	chain := "https://www.podtrac.com/pts/redirect.mp3/pdst.fm/e/pfx.vpixl.com/usvBQ/verifi.podscribe.com/rss/p/claritaspod.com/measure/chrt.fm/track/9DD8D/arttrk.com/p/PRGN3/traffic.megaphone.fm/TPC6209325126.mp3?updated=1688948785"
	if got, want := WithoutTrackers(chain), "https://traffic.megaphone.fm/TPC6209325126.mp3?updated=1688948785"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if plain := "https://sphinx.acast.com/p/open/s/1/e/2/media.mp3"; WithoutTrackers(plain) != plain {
		t.Error("an audio host's own URL changed")
	}
}
