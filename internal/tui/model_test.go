package tui

import (
	"bytes"
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
