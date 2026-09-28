package console

import (
	"regexp"
	"strings"
	"testing"
)

func runConsole(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	code := Run(append(args, "-configdir", dir), func(string) string { return "" }, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestConsoleUsers(t *testing.T) {
	dir := t.TempDir()
	if code, out, _ := runConsole(t, dir, "list"); code != 0 || !strings.Contains(out, "No users yet") {
		t.Errorf("empty list = %d %q", code, out)
	}

	code, out, _ := runConsole(t, dir, "add", "Alice")
	oneTime := regexp.MustCompile(`One-time password: (\S+)`).FindStringSubmatch(out)
	if code != 0 || !strings.Contains(out, "Added user 'alice'") || oneTime == nil || len(oneTime[1]) < 20 {
		t.Fatalf("add = %d %q", code, out)
	}
	if code, _, errOut := runConsole(t, dir, "add", "alice"); code != 1 || !strings.Contains(errOut, "already a user") {
		t.Errorf("add twice = %d %q", code, errOut)
	}
	if code, _, errOut := runConsole(t, dir, "add", "no/slash"); code != 1 || !strings.Contains(errOut, "Not a valid username") {
		t.Errorf("bad name = %d %q", code, errOut)
	}
	if code, out, _ := runConsole(t, dir, "list"); code != 0 || !regexp.MustCompile(`alice\s+off\s+one-time\s+never`).MatchString(out) {
		t.Errorf("list = %d %q", code, out)
	}

	code, out, _ = runConsole(t, dir, "reset-password", "alice")
	again := regexp.MustCompile(`One-time password: (\S+)`).FindStringSubmatch(out)
	if code != 0 || again == nil || again[1] == oneTime[1] || !strings.Contains(out, "ended 0 sessions") {
		t.Errorf("reset-password = %d %q", code, out)
	}
	if code, out, _ := runConsole(t, dir, "reset-mfa", "alice"); code != 0 || !strings.Contains(out, "Removed the authenticator of 'alice'") {
		t.Errorf("reset-mfa = %d %q", code, out)
	}
	if code, _, errOut := runConsole(t, dir, "reset-mfa", "bob"); code != 1 || !strings.Contains(errOut, "No user 'bob'") {
		t.Errorf("unknown user = %d %q", code, errOut)
	}
	if code, out, _ := runConsole(t, dir, "delete", "alice"); code != 0 || !strings.Contains(out, "Deleted user 'alice'") {
		t.Errorf("delete = %d %q", code, out)
	}

	for _, args := range [][]string{{}, {"frobnicate", "x"}, {"add"}, {"list", "extra"}} {
		var stdout, stderr strings.Builder
		if code := Run(args, func(string) string { return "" }, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "Usage: solstein user") {
			t.Errorf("Run(%v) = %d, want the usage", args, code)
		}
	}
}
