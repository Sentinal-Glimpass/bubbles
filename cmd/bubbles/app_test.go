package main

import (
	"bytes"
	"strings"
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

func TestDiveStatusFooter(t *testing.T) {
	var buf bytes.Buffer
	d := &diveStatus{out: &buf, label: " ● 0.6  cmo — back", cols: 80, rows: 24}
	// A claude output chunk goes through and triggers a (forced-first) footer paint.
	if _, err := d.Write([]byte("hello from claude")); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "hello from claude") {
		t.Fatal("claude output not passed through")
	}
	if !strings.Contains(out, "\x1b[1;23r") { // scroll region reserves rows 1..23
		t.Fatalf("no scroll-region reservation in output: %q", out)
	}
	if !strings.Contains(out, "\x1b[24;1H") { // footer painted on row 24
		t.Fatalf("footer not painted on the reserved row: %q", out)
	}
	if !strings.Contains(out, "0.6  cmo") {
		t.Fatalf("footer missing the bubble label: %q", out)
	}
}

func TestDiveStatusTruncatesAndSkipsTinyTerminals(t *testing.T) {
	var buf bytes.Buffer
	d := &diveStatus{out: &buf, label: strings.Repeat("x", 200), cols: 10, rows: 24}
	d.paintLocked(true)
	// Footer text is clipped to the column width (10 runes), never wider.
	if strings.Count(buf.String(), "x") != 10 {
		t.Fatalf("footer not truncated to cols: %q", buf.String())
	}
	// A 2-row terminal is too short to reserve a footer: paint is a no-op.
	buf.Reset()
	tiny := &diveStatus{out: &buf, label: "hi", cols: 80, rows: 2}
	tiny.paintLocked(true)
	if buf.Len() != 0 {
		t.Fatalf("tiny terminal should not paint a footer: %q", buf.String())
	}
}
