package main

import (
	"testing"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

func TestMarkAction(t *testing.T) {
	cur := addr.Address("0.1")
	marks := map[int]addr.Address{}

	// unbound slot -> bind current, no switch
	if dest := markAction(marks, 3, cur); dest != "" {
		t.Fatalf("binding should not switch, got %q", dest)
	}
	if marks[3] != cur {
		t.Fatalf("slot 3 should bind to %s, got %q", cur, marks[3])
	}
	// bound slot from a different bubble -> switch to it
	if dest := markAction(marks, 3, addr.Address("0.2")); dest != cur {
		t.Fatalf("should switch to %s, got %q", cur, dest)
	}
	// bound to current -> no-op
	if dest := markAction(marks, 3, cur); dest != "" {
		t.Fatalf("own slot should be no-op, got %q", dest)
	}
}
