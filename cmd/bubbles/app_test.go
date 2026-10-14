package main

import (
	"bytes"
	"testing"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

func TestMarkActionJumpVsBind(t *testing.T) {
	marks := map[int]addr.Address{3: "0.2"} // slot 3 assigned to another bubble

	// From inside 0.1, pressing leader+3 must JUMP to 0.2, not rebind 0.1.
	if dest := markAction(marks, 3, "0.1"); dest != "0.2" {
		t.Fatalf("assigned slot should jump: got %q, marks=%v", dest, marks)
	}
	if marks[3] != "0.2" {
		t.Fatalf("assigned slot must not be reassigned, marks=%v", marks)
	}
	// A FREE slot binds the current bubble.
	if dest := markAction(marks, 5, "0.1"); dest != "" || marks[5] != "0.1" {
		t.Fatalf("free slot should bind current: dest=%q marks=%v", dest, marks)
	}
	// Pressing the current bubble's own slot stays put.
	if dest := markAction(marks, 5, "0.1"); dest != "" {
		t.Fatalf("own slot should stay, got %q", dest)
	}
}

// TestDiveScan: dive input reaches claude whole — escape sequences and pastes
// are not split or eaten — and only the leader chords are intercepted.
func TestDiveScan(t *testing.T) {
	const L = clientLeaderByte
	arrowsAndPaste := []byte("\x1b[A\x1b[200~pasted text\x1b[201~\x1b[1;5C")
	fwd, cmd, _, armed := diveScan(false, arrowsAndPaste)
	if !bytes.Equal(fwd, arrowsAndPaste) || cmd != diveNone || armed {
		t.Fatalf("plain input must pass through untouched: fwd=%q cmd=%v armed=%v", fwd, cmd, armed)
	}

	fwd, cmd, rest, _ := diveScan(false, []byte{'h', 'i', L, L, 'x'})
	if string(fwd) != "hi" || cmd != diveFleet || string(rest) != "x" {
		t.Fatalf("leader,leader must return to the fleet after forwarding prior keys: fwd=%q cmd=%q rest=%q", fwd, cmd, rest)
	}

	fwd, cmd, rest, _ = diveScan(false, []byte{L, '3', 'z'})
	if len(fwd) != 0 || cmd != '3' || string(rest) != "z" {
		t.Fatalf("leader,digit is a mark command: fwd=%q cmd=%q rest=%q", fwd, cmd, rest)
	}

	fwd, cmd, _, _ = diveScan(false, []byte{L, 'a', 'b'})
	if string(fwd) != "ab" || cmd != diveNone {
		t.Fatalf("leader + other key forwards just the key: fwd=%q cmd=%q", fwd, cmd)
	}

	// A chord split across two reads.
	fwd, cmd, _, armed = diveScan(false, []byte{'q', L})
	if string(fwd) != "q" || cmd != diveNone || !armed {
		t.Fatalf("a trailing leader must stay pending: fwd=%q armed=%v", fwd, armed)
	}
	if _, cmd, _, _ = diveScan(armed, []byte{L}); cmd != diveFleet {
		t.Fatalf("the pending leader must complete the chord on the next read, got %q", cmd)
	}
}
