package tui

import (
	"strings"
	"testing"
)

func TestOverlayStacksWhenNarrow(t *testing.T) {
	body := "aardvark-a-very-long-tree-row-that-eats-the-width\nsecond"
	panel := []string{"USAGE 1234", "cpu 99%"}
	// Wide: panel sits to the RIGHT of the first body line.
	wide := overlayTopRight(body, panel, 120)
	if strings.HasPrefix(wide, "USAGE") {
		t.Fatal("wide terminal should overlay, not stack")
	}
	first := strings.SplitN(wide, "\n", 2)[0]
	if !strings.Contains(first, "aardvark") || !strings.Contains(first, "USAGE") {
		t.Fatalf("panel not overlaid on first row: %q", first)
	}
	// Narrow: no room side-by-side, so the panel stacks ON TOP of the body.
	narrow := overlayTopRight(body, panel, 40)
	if !strings.HasPrefix(narrow, "USAGE 1234") {
		t.Fatalf("narrow terminal should stack panel on top, got: %q", narrow)
	}
	if !strings.Contains(narrow, "aardvark") {
		t.Fatal("body dropped when stacking")
	}
}
