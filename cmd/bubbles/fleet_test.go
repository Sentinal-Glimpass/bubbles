package main

import (
	"path/filepath"
	"testing"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/kernel"
	"github.com/Sentinal-Glimpass/bubbles/internal/registry"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
)

func TestFleetSaveRestore(t *testing.T) {
	base := t.TempDir()

	// Build a fleet: two bubbles, introduced, with a number-slot.
	k1 := kernel.New(runner.NewFake())
	a1, _ := k1.Spawn(addr.Root, "alice", filepath.Join(base, "alice"), runner.SpawnOpts{Persona: "alice"})
	a2, _ := k1.Spawn(addr.Root, "bob", filepath.Join(base, "bob"), runner.SpawnOpts{Persona: "bob"})
	_ = k1.Introduce(addr.Root, a1, a2)
	if err := saveFleet(base, k1, map[int]addr.Address{2: a1}); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Fresh process: restore from disk.
	k2 := kernel.New(runner.NewFake())
	marks := restoreFleet(base, k2)

	sid := mustGet(t, k1, a1).SessionID
	if sid == "" {
		t.Fatal("spawn did not assign a session id")
	}
	if b, ok := k2.Reg.Get(a1); !ok || b.Persona != "alice" || b.SessionID != sid {
		t.Fatalf("%s not restored with session id: %+v ok=%v", a1, b, ok)
	}
	if b, ok := k2.Reg.Get(a2); !ok || b.Persona != "bob" {
		t.Fatalf("%s not restored", a2)
	}
	if !k2.Caps.CanSend(a1, a2) || !k2.Caps.CanSend(a2, a1) {
		t.Fatal("contacts not restored")
	}
	if marks[2] != a1 {
		t.Fatalf("marks not restored: %+v", marks)
	}
	// New spawns continue numbering instead of colliding.
	if a3, _ := k2.Spawn(addr.Root, "carol", filepath.Join(base, "carol"), runner.SpawnOpts{Persona: "carol"}); a3 != addr.Address("0.3") {
		t.Fatalf("post-restore spawn = %q want 0.3", a3)
	}
}

func mustGet(t *testing.T, k *kernel.Kernel, a addr.Address) *registry.Bubble {
	t.Helper()
	b, ok := k.Reg.Get(a)
	if !ok {
		t.Fatalf("bubble %s not found", a)
	}
	return b
}

func TestRestoreNoFile(t *testing.T) {
	// No saved fleet -> empty marks, no panic.
	k := kernel.New(runner.NewFake())
	if m := restoreFleet(t.TempDir(), k); len(m) != 0 {
		t.Fatalf("expected empty marks, got %+v", m)
	}
}
