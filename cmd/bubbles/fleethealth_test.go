package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/costmeter"
	"github.com/Sentinal-Glimpass/bubbles/internal/kernel"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
	"github.com/Sentinal-Glimpass/bubbles/internal/supervisor"
	"github.com/Sentinal-Glimpass/bubbles/internal/transcript"
)

// TestFleetHealthSnapshotOmitsUnmeasuredSources is the producer half of the
// panel's central rule: a source that has not reported must reach the TUI as
// nil, never as a zero. A zero on the panel reads as "verified healthy"; on a
// fleet where the stuck scan has never completed a sweep, that is a lie.
func TestFleetHealthSnapshotOmitsUnmeasuredSources(t *testing.T) {
	fr := runner.NewFake()
	k := kernel.New(fr)
	k.RelaunchProbe = 0
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	// Nothing has reported: no stuck tracker, no registry, no context gauge.
	msg := fleetHealthSnapshot(k, 0, nil, nil, now)
	if msg.Stuck != nil {
		t.Fatalf("stuck must be absent with no detector: %d", *msg.Stuck)
	}
	if msg.OverContext != nil {
		t.Fatalf("context must be absent before the pump measures anything: %d", *msg.OverContext)
	}
	if msg.FailingChecks != nil || msg.WedgedChecks != nil {
		t.Fatal("check counts must be absent with no registry")
	}
	// The kernel always tracks relaunch failures, so this one IS measured.
	if msg.CrashLooping == nil || *msg.CrashLooping != 0 {
		t.Fatalf("crash-loop count is always measured: %v", msg.CrashLooping)
	}

	// A tracker that has stepped only once still has no verdict: the detector is
	// a difference between two samples.
	st := newStuckTracker(k)
	st.now = func() time.Time { return now }
	st.Step()
	if msg := fleetHealthSnapshot(k, 0, st, nil, now); msg.Stuck != nil {
		t.Fatalf("one sample is not a verdict: %d", *msg.Stuck)
	}
	st.Step()
	msg = fleetHealthSnapshot(k, 0, st, nil, now)
	if msg.Stuck == nil || *msg.Stuck != 0 {
		t.Fatalf("two samples is a verdict of 0: %v", msg.Stuck)
	}

	// A registry whose checks have never run has nothing to report either.
	reg := supervisor.New(func() time.Time { return now })
	if err := reg.Register(supervisor.Check{Name: "c", Every: time.Second, Fn: func(context.Context) error { return nil }}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if msg := fleetHealthSnapshot(k, 0, st, reg, now); msg.FailingChecks != nil {
		t.Fatalf("a check that never ran is not a passing check: %d", *msg.FailingChecks)
	}
}

// TestFleetHealthSnapshotCountsFailingAndWedgedChecks covers the two check
// states that are NOT the same thing: a check whose last run failed, and a
// check that never finished a run at all. Failing() reports on outcomes, so it
// cannot see the second one — RunningSince is the only evidence a hung check
// leaves behind.
func TestFleetHealthSnapshotCountsFailingAndWedgedChecks(t *testing.T) {
	fr := runner.NewFake()
	k := kernel.New(fr)
	k.RelaunchProbe = 0
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	reg := supervisor.New(func() time.Time { return now })
	if err := reg.Register(supervisor.Check{Name: "bad", Every: time.Second,
		Fn: func(context.Context) error { return errors.New("boom") }}); err != nil {
		t.Fatalf("register: %v", err)
	}
	hung := make(chan struct{})
	if err := reg.Register(supervisor.Check{Name: "hung", Every: time.Second,
		Fn: func(context.Context) error { <-hung; return nil }}); err != nil {
		t.Fatalf("register: %v", err)
	}

	done := make(chan struct{})
	go func() { reg.RunDue(context.Background(), now.Add(2*time.Second)); close(done) }()

	// Wait for the failing check to have been recorded (the hung one holds
	// RunDue open, so we cannot simply wait for the batch).
	deadline := time.Now().Add(5 * time.Second)
	for reg.Failing() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("failing check never recorded")
		}
		time.Sleep(time.Millisecond)
	}

	// Well past the hung check's tenth interval.
	msg := fleetHealthSnapshot(k, 0, nil, reg, now.Add(2*time.Second+time.Minute))
	if msg.FailingChecks == nil || *msg.FailingChecks != 1 {
		t.Fatalf("failing checks: want 1, got %v", msg.FailingChecks)
	}
	if msg.WedgedChecks == nil || *msg.WedgedChecks != 1 {
		t.Fatalf("wedged checks: want 1, got %v", msg.WedgedChecks)
	}

	// A check merely mid-run is NOT wedged: the registry explicitly permits a
	// check to outrun its own interval.
	if msg := fleetHealthSnapshot(k, 0, nil, reg, now.Add(3*time.Second)); msg.WedgedChecks == nil || *msg.WedgedChecks != 0 {
		t.Fatalf("a check inside its grace must not count as wedged: %v", msg.WedgedChecks)
	}

	close(hung)
	<-done
}

// TestFleetHealthSnapshotCountsOverContext checks the gauge is read, not
// invented: the count appears only once the context pump has measured a size.
func TestFleetHealthSnapshotCountsOverContext(t *testing.T) {
	k, _, _, a, _ := newStuckFixture(t)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	if msg := fleetHealthSnapshot(k, 0, nil, nil, now); msg.OverContext != nil {
		t.Fatalf("unmeasured gauge must stay absent: %d", *msg.OverContext)
	}
	k.Cost.Set(a, costmeter.FContextTokens, transcript.ContextNudgeTokens)
	msg := fleetHealthSnapshot(k, 0, nil, nil, now)
	if msg.OverContext == nil || *msg.OverContext != 1 {
		t.Fatalf("bubble at the nudge threshold must count: %v", msg.OverContext)
	}
}
