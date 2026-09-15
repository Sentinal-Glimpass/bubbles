package main

import (
	"bytes"
	"testing"
)

// TestChordStep: a bare Ctrl-] never stops; a leader then Ctrl-] stops; a leader
// then anything else forwards BOTH bytes so the in-bubble leader survives. Both
// Ctrl-\ and Ctrl-/ act as leaders, and the forwarded byte is the one pressed.
func TestChordStep(t *testing.T) {
	// bare Ctrl-] forwards, does not stop
	fwd, stop, armed := chordStep(0, clientStopByte)
	if stop || armed != 0 || !bytes.Equal(fwd, []byte{clientStopByte}) {
		t.Fatalf("bare Ctrl-] should just forward: fwd=%v stop=%v armed=%v", fwd, stop, armed)
	}
	for _, leader := range []byte{clientLeaderByte, clientLeaderAlt} {
		// leader arms, forwards nothing yet
		fwd, stop, armed = chordStep(0, leader)
		if stop || armed != leader || fwd != nil {
			t.Fatalf("leader %#x should arm silently: fwd=%v stop=%v armed=%#x", leader, fwd, stop, armed)
		}
		// armed + Ctrl-] -> STOP
		if _, stop, _ = chordStep(leader, clientStopByte); !stop {
			t.Fatalf("leader %#x then Ctrl-] should stop the fleet", leader)
		}
		// armed + a normal key -> forward THE PRESSED leader + key, disarm
		fwd, stop, armed = chordStep(leader, 'x')
		if stop || armed != 0 || !bytes.Equal(fwd, []byte{leader, 'x'}) {
			t.Fatalf("leader %#x x should forward both: fwd=%v", leader, fwd)
		}
	}
}

// TestChordChunkKeepsEscapeSequenceContiguous is the regression test for the
// new-bubble folder picker cancelling on arrow keys: an arrow is ESC [ A, and
// forwarding it split let the hosted TUI read the lone ESC as Escape (cancel).
// A whole chunk must forward as one contiguous slice, byte-for-byte.
func TestChordChunkKeepsEscapeSequenceContiguous(t *testing.T) {
	up := []byte{0x1b, '[', 'A'} // arrow up
	fwd, stop, armed := chordChunk(0, up)
	if stop || armed != 0 {
		t.Fatalf("plain arrow must not stop or arm: stop=%v armed=%d", stop, armed)
	}
	if !bytes.Equal(fwd, up) {
		t.Fatalf("arrow sequence must be forwarded whole and unchanged, got %v want %v", fwd, up)
	}
}

// TestChordChunkForwardsMixedBurst: a burst of ordinary bytes (e.g. typed text
// plus an escape sequence) is forwarded verbatim in order.
func TestChordChunkForwardsMixedBurst(t *testing.T) {
	burst := append([]byte("hi"), 0x1b, '[', 'B') // "hi" + arrow down
	fwd, stop, _ := chordChunk(0, burst)
	if stop {
		t.Fatal("no stop chord present")
	}
	if !bytes.Equal(fwd, burst) {
		t.Fatalf("burst must pass through verbatim, got %v want %v", fwd, burst)
	}
}

// TestChordChunkStillDetectsStopChord: the leader+stop chord must still tear
// down, even inside a chunk, and the leader itself must not leak downstream.
func TestChordChunkStillDetectsStopChord(t *testing.T) {
	var leader byte
	for _, b := range []byte{0x1c, 0x1f} { // the possible leaders
		if isClientLeader(b) {
			leader = b
			break
		}
	}
	if leader == 0 {
		t.Skip("no client leader byte defined")
	}
	_, stop, _ := chordChunk(0, []byte{leader, clientStopByte})
	if !stop {
		t.Fatal("leader followed by stop byte in one chunk must stop the fleet")
	}
}

// TestChordChunkArmStateThreads: a leader at the end of one chunk arms the next.
func TestChordChunkArmStateThreads(t *testing.T) {
	var leader byte
	for _, b := range []byte{0x1c, 0x1f} {
		if isClientLeader(b) {
			leader = b
			break
		}
	}
	if leader == 0 {
		t.Skip("no client leader byte defined")
	}
	fwd, stop, armed := chordChunk(0, []byte{leader})
	if stop || len(fwd) != 0 || armed != leader {
		t.Fatalf("a trailing leader must arm the next chunk, not forward: fwd=%v armed=%d", fwd, armed)
	}
	if _, stop, _ := chordChunk(armed, []byte{clientStopByte}); !stop {
		t.Fatal("stop byte in the next chunk after an armed leader must stop")
	}
}
