package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/kernel"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
)

// newStuckFixture builds a kernel with one hot bubble and a tracker whose clock
// the test drives. No sleeping anywhere.
func newStuckFixture(t *testing.T) (*kernel.Kernel, *runner.FakeRunner, *stuckTracker, addr.Address, *time.Time) {
	t.Helper()
	fr := runner.NewFake()
	k := kernel.New(fr)
	k.RelaunchProbe = 0
	a, err := k.Spawn(addr.Root, "w", filepath.Join(t.TempDir(), "w"), runner.SpawnOpts{Persona: "w"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	// EnsureAlive here is TEST SETUP only — it is how a fixture gets a hot
	// bubble to observe. Neither the detector nor its wiring ever calls it.
	if sess := k.EnsureAlive(a); sess == nil {
		t.Fatal("EnsureAlive returned nil: fixture has no hot session")
	}
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	st := newStuckTracker(k)
	st.now = func() time.Time { return now }
	return k, fr, st, a, &now
}

func has(list []addr.Address, a addr.Address) bool {
	for _, x := range list {
		if x == a {
			return true
		}
	}
	return false
}

// The wired tracker reports a hot bubble that holds unconsumed mail and
// has produced no new output across two consecutive samples.
func TestStuckTrackerReportsWedgedBubble(t *testing.T) {
	k, fr, st, a, now := newStuckFixture(t)
	s := fr.Session(a)
	s.SetOutput("waiting for your approval")
	s.SetLastActivity(now.Add(-stuckThreshold))
	if _, err := k.Send(addr.Root, a, "do the thing", "body", 0, false); err != nil {
		t.Fatalf("send: %v", err)
	}

	st.Step() // first sighting: cannot establish "unchanged"
	if got := st.Stuck(); len(got) != 0 {
		t.Fatalf("first sample reported %v, want nothing", got)
	}
	st.Step()
	if got := st.Stuck(); !has(got, a) {
		t.Fatalf("Stuck = %v, want it to contain %v", got, a)
	}
}

// Output that moves between samples means the bubble is working.
func TestStuckTrackerIgnoresMovingOutput(t *testing.T) {
	k, fr, st, a, now := newStuckFixture(t)
	s := fr.Session(a)
	s.SetOutput("step 1")
	s.SetLastActivity(now.Add(-24 * time.Hour))
	if _, err := k.Send(addr.Root, a, "do the thing", "body", 0, false); err != nil {
		t.Fatalf("send: %v", err)
	}
	st.Step()
	s.SetOutput("step 1\nstep 2")
	st.Step()
	if got := st.Stuck(); len(got) != 0 {
		t.Fatalf("Stuck = %v, want nothing (output moved)", got)
	}
}

// A quiet bubble with an empty inbox is idle, not stuck.
func TestStuckTrackerIgnoresIdleBubble(t *testing.T) {
	_, fr, st, a, now := newStuckFixture(t)
	s := fr.Session(a)
	s.SetOutput("done")
	s.SetLastActivity(now.Add(-72 * time.Hour))
	st.Step()
	st.Step()
	if got := st.Stuck(); len(got) != 0 {
		t.Fatalf("Stuck = %v, want nothing (no pending mail)", got)
	}
}

// The scan is pure observation: sampling must never launch, resume or write to
// anything — a cold bubble stays cold and a hot one is not nudged.
func TestStuckTrackerNeverWakesOrWritesToBubbles(t *testing.T) {
	k, fr, st, a, now := newStuckFixture(t)
	s := fr.Session(a)
	s.SetOutput("wedged")
	s.SetLastActivity(now.Add(-stuckThreshold))
	if _, err := k.Send(addr.Root, a, "hello", "body", 0, false); err != nil {
		t.Fatalf("send: %v", err)
	}
	launches := len(fr.Launches)
	written := s.Written()

	for i := 0; i < 5; i++ {
		st.Step()
	}
	if !has(st.Stuck(), a) {
		t.Fatalf("expected %v to be reported, got %v", a, st.Stuck())
	}
	if got := len(fr.Launches); got != launches {
		t.Errorf("scanning launched %d session(s); the detector must never wake a bubble", got-launches)
	}
	if s.Written() != written {
		t.Errorf("scanning wrote %q to the session; the detector reports only", s.Written()[len(written):])
	}
	if !s.Alive() || s.Closed() {
		t.Error("scanning killed the session; the detector reports only")
	}
}

// StuckSamples walks the LIVE session table only: a bubble that is not hot is
// simply absent, never paged in to be inspected.
func TestStuckSamplesOnlyCoversHotBubbles(t *testing.T) {
	k, fr, _, a, _ := newStuckFixture(t)
	launches := len(fr.Launches)
	fr.Session(a).Die() // process gone: the bubble is no longer hot
	if got := k.StuckSamples(); len(got) != 0 {
		t.Fatalf("StuckSamples on a cold fleet = %v, want empty", got)
	}
	if got := len(fr.Launches); got != launches {
		t.Errorf("StuckSamples paged in %d bubble(s); it must sample hot bubbles only", got-launches)
	}
}
