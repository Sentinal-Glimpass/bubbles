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
