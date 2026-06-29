package tui

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"
	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/kernel"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
)

func newKernelWith(t *testing.T, personas ...string) *kernel.Kernel {
	t.Helper()
	k := kernel.New(runner.NewFake())
	for _, p := range personas {
		if _, err := k.Spawn(addr.Root, p, t.TempDir(), runner.SpawnOpts{Persona: p}); err != nil {
			t.Fatalf("spawn %s: %v", p, err)
		}
	}
	return k
}

func TestRenderAndPing(t *testing.T) {
	k := newKernelWith(t, "scout", "docs")
	m := New(k)
	m.BaseDir = t.TempDir()

	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(80, 24))

	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(b, []byte("scout")) && bytes.Contains(b, []byte("docs"))
	}, teatest.WithDuration(3*time.Second))

	tm.Send(PingMsg{From: addr.Address("0.1"), Subject: "found 3 bugs"})
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(b, []byte("found 3 bugs"))
	}, teatest.WithDuration(3*time.Second))

	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	tm.WaitFinished(t, teatest.WithFinalTimeout(2*time.Second))
}

func TestEnterRootStartsRoot(t *testing.T) {
	k := newKernelWith(t)
	m := New(k)
	m.BaseDir = t.TempDir()

	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(80, 24))
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter}) // cursor starts on root

	fm := tm.FinalModel(t, teatest.WithFinalTimeout(2*time.Second)).(Model)
	if fm.Selected != addr.Root {
		t.Fatalf("enter on root: Selected = %q want root", fm.Selected)
	}
	if _, ok := k.Reg.Get(addr.Root); !ok {
		t.Fatal("root missing")
	}
	if b, _ := k.Reg.Get(addr.Root); b.SessionID == "" {
		t.Fatal("root session not started on enter")
	}
}

func TestDiveSelectsAndQuits(t *testing.T) {
	k := newKernelWith(t, "scout", "docs")
	m := New(k)
	m.BaseDir = t.TempDir()

	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(80, 24))
	tm.Send(tea.KeyMsg{Type: tea.KeyDown})  // root -> 0.1
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter}) // dive

	fm := tm.FinalModel(t, teatest.WithFinalTimeout(2*time.Second)).(Model)
	if fm.Selected != addr.Address("0.1") {
		t.Fatalf("Selected = %q want 0.1", fm.Selected)
	}
}

func TestIntroduceGroup(t *testing.T) {
	k := newKernelWith(t, "alice", "bob", "carol") // 0.1, 0.2, 0.3
	m := New(k)
	m.BaseDir = t.TempDir()

	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(80, 24))
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'i'}}) // introduce mode
	tm.Send(tea.KeyMsg{Type: tea.KeyDown})                      // -> 0.1
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})                     // add 0.1 ✓
	tm.Send(tea.KeyMsg{Type: tea.KeyDown})                      // -> 0.2
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})                     // add 0.2 ✓
	tm.Send(tea.KeyMsg{Type: tea.KeyDown})                      // -> 0.3
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})                     // add 0.3 ✓
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})                     // again on 0.3 -> finalize

	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	tm.WaitFinished(t, teatest.WithFinalTimeout(2*time.Second))

	// every pair among {0.1, 0.2, 0.3} should now be mutual contacts
	for _, p := range [][2]string{{"0.1", "0.2"}, {"0.2", "0.3"}, {"0.1", "0.3"}} {
		if !k.Caps.CanSend(addr.Address(p[0]), addr.Address(p[1])) ||
			!k.Caps.CanSend(addr.Address(p[1]), addr.Address(p[0])) {
			t.Fatalf("after group introduce, %s and %s should be mutual contacts", p[0], p[1])
		}
	}
}

func TestFolderPickerSelectsSubdir(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	k := newKernelWith(t)
	m := New(k)
	m.BaseDir = base

	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(80, 24))
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	tm.Type("worker")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter}) // persona -> folder list [".", "api/", "+ new"]
	tm.Send(tea.KeyMsg{Type: tea.KeyDown})  // -> "api/"
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter}) // select api

	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(b, []byte("worker"))
	}, teatest.WithDuration(3*time.Second))
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	tm.WaitFinished(t, teatest.WithFinalTimeout(2*time.Second))

	b, ok := k.Reg.Get(addr.Address("0.1"))
	if !ok {
		t.Fatal("bubble 0.1 not spawned")
	}
	if want := filepath.Join(base, "api"); b.Dir != want {
		t.Fatalf("bubble dir = %q want %q", b.Dir, want)
	}
}

func TestTogglePermission(t *testing.T) {
	k := newKernelWith(t)
	allow := true
	m := New(k)
	m.BaseDir = t.TempDir()
	m.AllowAll = &allow

	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(80, 24))
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlP})
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	tm.WaitFinished(t, teatest.WithFinalTimeout(2*time.Second))

	if allow {
		t.Fatal("ctrl+p should have toggled AllowAll to false")
	}
}

func hasAddr(rows []addr.Address, a addr.Address) bool {
	for _, r := range rows {
		if r == a {
			return true
		}
	}
	return false
}

func TestBuildRowsCollapse(t *testing.T) {
	k := newKernelWith(t, "parent") // 0.1
	k.SpawnUnder(addr.Root, addr.Address("0.1"), "child", t.TempDir(), runner.SpawnOpts{Persona: "child"})

	exp := map[addr.Address]bool{addr.Root: true} // 0.1 collapsed
	rows := buildRows(k.Reg, exp)
	if !hasAddr(rows, "0.1") {
		t.Fatal("0.1 should be visible (root expanded)")
	}
	if hasAddr(rows, "0.1.1") {
		t.Fatal("0.1.1 should be hidden while 0.1 is collapsed")
	}
	exp["0.1"] = true // expand 0.1
	if rows = buildRows(k.Reg, exp); !hasAddr(rows, "0.1.1") {
		t.Fatal("0.1.1 should appear once 0.1 is expanded")
	}
}

func TestDescendantCount(t *testing.T) {
	k := newKernelWith(t, "a", "b") // 0.1, 0.2
	k.SpawnUnder(addr.Root, addr.Address("0.1"), "c", t.TempDir(), runner.SpawnOpts{Persona: "c"}) // 0.1.1
	if got := descendantCount(k.Reg, addr.Root); got != 3 {
		t.Fatalf("root descendants = %d want 3", got)
	}
	if got := descendantCount(k.Reg, addr.Address("0.1")); got != 1 {
		t.Fatalf("0.1 descendants = %d want 1", got)
	}
	if got := descendantCount(k.Reg, addr.Address("0.2")); got != 0 {
		t.Fatalf("0.2 descendants = %d want 0", got)
	}
}

func TestCreateGroup(t *testing.T) {
	k := newKernelWith(t, "a", "b") // 0.1, 0.2
	m := New(k)
	m.BaseDir = t.TempDir()

	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(80, 24))
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}}) // group mode
	tm.Send(tea.KeyMsg{Type: tea.KeyDown})                      // -> 0.1
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})                     // add 0.1 ✓
	tm.Send(tea.KeyMsg{Type: tea.KeyDown})                      // -> 0.2
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})                     // add 0.2 ✓
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})                     // again on 0.2 -> name
	tm.Type("team")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})                     // -> options
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'i'}}) // introduce-all ON
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}}) // attach session ON
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})                     // create

	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	tm.WaitFinished(t, teatest.WithFinalTimeout(2*time.Second))

	g, ok := k.Groups.Get("team")
	if !ok || len(g.Members) != 2 || g.Session == "" {
		t.Fatalf("group not created with session: %+v ok=%v", g, ok)
	}
	if !k.Caps.CanSend(addr.Address("0.1"), addr.Address("0.2")) {
		t.Fatal("introduce-all should connect the members")
	}
	if !k.Caps.CanSend(g.Session, addr.Address("0.1")) {
		t.Fatal("group session should reach members")
	}
}

func TestFleetRowsGroupNodes(t *testing.T) {
	k := newKernelWith(t, "a", "b") // 0.1, 0.2
	k.CreateGroup("team", []addr.Address{"0.1", "0.2"}, false)
	m := New(k)

	hdr, members := false, 0
	for _, r := range m.rows {
		if r.header && r.group == "team" {
			hdr = true
		}
		if !r.header && r.group == "team" {
			members++
		}
	}
	if !hdr {
		t.Fatal("group should appear as a header row (sibling of root)")
	}
	if members != 0 {
		t.Fatal("members should be hidden while the group is collapsed")
	}

	m.groupExpanded["team"] = true
	m.rows = m.fleetRows()
	members = 0
	for _, r := range m.rows {
		if !r.header && r.group == "team" {
			members++
		}
	}
	if members != 2 {
		t.Fatalf("expanded group should list 2 members, got %d", members)
	}
}

func TestReassignMark(t *testing.T) {
	k := newKernelWith(t, "a", "b") // 0.1, 0.2
	marks := map[int]addr.Address{}
	m := New(k)
	m.BaseDir = t.TempDir()
	m.Marks = marks

	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(80, 24))
	tm.Send(tea.KeyMsg{Type: tea.KeyDown})                      // -> 0.1
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}}) // slot 1 free -> bind 0.1
	tm.Send(tea.KeyMsg{Type: tea.KeyDown})                      // -> 0.2
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}}) // arm set
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}}) // reassign slot 1 -> 0.2

	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	tm.WaitFinished(t, teatest.WithFinalTimeout(2*time.Second))

	if marks[1] != addr.Address("0.2") {
		t.Fatalf("slot 1 = %q want 0.2 after reassign", marks[1])
	}
}

func TestSpawnUnderSelectedBubble(t *testing.T) {
	k := newKernelWith(t, "parent") // 0.1
	m := New(k)
	m.BaseDir = t.TempDir()

	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(80, 24))
	tm.Send(tea.KeyMsg{Type: tea.KeyDown})                      // root -> 0.1
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}}) // spawn under 0.1
	tm.Type("child")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter}) // persona -> folder picker (rooted at 0.1's dir)
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter}) // pick "." -> spawn

	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(b, []byte("child"))
	}, teatest.WithDuration(3*time.Second))
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	tm.WaitFinished(t, teatest.WithFinalTimeout(2*time.Second))

	b, ok := k.Reg.Get(addr.Address("0.1.1"))
	if !ok {
		t.Fatal("nested bubble 0.1.1 not created")
	}
	if b.Parent != addr.Address("0.1") || b.Persona != "child" {
		t.Fatalf("0.1.1 = %+v want parent 0.1, persona child", b)
	}
}

func TestMarkBindAndJump(t *testing.T) {
	k := newKernelWith(t, "alice", "bob") // 0.1, 0.2
	m := New(k)
	m.BaseDir = t.TempDir()
	marks := map[int]addr.Address{}
	m.Marks = marks

	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(80, 24))
	tm.Send(tea.KeyMsg{Type: tea.KeyDown})                      // root -> 0.1
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}}) // slot 1 free -> bind 0.1
	tm.Send(tea.KeyMsg{Type: tea.KeyDown})                      // 0.1 -> 0.2
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}}) // slot 1 bound -> jump to 0.1 (quits)

	fm := tm.FinalModel(t, teatest.WithFinalTimeout(2*time.Second)).(Model)
	if fm.Selected != addr.Address("0.1") {
		t.Fatalf("digit jump Selected = %q want 0.1", fm.Selected)
	}
	if marks[1] != addr.Address("0.1") {
		t.Fatalf("slot 1 = %q want 0.1 (binding should persist in shared map)", marks[1])
	}
}

func TestSpawnKeyAddsBubble(t *testing.T) {
	k := newKernelWith(t) // start with just root
	m := New(k)
	m.BaseDir = t.TempDir()

	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(80, 24))
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	tm.Type("tester")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter}) // persona -> folder stage
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter}) // blank folder -> spawn in BaseDir/tester

	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(b, []byte("tester"))
	}, teatest.WithDuration(3*time.Second))

	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	tm.WaitFinished(t, teatest.WithFinalTimeout(2*time.Second))

	if _, ok := k.Reg.Get(addr.Address("0.1")); !ok {
		t.Fatal("spawned bubble 0.1 not in registry")
	}
}
