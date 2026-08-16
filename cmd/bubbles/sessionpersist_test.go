package main

import (
	"path/filepath"
	"testing"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/kernel"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
)

// persistDriver reproduces the daemon's change-driven autosave exactly as
// internal/tui/model.go runs it: on each tick, compare the registry version
// against the one captured at the last save, and only if it MOVED run
// OnPersist (SyncSessionIDs, then saveFleet) and re-capture. Everything about
// the bug and about the loop the fix must not create lives in that ordering.
type persistDriver struct {
	k       *kernel.Kernel
	base    string
	marks   map[int]addr.Address
	lastVer int64
	saves   int
}

func newPersistDriver(k *kernel.Kernel, base string) *persistDriver {
	return &persistDriver{k: k, base: base, marks: map[int]addr.Address{}, lastVer: k.Reg.Version()}
}

func (d *persistDriver) tick(t *testing.T) {
	t.Helper()
	if v := d.k.Reg.Version(); v != d.lastVer {
		d.lastVer = v
		d.k.SyncSessionIDs() // OnPersist's first act, immediately before the save
		if err := saveFleet(d.base, d.k, d.marks); err != nil {
			t.Fatalf("saveFleet: %v", err)
		}
		d.saves++
	}
}

// TestLaunchSessionIDReachesDiskOnItsOwn is the incident, reduced: a bubble
// launches, mints a session id, and then goes cold — with NO other fleet change
// to piggyback on. The id must still be on disk, because the launch itself
// marked the fleet dirty. Before the fix the launch left the version alone, the
// autosave never fired, and the next start resumed the OLD id from fleet.json
// while the real conversation was orphaned.
func TestLaunchSessionIDReachesDiskOnItsOwn(t *testing.T) {
	base := t.TempDir()
	k1 := kernel.New(runner.NewFake())
	k1.RelaunchProbe = 0
	a, err := k1.Spawn(addr.Root, "w", filepath.Join(base, "w"), runner.SpawnOpts{Persona: "w"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	// The fleet as the daemon has it just after boot: saved once, nothing dirty.
	d := newPersistDriver(k1, base)
	if err := saveFleet(base, k1, d.marks); err != nil {
		t.Fatalf("initial save: %v", err)
	}
	d.lastVer = k1.Reg.Version()

	k1.EnsureAlive(a) // first-ever launch: mints and records a fresh session id
	sid, _ := k1.Reg.SessionID(a)
	if sid == "" {
		t.Fatal("launch did not assign a session id")
	}
	// The bubble is paged out again, so the live-id hook can no longer see it.
	// This is precisely the state in which the id used to be lost: SyncSessionIDs
	// reads "" and has nothing to contribute at the next save.
	k1.CurrentSessionID = func(addr.Address) string { return "" }

	d.tick(t)
	if d.saves != 1 {
		t.Fatalf("saves = %d after a launch-path session id changed, want 1 — the launch must mark the fleet dirty", d.saves)
	}

	// Fresh process: what a restart actually resumes.
	k2 := kernel.New(runner.NewFake())
	restoreFleet(base, k2)
	got, _ := k2.Reg.SessionID(a)
	if got != sid {
		t.Fatalf("restored session id = %q want %q — the new conversation did not survive the round trip", got, sid)
	}
}

// TestPersistLoopDoesNotSelfTrigger is the anti-loop guard, and it is the exact
// reason the two entry points may not be collapsed into one.
//
// SyncSessionIDs runs INSIDE OnPersist, after the persist loop has captured the
// version it is saving at. If it marked the fleet dirty, the next tick would see
// a change, save, dirty it again — forever. So: a bare SyncSessionIDs must not
// move the version, and after the one save the launch legitimately earned, a
// long run of ticks must produce no further saves at all, even with a live hook
// reporting an id that differs from the stored one on every sweep.
func TestPersistLoopDoesNotSelfTrigger(t *testing.T) {
	base := t.TempDir()
	k := kernel.New(runner.NewFake())
	k.RelaunchProbe = 0
	a, err := k.Spawn(addr.Root, "w", filepath.Join(base, "w"), runner.SpawnOpts{Persona: "w"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	d := newPersistDriver(k, base)
	if err := saveFleet(base, k, d.marks); err != nil {
		t.Fatalf("initial save: %v", err)
	}
	d.lastVer = k.Reg.Version()

	k.EnsureAlive(a) // one genuine, dirty-marking change
	// A hot bubble that has since /resume'd onto a different conversation: every
	// SyncSessionIDs from here on writes a value that differs from the stored one.
	k.CurrentSessionID = func(addr.Address) string { return "live-abc" }

	// A bare sync must not move the version.
	v := k.Reg.Version()
	k.SyncSessionIDs()
	if got := k.Reg.Version(); got != v {
		t.Fatalf("version moved %d -> %d across SyncSessionIDs; the pre-save sweep must never dirty the fleet", v, got)
	}
	if sid, _ := k.Reg.SessionID(a); sid != "live-abc" {
		t.Fatalf("SyncSessionIDs stored %q want live-abc — it must still freshen the value", sid)
	}

	// save -> tick -> save: exactly one save (the one the launch owes), then
	// quiet, for as long as we care to look. d.lastVer is still the boot value.
	for i := 0; i < 200; i++ {
		d.tick(t)
	}
	if d.saves != 1 {
		t.Fatalf("saves = %d across 200 ticks, want exactly 1 — one launch-path change owes one save, and the save must not schedule the next", d.saves)
	}
}
