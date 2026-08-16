package registry

import (
	"testing"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

// TestRecordSessionIDMarksFleetDirty is the core of the persistence fix: a
// bubble that genuinely acquires (or moves to) a conversation is durable fleet
// state, and the change-driven autosave only writes when the version MOVES. A
// launch-path id that leaves the version alone reaches disk only by luck —
// if some unrelated change happens to bump while the bubble is still hot — and
// otherwise the next launch resumes the superseded id.
func TestRecordSessionIDMarksFleetDirty(t *testing.T) {
	r := New()
	b := r.Add(addr.Root, "worker", "/tmp/w")

	v0 := r.Version()
	if !r.RecordSessionID(b.Addr, "sess-new") {
		t.Fatal("RecordSessionID reported no change on a first-ever id")
	}
	if r.Version() == v0 {
		t.Fatal("RecordSessionID must bump version so fleet.json is re-saved with the new id")
	}
	if sid, ok := r.SessionID(b.Addr); !ok || sid != "sess-new" {
		t.Fatalf("SessionID = %q ok=%v want sess-new", sid, ok)
	}
}

// TestRecordSessionIDSameIDBumpsAtMostOnce: a bump must be a REAL change.
// Re-recording the id a bubble already has must leave the version alone, or a
// no-op refresh re-triggers a save every time it runs.
func TestRecordSessionIDSameIDBumpsAtMostOnce(t *testing.T) {
	r := New()
	b := r.Add(addr.Root, "worker", "/tmp/w")

	v0 := r.Version()
	r.RecordSessionID(b.Addr, "sess-a")
	v1 := r.Version()
	if v1 == v0 {
		t.Fatal("the first record of a new id must bump")
	}
	if r.RecordSessionID(b.Addr, "sess-a") {
		t.Error("re-recording the SAME id reported a change")
	}
	if v2 := r.Version(); v2 != v1 {
		t.Fatalf("version moved %d -> %d on a no-op re-record; a bump must be a real change", v1, v2)
	}
	// ...and a genuine second change still bumps.
	if !r.RecordSessionID(b.Addr, "sess-b") {
		t.Fatal("a real id change reported no change")
	}
	if r.Version() == v1 {
		t.Fatal("a real id change must still bump the version")
	}
}

// TestRecordSessionIDUnknownBubble: a missing address is not a change and must
// not dirty the fleet (an autosave triggered by a write that landed nowhere
// would be a pure loop source).
func TestRecordSessionIDUnknownBubble(t *testing.T) {
	r := New()
	v0 := r.Version()
	if r.RecordSessionID(addr.Address("9.9"), "x") {
		t.Error("RecordSessionID reported a change for a bubble that does not exist")
	}
	if r.Version() != v0 {
		t.Fatal("a write to a missing bubble must not bump the version")
	}
}

// TestSetSessionIDStaysNonDirtying pins the other half of the split from the
// registry's side (the kernel-side guard is TestSetSessionIDDoesNotBumpVersion).
// SyncSessionIDs writes through this one from INSIDE the persist callback, so a
// bump here lands after the persist loop captured its version and makes every
// save schedule another one.
func TestSetSessionIDStaysNonDirtying(t *testing.T) {
	r := New()
	b := r.Add(addr.Root, "worker", "/tmp/w")
	r.RecordSessionID(b.Addr, "sess-a")

	v := r.Version()
	r.SetSessionIDForSave(b.Addr, "sess-live") // a DIFFERENT id, i.e. a real value change
	if got := r.Version(); got != v {
		t.Fatalf("version moved %d -> %d on SetSessionIDForSave; the pre-save setter must never dirty the fleet", v, got)
	}
	if sid, _ := r.SessionID(b.Addr); sid != "sess-live" {
		t.Fatalf("SessionID = %q want sess-live — the value must still land", sid)
	}
}
