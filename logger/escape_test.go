package logger

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
)

// A title from a source feed can't add lines of its own to the log, or send
// escape sequences to the terminal reading it.
func TestLogEscapesControlCharacters(t *testing.T) {
	var output bytes.Buffer
	log := newLogger(&output, logrus.InfoLevel)
	log.Info("Cached episode 'Evil\n[INFO]: 2026-10-01 12:00:00 - Signed in 'admin'\r\x1b[2J x\tok' of 'Show'.")

	got := output.String()
	if strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, "\n") {
		t.Fatalf("one entry took more than one line:\n%q", got)
	}
	want := "Cached episode 'Evil\\n[INFO]: 2026-10-01 12:00:00 - Signed in 'admin'\\r\\x1b[2J\\u2028x\tok' of 'Show'.\n"
	if !strings.HasSuffix(got, want) {
		t.Errorf("logged %q, want it to end %q", got, want)
	}
	if !strings.HasPrefix(got, "[INFO]: ") {
		t.Errorf("the entry lost its level: %q", got)
	}
}

func TestEscapeControl(t *testing.T) {
	for in, want := range map[string]string{
		"plain Ærlig talt — «quotes»": "plain Ærlig talt — «quotes»",
		"a\nb\r\nc":                   `a\nb\r\nc`,
		"bell\a del\x7f c1\u0085":     `bell\x07 del\x7f c1\x85`,
		"tab\tstays":                  "tab\tstays",
		"line\u2028para\u2029":        "line\\u2028para\\u2029",
	} {
		if got := escapeControl(in); got != want {
			t.Errorf("escapeControl(%q) = %q, want %q", in, got, want)
		}
	}
}
