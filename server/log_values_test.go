package server

import "testing"

// orDefault only ever puts one of three constants in the log.
func TestOrDefault(t *testing.T) {
	for in, want := range map[string]string{"on": "on", "off": "off", "": "default", "on\n[INFO]: forged": "default"} {
		if got := orDefault(in); got != want {
			t.Errorf("orDefault(%q) = %q, want %q", in, got, want)
		}
	}
}
