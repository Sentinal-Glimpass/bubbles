package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestOverlayKeepsPanelTopRight(t *testing.T) {
	body := "aardvark-a-very-long-tree-row-that-eats-the-width\nsecond"
	panel := []string{"USAGE 1234", "cpu 99%"}

	// Wide: panel sits to the RIGHT of the (untruncated) first body line.
	wide := overlayTopRight(body, panel, 120)
	first := strings.SplitN(wide, "\n", 2)[0]
	if !strings.Contains(first, "aardvark-a-very-long-tree-row") || !strings.Contains(first, "USAGE") {
		t.Fatalf("panel not overlaid top-right on a wide terminal: %q", first)
	}

	// Narrow: the panel STAYS top-right (never gone, never stacked); the tree row
	// is clipped with an ellipsis so it can't collide with the metrics.
	narrow := overlayTopRight(body, panel, 40)
	nfirst := strings.SplitN(narrow, "\n", 2)[0]
	if strings.HasPrefix(narrow, "USAGE") {
		t.Fatalf("panel should stay top-right, not stack on top: %q", narrow)
	}
	if !strings.Contains(nfirst, "USAGE 1234") {
		t.Fatalf("metrics missing from the narrow overlay: %q", nfirst)
	}
	if !strings.Contains(nfirst, "…") {
		t.Fatalf("long tree row should be clipped with an ellipsis: %q", nfirst)
	}
	if w := lipgloss.Width(nfirst); w > 40 {
		t.Fatalf("overlaid row wider than terminal (%d > 40): %q", w, nfirst)
	}
}

func TestFleetHealthRowsOmitUnavailableMetrics(t *testing.T) {
	rows := fleetHealthRows(FleetHealth{Hot: 2, Total: 5, Suppressed: 40, Capped: 0, Inlined: 12, Backlog: 3})
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "40") {
		t.Fatal("suppressed count must be shown")
	}
	if strings.Contains(joined, "stuck") {
		t.Fatal("Phase 4 metrics must be omitted, not rendered as zero")
	}
}

func TestUsagePanelIncludesFleetHealth(t *testing.T) {
	m := Model{}
	m.health = FleetHealth{Hot: 1, Total: 2, Suppressed: 7}
	joined := strings.Join(usagePanel(m), "\n")
	if !strings.Contains(joined, "FLEET") {
		t.Fatal("usagePanel must include the fleet health block")
	}
}
