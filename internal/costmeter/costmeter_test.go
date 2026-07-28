package costmeter

import (
	"testing"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

func TestAddAndSnapshot(t *testing.T) {
	m := New()
	m.Add("0.1", FNoticesWritten, 1)
	m.Add("0.1", FNoticesWritten, 2)
	m.Add("0.1", FNoticesSuppressed, 5)
	m.Add("0.2", FDeliveriesInline, 1)

	snap := m.Snapshot()
	if got := snap[addr.Address("0.1")].NoticesWritten; got != 3 {
		t.Fatalf("0.1 NoticesWritten = %d, want 3", got)
	}
	if got := snap[addr.Address("0.1")].NoticesSuppressed; got != 5 {
		t.Fatalf("0.1 NoticesSuppressed = %d, want 5", got)
	}
	if got := snap[addr.Address("0.2")].DeliveriesInline; got != 1 {
		t.Fatalf("0.2 DeliveriesInline = %d, want 1", got)
	}
}

func TestSnapshotIsACopy(t *testing.T) {
	m := New()
	m.Add("0.1", FNoticesWritten, 1)
	snap := m.Snapshot()
	c := snap[addr.Address("0.1")]
	c.NoticesWritten = 99
	if m.Snapshot()[addr.Address("0.1")].NoticesWritten != 1 {
		t.Fatal("Snapshot must return a copy, not live state")
	}
}

func TestVersionBumpsOnChange(t *testing.T) {
	m := New()
	v0 := m.Version()
	m.Add("0.1", FNoticesWritten, 1)
	if m.Version() == v0 {
		t.Fatal("Version must bump on Add")
	}
}

func TestSetContextTokensReplacesNotAccumulates(t *testing.T) {
	m := New()
	m.Set("0.1", FContextTokens, 500_000)
	m.Set("0.1", FContextTokens, 620_000)
	if got := m.Snapshot()[addr.Address("0.1")].ContextTokens; got != 620_000 {
		t.Fatalf("ContextTokens = %d, want 620000 (Set replaces)", got)
	}
}
