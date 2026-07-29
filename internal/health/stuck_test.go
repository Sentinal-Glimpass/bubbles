package health

import (
	"testing"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

// Every test here is a guard against a FALSE POSITIVE — the failure mode that
// matters, because the reported list feeds an operator panel and a wrong entry
// invites a human to disturb a bubble that is working. Time is supplied, never
// read: there are no sleeps and no wall-clock dependence anywhere in this file.

const threshold = 5 * time.Minute

// base is an arbitrary fixed instant; nothing depends on its value.
var base = time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

func cfg() Config { return Config{Threshold: threshold} }

// stale is a LastActivity exactly `threshold` before now.
var stale = base.Add(-threshold)

func mk(a string, out string, mail int) Sample {
	return Sample{Addr: addr.Address(a), LastActivity: stale, RecentOutput: out, UnreadMail: mail, Alive: true}
}

func want(t *testing.T, got []addr.Address, expect ...addr.Address) {
	t.Helper()
	if len(got) != len(expect) {
		t.Fatalf("Stuck = %v, want %v", got, expect)
	}
	for i := range got {
		if got[i] != expect[i] {
			t.Fatalf("Stuck = %v, want %v", got, expect)
		}
	}
}

func TestStuckReportsPendingMailWithUnchangedOutput(t *testing.T) {
	prev := []Sample{mk("0.1", "thinking...", 1)}
	cur := []Sample{mk("0.1", "thinking...", 1)}
	want(t, Stuck(cfg(), prev, cur, base), addr.Address("0.1"))
}

// A bubble whose output MOVED is working, however long it has been at it.
func TestStuckNeverReportsWhenOutputChanged(t *testing.T) {
	long := base.Add(-72 * time.Hour)
	prev := []Sample{{Addr: "0.1", LastActivity: long, RecentOutput: "step 1", UnreadMail: 3, Alive: true}}
	cur := []Sample{{Addr: "0.1", LastActivity: long, RecentOutput: "step 1\nstep 2", UnreadMail: 3, Alive: true}}
	want(t, Stuck(cfg(), prev, cur, base))
}

// No mail means idle, not stuck: that is EvictIdle's business.
func TestStuckNeverReportsWithoutPendingMail(t *testing.T) {
	quiet := base.Add(-30 * 24 * time.Hour)
	prev := []Sample{{Addr: "0.1", LastActivity: quiet, RecentOutput: "same", UnreadMail: 0, Alive: true}}
	cur := []Sample{{Addr: "0.1", LastActivity: quiet, RecentOutput: "same", UnreadMail: 0, Alive: true}}
	want(t, Stuck(cfg(), prev, cur, base))
}

func TestStuckNeverReportsDeadSession(t *testing.T) {
	s := mk("0.1", "same", 2)
	s.Alive = false
	want(t, Stuck(cfg(), []Sample{s}, []Sample{s}, base))
}

// One sample can never establish "unchanged".
func TestStuckNeverReportsFirstSighting(t *testing.T) {
	cur := []Sample{mk("0.1", "same", 1)}
	want(t, Stuck(cfg(), nil, cur, base))
	// a different bubble in prev is still no observation of this one
	want(t, Stuck(cfg(), []Sample{mk("0.2", "same", 1)}, cur, base))
}

func TestStuckThresholdBoundaryIsExact(t *testing.T) {
	at := []Sample{{Addr: "0.1", LastActivity: base.Add(-threshold), RecentOutput: "same", UnreadMail: 1, Alive: true}}
	want(t, Stuck(cfg(), at, at, base), addr.Address("0.1"))

	under := []Sample{{Addr: "0.1", LastActivity: base.Add(-threshold + time.Nanosecond), RecentOutput: "same", UnreadMail: 1, Alive: true}}
	want(t, Stuck(cfg(), under, under, base))
}

func TestStuckZeroLastActivityIsNeverReported(t *testing.T) {
	s := []Sample{{Addr: "0.1", RecentOutput: "same", UnreadMail: 1, Alive: true}}
	want(t, Stuck(cfg(), s, s, base))
}

func TestStuckNonPositiveThresholdDisablesDetection(t *testing.T) {
	s := []Sample{mk("0.1", "same", 1)}
	want(t, Stuck(Config{}, s, s, base))
	want(t, Stuck(Config{Threshold: -time.Second}, s, s, base))
}

// Verbatim copies of the only three marker strings the repo matches against
// RecentOutput today. Sources of truth (both unexported, deliberately not
// exported just for this test):
// Named by SYMBOL, not line number, because a line pointer here has already
// drifted once:
//   - internal/runner/local.go   resumeMenuOpt1 / resumeMenuOpt2
//   - internal/kernel            resumeLost(), which matches "No conversation found"
const (
	resumeMenuOpt1      = "Resume from summary"
	resumeMenuOpt2      = "Resume full session as-is"
	noConversationFound = "No conversation found"
)

// The detector must not pattern-match terminal text. A bubble sitting on the
// resume menu or a failed --resume is not, by that fact alone, stuck.
func TestStuckIgnoresKnownMarkerStrings(t *testing.T) {
	for _, marker := range []string{resumeMenuOpt1, resumeMenuOpt2, noConversationFound,
		resumeMenuOpt1 + "\n" + resumeMenuOpt2} {
		// present, but with no pending mail: never reported.
		idle := []Sample{{Addr: "0.1", LastActivity: stale, RecentOutput: marker, UnreadMail: 0, Alive: true}}
		want(t, Stuck(cfg(), idle, idle, base))

		// present, mail pending, but output still moving: never reported.
		prev := []Sample{{Addr: "0.1", LastActivity: stale, RecentOutput: marker, UnreadMail: 1, Alive: true}}
		cur := []Sample{{Addr: "0.1", LastActivity: stale, RecentOutput: marker + " >", UnreadMail: 1, Alive: true}}
		want(t, Stuck(cfg(), prev, cur, base))
	}
}

// The marker strings are also not a shield: a genuinely wedged bubble is still
// reported when its ring happens to contain one.
func TestStuckMarkerStringsAreNotAShield(t *testing.T) {
	s := []Sample{{Addr: "0.1", LastActivity: stale, RecentOutput: noConversationFound, UnreadMail: 1, Alive: true}}
	want(t, Stuck(cfg(), s, s, base), addr.Address("0.1"))
}

func TestStuckResultIsSortedAndFiltersMixedFleet(t *testing.T) {
	prev := []Sample{
		mk("0.3", "same-3", 1),
		mk("0.1", "same-1", 2),
		mk("0.2", "moving", 1),
		mk("0.4", "same-4", 0), // no mail
	}
	cur := []Sample{
		mk("0.3", "same-3", 1),
		mk("0.1", "same-1", 2),
		mk("0.2", "moving on", 1),
		mk("0.4", "same-4", 0),
		mk("0.5", "new", 5), // first sighting
	}
	want(t, Stuck(cfg(), prev, cur, base), addr.Address("0.1"), addr.Address("0.3"))
}
