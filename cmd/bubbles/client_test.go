package main

import (
	"bytes"
	"testing"
)

// TestChordStep: a bare Ctrl-] never stops; Ctrl-\ then Ctrl-] stops; Ctrl-\
// then anything else forwards BOTH bytes so the in-bubble Ctrl-\ leader survives.
func TestChordStep(t *testing.T) {
	// bare Ctrl-] forwards, does not stop
	fwd, stop, armed := chordStep(false, clientStopByte)
	if stop || armed || !bytes.Equal(fwd, []byte{clientStopByte}) {
		t.Fatalf("bare Ctrl-] should just forward: fwd=%v stop=%v armed=%v", fwd, stop, armed)
	}
	// Ctrl-\ arms, forwards nothing yet
	fwd, stop, armed = chordStep(false, clientLeaderByte)
	if stop || !armed || fwd != nil {
		t.Fatalf("Ctrl-\\ should arm silently: fwd=%v stop=%v armed=%v", fwd, stop, armed)
	}
	// armed + Ctrl-] -> STOP
	_, stop, _ = chordStep(true, clientStopByte)
	if !stop {
		t.Fatal("Ctrl-\\ then Ctrl-] should stop the fleet")
	}
	// armed + Ctrl-\ (a second leader) -> forward both, disarm (in-bubble pop-to-fleet)
	fwd, stop, armed = chordStep(true, clientLeaderByte)
	if stop || armed || !bytes.Equal(fwd, []byte{clientLeaderByte, clientLeaderByte}) {
		t.Fatalf("Ctrl-\\ Ctrl-\\ should forward both: fwd=%v", fwd)
	}
	// armed + a normal key -> forward leader + key, disarm
	fwd, stop, armed = chordStep(true, 'x')
	if stop || armed || !bytes.Equal(fwd, []byte{clientLeaderByte, 'x'}) {
		t.Fatalf("Ctrl-\\ x should forward both: fwd=%v", fwd)
	}
}
