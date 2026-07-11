package main

import (
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


