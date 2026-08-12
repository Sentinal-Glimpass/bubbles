package main

import (
	"context"
	"testing"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/kernel"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
	"github.com/Sentinal-Glimpass/bubbles/internal/supervisor"
)

// testInboxPoll is a deliberately odd value so the inbox-drain assertion below
// proves the check takes its interval from checkDeps rather than a constant.
const testInboxPoll = 7 * time.Minute

func testCheckDeps(t *testing.T) checkDeps {
	t.Helper()
	k := kernel.New(runner.NewFake())
	k.RelaunchProbe = 0
	return checkDeps{
		k:             k,
		baseDir:       t.TempDir(),
		health:        NewHealthManager(k),
		stuck:         newStuckTracker(k),
		tempSweep:     func() {},
		inboxPoll:     testInboxPoll,
		sampler:       func() {},
		claudeUsage:   func() {},
		headroomStats: func() {},
	}
}

// TestBackgroundChecksInventory is the migration's completeness gate: it
// enumerates every background loop the process runs, by name and interval, so
// a loop left behind (or an interval quietly changed) is a test failure rather
// than an omission nobody notices.
func TestBackgroundChecksInventory(t *testing.T) {
	want := []struct {
		name  string
		every time.Duration
		phase checkPhase
	}{
		{"budget", 5 * time.Second, phaseBoot},
		{"idle", 60 * time.Second, phaseBoot},
		{"flush-held", 1 * time.Second, phaseBoot},
		{"coalesce-drain", 1 * time.Second, phaseBoot},
		{"compact-flush", 2 * time.Second, phaseBoot},
		{"inbox-drain", testInboxPoll, phaseBoot},
		{"recover-unread", 45 * time.Second, phaseBoot},
		{"schedules", 20 * time.Second, phaseBoot},
		{"sampler", 2 * time.Second, phaseBoot},
		{"claude-usage", 1 * time.Second, phaseBoot},
		{"headroom-stats", 3 * time.Second, phaseBoot},
		{"stuck-scan", stuckEvery, phaseBoot},
		{"log-rotate", 5 * time.Minute, phaseBoot},
		{"temp-config-sweep", 10 * time.Minute, phaseBoot},
		{"verifier-reap", 5 * time.Minute, phaseAfterLoad},
		{"mute-reap", 10 * time.Minute, phaseAfterLoad},
		{"health-sweep", 2 * time.Minute, phaseAfterLoad},
		{"keep-alive", 30 * time.Second, phaseAfterLoad},
		{"save-inbox", 2 * time.Second, phaseAfterLoad},
		{"save-tasks", 2 * time.Second, phaseAfterLoad},
		{"save-schedules", 2 * time.Second, phaseAfterLoad},
	}

	got := backgroundChecks(testCheckDeps(t))
	byName := map[string]bgCheck{}
	for _, c := range got {
		if _, dup := byName[c.Name]; dup {
			t.Fatalf("duplicate check name %q", c.Name)
		}
		if c.Fn == nil {
			t.Errorf("check %q has a nil Fn", c.Name)
		}
		byName[c.Name] = c
	}
	for _, w := range want {
		c, ok := byName[w.name]
		if !ok {
			t.Errorf("check %q is not registered — a background loop was left behind", w.name)
			continue
		}
		if c.Every != w.every {
			t.Errorf("check %q interval = %v, want %v (intervals must match the pre-migration loops exactly)", w.name, c.Every, w.every)
		}
		if c.phase != w.phase {
			t.Errorf("check %q phase = %v, want %v", w.name, c.phase, w.phase)
		}
		delete(byName, w.name)
	}
	for name := range byName {
		t.Errorf("unexpected check %q — add it to this test's inventory deliberately", name)
	}
	if len(got) != len(want) {
		t.Errorf("got %d checks, want %d", len(got), len(want))
	}
}

// TestBackgroundChecksRegisterCleanly proves the whole inventory is acceptable
// to the registry (no empty/duplicate names, no non-positive intervals) and
// that both phases land in the same registry.
func TestBackgroundChecksRegisterCleanly(t *testing.T) {
	checks := backgroundChecks(testCheckDeps(t))
	reg := supervisor.New(time.Now)
	for _, c := range checks {
		if err := reg.Register(c.Check); err != nil {
			t.Fatalf("Register(%q): %v", c.Name, err)
		}
	}
	if n := len(reg.Snapshot()); n != len(checks) {
		t.Fatalf("registry holds %d checks, want %d", n, len(checks))
	}
}

// TestRunChecksDrivesRegistry covers the driver: it keeps calling RunDue, and a
// panicking check neither kills the driver nor stops the other checks.
func TestRunChecksDrivesRegistry(t *testing.T) {
	reg := supervisor.New(time.Now)
	ran := make(chan struct{}, 100)
	if err := reg.Register(supervisor.Check{Name: "boom", Every: time.Millisecond, Fn: func(context.Context) error {
		panic("check exploded")
	}}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(supervisor.Check{Name: "ok", Every: time.Millisecond, Fn: func(context.Context) error {
		select {
		case ran <- struct{}{}:
		default:
		}
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runChecks(ctx, reg, 2*time.Millisecond)
	for i := 0; i < 3; i++ {
		select {
		case <-ran:
		case <-time.After(3 * time.Second):
			t.Fatalf("driver stopped running checks after %d runs", i)
		}
	}
	for _, s := range reg.Snapshot() {
		if s.Name == "boom" && !s.Panicked {
			t.Error("panicking check was not recorded as panicked")
		}
	}
}

// TestRunChecksStopsOnCancel makes sure the driver honours cancellation rather
// than leaking a goroutine for the life of the process.
func TestRunChecksStopsOnCancel(t *testing.T) {
	reg := supervisor.New(time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runChecks(ctx, reg, time.Millisecond); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runChecks did not return after cancel")
	}
}

// TestSaverStepWritesOnlyOnVersionChange pins the saver gate the old
// startSaver closure implemented: first tick always writes (seed -1), and a
// tick with an unchanged version writes nothing.
func TestSaverStepWritesOnlyOnVersionChange(t *testing.T) {
	var ver int64
	writes := 0
	step := saverStep("", nil, func() int64 { return ver }, func(string, *kernel.Kernel) error {
		writes++
		return nil
	})
	step()
	if writes != 1 {
		t.Fatalf("first tick wrote %d times, want 1", writes)
	}
	step()
	if writes != 1 {
		t.Fatalf("unchanged version wrote %d times, want 1", writes)
	}
	ver++
	step()
	if writes != 2 {
		t.Fatalf("changed version wrote %d times, want 2", writes)
	}
}
