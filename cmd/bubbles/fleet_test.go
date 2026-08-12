package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/inbox"
	"github.com/Sentinal-Glimpass/bubbles/internal/kernel"
	"github.com/Sentinal-Glimpass/bubbles/internal/notify"
	"github.com/Sentinal-Glimpass/bubbles/internal/registry"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
	"github.com/Sentinal-Glimpass/bubbles/internal/tasks"
)

func TestFleetSaveRestore(t *testing.T) {
	base := t.TempDir()

	// Build a fleet: two bubbles, introduced, with a number-slot.
	k1 := kernel.New(runner.NewFake())
	a1, _ := k1.Spawn(addr.Root, "alice", filepath.Join(base, "alice"), runner.SpawnOpts{Persona: "alice"})
	a2, _ := k1.Spawn(addr.Root, "bob", filepath.Join(base, "bob"), runner.SpawnOpts{Persona: "bob"})
	_ = k1.Introduce(addr.Root, a1, a2)
	k1.EnsureAlive(a1)       // launch a1 so it has a session id (lazy: spawn alone assigns none)
	k1.SetEnabled(a2, false) // disable bob; must survive the round-trip
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
	if b, ok := k2.Reg.Get(a2); !ok || b.Persona != "bob" || !b.Disabled {
		t.Fatalf("%s not restored (disabled should persist): %+v", a2, b)
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

func TestFleetPersistsMuteRules(t *testing.T) {
	base := t.TempDir()
	k1 := kernel.New(runner.NewFake())
	a, _ := k1.Spawn(addr.Root, "w", filepath.Join(base, "w"), runner.SpawnOpts{Persona: "w"})
	k1.Reg.SetMuteRules(a, []notify.Rule{{ID: "r1", Source: "pump", SubjectRe: "^opt_out$", Window: time.Hour}})
	if err := saveFleet(base, k1, map[int]addr.Address{}); err != nil {
		t.Fatal(err)
	}

	k2 := kernel.New(runner.NewFake())
	restoreFleet(base, k2)
	got := k2.Reg.MuteRules(a)
	if len(got) != 1 || got[0].SubjectRe != "^opt_out$" {
		t.Fatalf("restored rules = %+v", got)
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

func TestRestoreAppliesParentContacts(t *testing.T) {
	base := t.TempDir()
	// An OLD manifest: parent 0.1 with child 0.1.1, but the parent's saved
	// contacts do NOT include the child (the rule didn't exist at save time).
	m := manifest{Bubbles: []bubbleRec{
		{Addr: "0.1", Persona: "p", Dir: filepath.Join(base, "p"), Parent: "0"},
		{Addr: "0.1.1", Persona: "c", Dir: filepath.Join(base, "c"), Parent: "0.1"},
	}}
	data, _ := json.MarshalIndent(m, "", "  ")
	_ = os.MkdirAll(filepath.Dir(fleetPath(base)), 0o755)
	if err := os.WriteFile(fleetPath(base), data, 0o644); err != nil {
		t.Fatal(err)
	}

	k := kernel.New(runner.NewFake())
	restoreFleet(base, k)
	if !k.Caps.CanSend(addr.Address("0.1"), addr.Address("0.1.1")) {
		t.Fatal("restore should re-apply parent 0.1 -> child 0.1.1 contact")
	}
	if k.Caps.CanSend(addr.Address("0.1.1"), addr.Address("0.1")) {
		t.Fatal("child should still NOT reach parent")
	}
}

func TestFleetGroupsRoundTrip(t *testing.T) {
	base := t.TempDir()
	k1 := kernel.New(runner.NewFake())
	a, _ := k1.Spawn(addr.Root, "a", filepath.Join(base, "a"), runner.SpawnOpts{Persona: "a"})
	b, _ := k1.Spawn(addr.Root, "b", filepath.Join(base, "b"), runner.SpawnOpts{Persona: "b"})
	k1.CreateGroup("team", []addr.Address{a, b}, false)
	if err := saveFleet(base, k1, map[int]addr.Address{}); err != nil {
		t.Fatal(err)
	}

	k2 := kernel.New(runner.NewFake())
	restoreFleet(base, k2)
	if g, ok := k2.Groups.Get("team"); !ok || len(g.Members) != 2 {
		t.Fatalf("group not restored: %+v ok=%v", g, ok)
	}
}

func TestRestoreNoFile(t *testing.T) {
	// No saved fleet -> empty marks, no panic.
	k := kernel.New(runner.NewFake())
	if m := restoreFleet(t.TempDir(), k); len(m) != 0 {
		t.Fatalf("expected empty marks, got %+v", m)
	}
}

func TestInboxPersistRoundTrip(t *testing.T) {
	base := t.TempDir()

	k1 := kernel.New(runner.NewFake())
	k1.RelaunchProbe = 0
	a, _ := k1.Spawn(addr.Root, "a", filepath.Join(base, "a"), runner.SpawnOpts{Name: "a"})
	b, _ := k1.Spawn(addr.Root, "b", filepath.Join(base, "b"), runner.SpawnOpts{Name: "b"})
	k1.Introduce(addr.Root, a, b)
	id := k1.Store.Append(inboxMsg(a, b, "hello", "world", 0))
	k1.Store.Append(inboxMsg(b, a, "re", "hi", id)) // a reply, so reply_to must resolve after restore

	if err := saveInbox(base, k1); err != nil {
		t.Fatalf("saveInbox: %v", err)
	}

	// fresh process restores the mail
	k2 := kernel.New(runner.NewFake())
	existed, ok := loadInbox(base, k2)
	if !existed || !ok {
		t.Fatalf("loadInbox existed=%v ok=%v", existed, ok)
	}
	if k2.Store.UnreadCount(b) != 1 {
		t.Fatalf("unread mail should survive restart, got %d", k2.Store.UnreadCount(b))
	}
	// the ID sequence continues so a new message doesn't collide
	next := k2.Store.Append(inboxMsg(a, b, "again", "", 0))
	if next <= id+1 {
		t.Fatalf("id sequence should continue past restored ids (got %d, had %d)", next, id+1)
	}
}

func inboxMsg(from, to addr.Address, subj, body string, replyTo int) inbox.Message {
	return inbox.Message{From: from, To: to, Subject: subj, Body: body, ReplyTo: replyTo}
}

func TestTasksPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	k := kernel.New(runner.NewFake())
	k.Tasks.Create(tasks.Task{Assigner: "0", Worker: "0.1", Brief: "fix adder", Checklist: []string{"works"}})
	if err := saveTasks(dir, k); err != nil {
		t.Fatal(err)
	}
	k2 := kernel.New(runner.NewFake())
	loadTasks(dir, k2)
	got, ok := k2.Tasks.Get("t1")
	if !ok || got.Brief != "fix adder" || got.State != tasks.Open {
		t.Fatalf("restored task = %+v ok=%v", got, ok)
	}
	// The ID sequence continues after restore.
	if n := k2.Tasks.Create(tasks.Task{Worker: "0.2"}); n.ID != "t2" {
		t.Fatalf("sequence after restore: %s", n.ID)
	}
}

// TestSaveFleetTornWriteKeepsPreviousFleet is the durability gate that Task 2
// made load-bearing. saveFleet used to be a bare os.WriteFile: it truncated
// fleet.json in place, so a write that failed partway (disk full) left invalid
// JSON on disk, and loadFleet drops the ENTIRE fleet on an unmarshal failure.
// Before Task 2 that was a lost update; now that a returned spawn address means
// "durably recorded", it would be a fleet-destroying event. The write must be
// atomic: the real file is only ever replaced by a complete one.
func TestSaveFleetTornWriteKeepsPreviousFleet(t *testing.T) {
	base := t.TempDir()

	k := kernel.New(runner.NewFake())
	a1, _ := k.Spawn(addr.Root, "alice", filepath.Join(base, "alice"), runner.SpawnOpts{Persona: "alice"})
	a2, _ := k.Spawn(addr.Root, "bob", filepath.Join(base, "bob"), runner.SpawnOpts{Persona: "bob"})
	_ = k.Introduce(addr.Root, a1, a2)
	if err := saveFleet(base, k, nil); err != nil {
		t.Fatalf("initial save: %v", err)
	}
	good, err := os.ReadFile(fleetPath(base))
	if err != nil {
		t.Fatalf("read initial fleet: %v", err)
	}

	// Simulate the disk filling up mid-write: the bytes that fit are stored, then
	// ENOSPC. Restored after the test so later cases are unaffected.
	real := syncedWrite
	defer func() { syncedWrite = real }()
	boom := errors.New("no space left on device")
	syncedWrite = func(path string, data []byte, perm os.FileMode) error {
		_ = real(path, data[:len(data)/2], perm) // short write: half the bytes land
		return boom
	}

	k.Spawn(addr.Root, "carol", filepath.Join(base, "carol"), runner.SpawnOpts{Persona: "carol"})
	if err := saveFleet(base, k, nil); !errors.Is(err, boom) {
		t.Fatalf("saveFleet should surface the write error, got %v", err)
	}

	// The PREVIOUS fleet must still be on disk, byte for byte, and loadable.
	after, err := os.ReadFile(fleetPath(base))
	if err != nil {
		t.Fatalf("fleet.json is gone after a failed save: %v", err)
	}
	if !bytes.Equal(after, good) {
		t.Fatalf("a failed save corrupted fleet.json:\nwant %d bytes\ngot  %d bytes: %s", len(good), len(after), after)
	}
	m, ok := loadFleet(base)
	if !ok {
		t.Fatal("fleet.json no longer loads after a failed save — the whole fleet would be dropped on next start")
	}
	if len(m.Bubbles) != 2 {
		t.Fatalf("restored fleet has %d bubbles, want the 2 from before the failed save", len(m.Bubbles))
	}
	assertNoTempFiles(t, base)
}

// TestSaveFleetSuccessIsUnchanged: the atomic path must write exactly the bytes
// the direct write wrote, with the same mode, and leave no temp file behind.
func TestSaveFleetSuccessIsUnchanged(t *testing.T) {
	base := t.TempDir()
	k := kernel.New(runner.NewFake())
	a1, _ := k.Spawn(addr.Root, "alice", filepath.Join(base, "alice"), runner.SpawnOpts{Persona: "alice"})
	k.EnsureAlive(a1)

	// Capture the serialized manifest as handed to the writer, so this asserts
	// the file content without duplicating saveFleet's serializer.
	real := syncedWrite
	defer func() { syncedWrite = real }()
	var handed []byte
	syncedWrite = func(path string, data []byte, perm os.FileMode) error {
		handed = append([]byte(nil), data...)
		return real(path, data, perm)
	}
	if err := saveFleet(base, k, map[int]addr.Address{2: a1}); err != nil {
		t.Fatalf("save: %v", err)
	}
	on, err := os.ReadFile(fleetPath(base))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(on, handed) {
		t.Fatalf("fleet.json is not the serialized manifest:\nwant %s\ngot  %s", handed, on)
	}
	st, err := os.Stat(fleetPath(base))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Fatalf("fleet.json mode = %v want 0644", st.Mode().Perm())
	}
	assertNoTempFiles(t, base)
	if _, ok := loadFleet(base); !ok {
		t.Fatal("saved fleet does not load back")
	}
}

// assertNoTempFiles fails if the metadata dir holds anything that is not one of
// the known manifests — a leaked scratch file from an atomic write.
func assertNoTempFiles(t *testing.T, base string) {
	t.Helper()
	ents, err := os.ReadDir(filepath.Dir(fleetPath(base)))
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{"fleet.json": true, "inbox.json": true, "tasks.json": true, ".gitignore": true}
	for _, e := range ents {
		if !known[e.Name()] {
			t.Fatalf("leftover temp file in %s: %s", filepath.Dir(fleetPath(base)), e.Name())
		}
	}
}
