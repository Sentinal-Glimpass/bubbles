package main

import (
	"testing"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

func TestParseLeader(t *testing.T) {
	cases := map[string]byte{
		"ctrl-a": 0x01, "ctrl+a": 0x01, "c-a": 0x01, "^a": 0x01,
		"ctrl-g": 0x07, "ctrl-b": 0x02, "ctrl-\\": 0x1c,
		"":      0x01, // default
		"bogus": 0x01, // default
	}
	for in, want := range cases {
		if got := parseLeader(in); got != want {
			t.Errorf("parseLeader(%q) = 0x%02x want 0x%02x", in, got, want)
		}
	}
}

func TestIsCtrlLeft(t *testing.T) {
	for _, s := range []string{"\x1b[1;5D", "\x1b[5D", "\x1bO5D"} {
		if !isCtrlLeft([]byte(s)) {
			t.Errorf("isCtrlLeft(%q) = false want true", s)
		}
	}
	for _, s := range []string{"\x1b[D", "\x1b[1;5C", "\x1b", "\x1b[A", "x"} {
		if isCtrlLeft([]byte(s)) {
			t.Errorf("isCtrlLeft(%q) = true want false", s)
		}
	}
}

func TestLeaderFeed(t *testing.T) {
	cur := addr.Address("0.1")

	// plain byte is forwarded
	var s leaderState
	if r := s.feed('x', cur, map[int]addr.Address{}); string(r.forward) != "x" {
		t.Fatalf("plain byte: %+v", r)
	}

	// Ctrl-\ instantly returns to fleet
	s = leaderState{}
	if r := s.feed(detachByte, cur, nil); !r.fleet {
		t.Fatalf("ctrl-\\ should go to fleet: %+v", r)
	}

	// leader leader -> fleet
	s = leaderState{}
	s.feed(leaderByte, cur, nil)
	if r := s.feed(leaderByte, cur, nil); !r.fleet {
		t.Fatalf("leader leader should go to fleet: %+v", r)
	}

	// Ctrl-Q <unbound digit> -> binds current to that slot, no nav
	marks := map[int]addr.Address{}
	s = leaderState{}
	s.feed(leaderByte, cur, marks)
	if r := s.feed('3', cur, marks); r.fleet || r.switchTo != "" {
		t.Fatalf("binding should not navigate: %+v", r)
	}
	if marks[3] != cur {
		t.Fatalf("slot 3 should bind to %s, got %q", cur, marks[3])
	}

	// Ctrl-Q <bound digit> from a different bubble -> switch to it
	s = leaderState{}
	s.feed(leaderByte, addr.Address("0.2"), marks)
	if r := s.feed('3', addr.Address("0.2"), marks); r.switchTo != cur {
		t.Fatalf("Ctrl-Q 3 should switch to %s, got %+v", cur, r)
	}

	// Ctrl-Q <digit bound to current> -> no-op (already here)
	s = leaderState{}
	s.feed(leaderByte, cur, marks)
	if r := s.feed('3', cur, marks); r.fleet || r.switchTo != "" || len(r.forward) != 0 {
		t.Fatalf("Ctrl-Q on own slot should be a no-op: %+v", r)
	}
}
