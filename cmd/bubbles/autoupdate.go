package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/selfupdate"
)

// updateReady is set once a newer binary is on disk than the running daemon, so
// the TUI can nudge for a restart without re-running the on-disk binary every
// sampler tick. daemonRevision is the commit the running daemon was built from,
// captured once at boot.
var (
	updateReady    atomic.Bool
	daemonRevision string
)

// noteDaemonRevision records the running build's commit at boot, for staleness
// comparison after an auto-update swaps the on-disk binary.
func noteDaemonRevision() { daemonRevision = selfupdate.CurrentRevision() }

// Auto-update: the daemon quietly keeps the on-disk bubbles binary current with
// main, and NEVER restarts the running fleet to apply it (that would interrupt
// every in-flight agent). Applying is left to the operator's next restart, which
// the TUI nudges once an update is staged. Default on; opt out with
// --auto-update=off or BUBBLES_AUTO_UPDATE=0.
//
// Everything here fails open: no network, no `go`, a build error, a "dev" build
// — any of these just skips, leaving the working binary in place.

const (
	autoUpdateTick  = 10 * time.Minute // supervisor cadence: staleness is re-checked every tick
	autoUpdateEvery = 1 * time.Hour    // network+build work at most this often (well inside GitHub's anon rate limit)
)

// autoUpdateEnabled reports whether background auto-update should run.
func autoUpdateEnabled() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("BUBBLES_AUTO_UPDATE")), "0") &&
		!strings.EqualFold(strings.TrimSpace(os.Getenv("BUBBLES_AUTO_UPDATE")), "off")
}

// updateStampPath records the last check time, so a restart-storm doesn't hammer
// GitHub and so the interval survives across daemon restarts.
func updateStampPath(baseDir string) string {
	return filepath.Join(baseDir, ".bubbles", "last-update-check")
}

func lastUpdateCheck(baseDir string) time.Time {
	data, err := os.ReadFile(updateStampPath(baseDir))
	if err != nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
	if err != nil {
		return time.Time{}
	}
	return t
}

func markUpdateCheck(baseDir string, t time.Time) {
	_ = os.WriteFile(updateStampPath(baseDir), []byte(t.Format(time.RFC3339)), 0o644)
}

// installedBinaryDir is where the running daemon's binary lives — where a rebuilt
// binary must be installed so the next `bubbles` picks it up.
func installedBinaryDir() (string, string, bool) {
	self, err := os.Executable()
	if err != nil {
		return "", "", false
	}
	if resolved, rerr := filepath.EvalSymlinks(self); rerr == nil {
		self = resolved
	}
	return filepath.Dir(self), self, true
}

// runAutoUpdate is one check cycle: rate-limited, it compares main's HEAD to the
// ON-DISK binary (not the running daemon, which may already be behind a prior
// auto-update) and rebuilds if they differ. Logging goes to stderr → daemon.log.
func runAutoUpdate(baseDir string, force bool) {
	installDir, selfPath, ok := installedBinaryDir()
	if !ok {
		return
	}
	// Staleness is checked EVERY tick, independent of the network gate and of
	// the opt-out: a binary installed by hand (`bubbles --update`, the
	// installer, `make install`) makes the running daemon stale just as surely
	// as an auto-update does, and the operator should see the restart nudge
	// within one tick, not whenever the next network check happens to run.
	refreshUpdateReady(selfPath)

	if !autoUpdateEnabled() {
		return
	}
	now := time.Now()
	if !force && !selfupdate.DueForCheck(lastUpdateCheck(baseDir), now, autoUpdateEvery) {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	remote, err := selfupdate.FetchLatestCommit(ctx)
	if err != nil {
		return // offline / rate-limited: try again next interval, silently
	}
	markUpdateCheck(baseDir, now) // only after a successful fetch

	installed := selfupdate.RevisionOf(selfPath)
	if installed != "" && selfupdate.SameCommit(installed, remote) {
		return // on-disk binary already current (staleness vs the daemon was refreshed above)
	}
	if err := selfupdate.Apply(installDir, remote); err != nil {
		fmt.Fprintf(os.Stderr, "bubbles: auto-update skipped: %v\n", err)
		return
	}
	updateReady.Store(true) // installed a newer binary; a restart will apply it
	fmt.Fprintf(os.Stderr, "bubbles: auto-update installed %s — restart (bubbles stop && bubbles) to apply; running bubbles resume\n", short(remote))
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// refreshUpdateReady sets the dashboard nudge from one comparison: is the
// binary on disk a different build from the one this daemon is running? It
// runs the on-disk binary's `version`, so it lives on the 10-minute tick, never
// on the 2-second sampler.
func refreshUpdateReady(selfPath string) {
	if daemonRevision == "" {
		return
	}
	installed := selfupdate.RevisionOf(selfPath)
	updateReady.Store(installed != "" && !selfupdate.SameCommit(daemonRevision, installed))
}
