package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGoInstallTargetsMainNotLatest is the regression test for the bug that
// shipped a two-month-old build to every user: `go install …@latest` resolves to
// the highest semver TAG, not the branch tip. bubbles ships from main, so the
// updater must pin @main. If this ever reverts to @latest, users silently get
// whatever stale tag exists.
func TestGoInstallTargetsMainNotLatest(t *testing.T) {
	args := goInstallArgs()
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "@latest") {
		t.Fatalf("go install must not use @latest (it resolves to the newest TAG, not the branch tip): %q", joined)
	}
	if !strings.HasSuffix(joined, "/cmd/bubbles@main") {
		t.Fatalf("go install must target the branch tip cmd/bubbles@main, got %q", joined)
	}
	if args[0] != "install" {
		t.Fatalf("first arg must be install, got %q", args[0])
	}
}

func TestChooseUpdateStrategy(t *testing.T) {
	cases := []struct {
		hasGo, hasCurl bool
		want           updateStrategy
	}{
		{true, true, updateViaGo},     // go preferred: rebuilds just bubbles, in place
		{true, false, updateViaGo},    // go alone is enough
		{false, true, updateViaInstaller}, // no go: installer bootstraps it
		{false, false, updateNoTool},  // neither: cannot self-update
	}
	for _, c := range cases {
		if got := chooseUpdateStrategy(c.hasGo, c.hasCurl); got != c.want {
			t.Fatalf("chooseUpdateStrategy(go=%v,curl=%v)=%v, want %v", c.hasGo, c.hasCurl, got, c.want)
		}
	}
}

// TestUpdateRestartHintReflectsALiveDaemon: the hint must tell the operator to
// restart ONLY when a fleet is actually running here (a rebuild is inert until
// the daemon restarts), and must not cry wolf when nothing is running.
func TestUpdateRestartHintReflectsALiveDaemon(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, ".bubbles"), 0o755); err != nil {
		t.Fatal(err)
	}

	// No pidfile: nothing to restart.
	if h := updateRestartHint(base); strings.Contains(h, "restart") {
		t.Fatalf("with no daemon the hint must not tell the operator to restart: %q", h)
	}

	// Our own pid is alive, so it stands in for a running daemon.
	if err := os.WriteFile(pidFile(base), []byte(strings.TrimSpace(itoa(os.Getpid()))), 0o644); err != nil {
		t.Fatal(err)
	}
	if h := updateRestartHint(base); !strings.Contains(h, "bubbles stop") {
		t.Fatalf("with a live daemon the hint must give the restart command: %q", h)
	}

	// A dead pid must read as not-running (stale pidfile after a crash/reboot).
	if err := os.WriteFile(pidFile(base), []byte("2147483000"), 0o644); err != nil {
		t.Fatal(err)
	}
	if daemonAlive(base) {
		t.Fatal("a pid that does not exist must not read as a live daemon")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
