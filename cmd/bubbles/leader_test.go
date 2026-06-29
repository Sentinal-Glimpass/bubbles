package main

import (
	"testing"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

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

	// Ctrl-Q then q -> fleet
	s = leaderState{}
	s.feed(leaderByte, cur, nil)
	if r := s.feed('q', cur, nil); !r.fleet {
		t.Fatalf("Ctrl-Q q should go to fleet: %+v", r)
	}

	// Ctrl-Q Ctrl-Q -> literal Ctrl-Q forwarded
	s = leaderState{}
	s.feed(leaderByte, cur, nil)
	if r := s.feed(leaderByte, cur, nil); len(r.forward) != 1 || r.forward[0] != leaderByte {
		t.Fatalf("Ctrl-Q Ctrl-Q should forward literal: %+v", r)
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
