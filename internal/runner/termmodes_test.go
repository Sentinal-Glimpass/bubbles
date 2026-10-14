package runner

import (
	"strings"
	"testing"
)

// TestTermModesReplay: the modes claude switches on at launch (captured from a
// real claude startup) are replayed for a later viewer; ones it switched off
// are not.
func TestTermModesReplay(t *testing.T) {
	var m termModes
	m.feed([]byte("\x1b[?25l\x1b[?2004h\x1b[?1004h\x1b[?2031h hello \x1b[?1049h"))
	m.feed([]byte("\x1b[?1000h\x1b[?1002h\x1b[?1003h\x1b[?1006h\x1b[>1u\x1b[>4;2m"))
	m.feed([]byte("\x1b[?1003l text \x1b[?25h"))
	got := string(m.replay())
	for _, want := range []string{"\x1b[?25h", "\x1b[?1000h", "\x1b[?1002h", "\x1b[?1004h", "\x1b[?1006h", "\x1b[?2004h", "\x1b[?2031h", "\x1b[>1u", "\x1b[>4;2m"} {
		if !strings.Contains(got, want) {
			t.Errorf("replay missing %q: %q", want, got)
		}
	}
	for _, not := range []string{"\x1b[?1003h", "1049"} {
		if strings.Contains(got, not) {
			t.Errorf("replay must not contain %q: %q", not, got)
		}
	}
}

func TestTermModesSplitAndKittyStack(t *testing.T) {
	var m termModes
	// A sequence split across reads is still recognised.
	m.feed([]byte("abc\x1b[?20"))
	m.feed([]byte("04h def"))
	if !m.dec[2004] {
		t.Fatal("a mode sequence split across reads must be tracked")
	}
	m.feed([]byte("\x1b[>3u\x1b[>1u\x1b[<u"))
	if got := string(m.replay()); !strings.Contains(got, "\x1b[>3u") {
		t.Fatalf("kitty pop must restore the previous flags, got %q", got)
	}
	m.feed([]byte("\x1b[<u\x1b[>4m"))
	if got := string(m.replay()); strings.Contains(got, ">3u") || strings.Contains(got, ">4;") {
		t.Fatalf("popped kitty flags / reset modifyOtherKeys must not replay, got %q", got)
	}
	// Bare text and a lone ESC at the end don't break anything.
	m.feed([]byte("plain\x1b"))
	m.feed([]byte("[?1000h"))
	if !m.dec[1000] {
		t.Fatal("ESC at a read boundary must be carried")
	}
}
