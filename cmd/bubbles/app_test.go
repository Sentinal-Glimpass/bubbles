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



func TestDiveFooterWriteDoesNotPaintInline(t *testing.T) {
	var buf bytes.Buffer
	d := &diveFooter{out: &buf, label: "0.6 · cmo", cols: 80, rows: 24}
	// Write passes claude bytes through and marks dirty — but must NOT paint the
	// footer inline (that per-byte repaint was the rapid-blink bug).
	n, err := d.Write([]byte("claude frame"))
	if err != nil || n != len("claude frame") {
		t.Fatalf("write n=%d err=%v", n, err)
	}
	if buf.String() != "claude frame" {
		t.Fatalf("inline paint leaked into output: %q", buf.String())
	}
	if !d.dirty {
		t.Fatal("write should mark the footer dirty")
	}
}

func TestDiveFooterRefreshPaintsReservedRow(t *testing.T) {
	var buf bytes.Buffer
	d := &diveFooter{out: &buf, label: "0.6 · cmo", cols: 80, rows: 24}
	d.refresh(80, 24)
	out := buf.String()
	if !strings.Contains(out, "\x1b[1;23r") { // reserve rows 1..23
		t.Fatalf("no scroll-region reservation: %q", out)
	}
	if !strings.Contains(out, "\x1b[24;1H") { // footer on row 24
		t.Fatalf("footer not on reserved row: %q", out)
	}
	if !strings.Contains(out, "0.6 · cmo") {
		t.Fatalf("label missing: %q", out)
	}
	if d.dirty {
		t.Fatal("refresh should clear dirty")
	}
}

func TestDiveFooterTruncatesAndSkipsTiny(t *testing.T) {
	var buf bytes.Buffer
	d := &diveFooter{out: &buf, label: strings.Repeat("x", 200), cols: 10, rows: 24}
	d.refresh(10, 24)
	if strings.Count(buf.String(), "x") != 9 { // 1 leading space + 9 x = 10 cols
		t.Fatalf("not truncated to cols: %q", buf.String())
	}
	buf.Reset()
	tiny := &diveFooter{out: &buf, label: "hi", cols: 80, rows: 2}
	tiny.refresh(80, 2)
	if buf.Len() != 0 {
		t.Fatalf("2-row terminal should not paint: %q", buf.String())
	}
}
