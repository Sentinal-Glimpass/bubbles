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
