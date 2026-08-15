package costmeter

import (
	"testing"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

// TestFieldOrdinalsAreStable pins every Field's numeric value. The constants
// are an iota block and Counters is persisted, so renumbering one silently
// re-labels every counter recorded before the change: a fleet's trim history
// would come back as somebody else's notice count. New fields are APPENDED,
// never inserted.
func TestFieldOrdinalsAreStable(t *testing.T) {
	want := []struct {
		f    Field
		n    int
		name string
	}{
		{FNoticesWritten, 0, "FNoticesWritten"},
		{FNoticesSuppressed, 1, "FNoticesSuppressed"},
		{FNoticesCapped, 2, "FNoticesCapped"},
		{FDeliveriesInline, 3, "FDeliveriesInline"},
		{FDeliveriesViaTool, 4, "FDeliveriesViaTool"},
		{FTurnsTriggered, 5, "FTurnsTriggered"},
		{FEvictions, 6, "FEvictions"},
		{FRewarms, 7, "FRewarms"},
		{FContextTokens, 8, "FContextTokens"},
		{FOversizedTranscripts, 9, "FOversizedTranscripts"},
		{FRelaunchesSuppressed, 10, "FRelaunchesSuppressed"},
		{FCompactsExpired, 11, "FCompactsExpired"},
		{FCompactsDropped, 12, "FCompactsDropped"},
		{FCompactsRetried, 13, "FCompactsRetried"},
		{FCompactsAbandoned, 14, "FCompactsAbandoned"},
		{FCompactsAccepted, 15, "FCompactsAccepted"},
		{FTranscriptsTrimmed, 16, "FTranscriptsTrimmed"},
		{FTranscriptBytesArchived, 17, "FTranscriptBytesArchived"},
		{FTrimsRefused, 18, "FTrimsRefused"},
	}
	for _, w := range want {
		if int(w.f) != w.n {
			t.Errorf("%s = %d, want %d — F* constants are appended, never renumbered", w.name, int(w.f), w.n)
		}
	}
}

// TestTrimCountersAccumulate: the three new counters must be wired into the
// field() switch, or every Add against them is a silent no-op.
func TestTrimCountersAccumulate(t *testing.T) {
	m := New()
	m.Add("0.1", FTranscriptsTrimmed, 1)
	m.Add("0.1", FTranscriptsTrimmed, 1)
	m.Add("0.1", FTranscriptBytesArchived, 4096)
	m.Add("0.1", FTrimsRefused, 3)

	c := m.Snapshot()[addr.Address("0.1")]
	if c.TranscriptsTrimmed != 2 {
		t.Errorf("TranscriptsTrimmed = %d, want 2", c.TranscriptsTrimmed)
	}
	if c.TranscriptBytesArchived != 4096 {
		t.Errorf("TranscriptBytesArchived = %d, want 4096", c.TranscriptBytesArchived)
	}
	if c.TrimsRefused != 3 {
		t.Errorf("TrimsRefused = %d, want 3", c.TrimsRefused)
	}
}
