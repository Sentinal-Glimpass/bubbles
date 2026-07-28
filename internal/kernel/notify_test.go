package kernel

import (
	"strings"
	"testing"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/notify"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
)

// TestMutedUrgentMessageDoesNotWakeColdBubble is the whole point of the phase:
// a muted webhook must not page a cold bubble back in, because that page-in
// costs a full prompt-cache rewarm. Suppressing only the notice would leave
// the dominant cost in place.
func TestMutedUrgentMessageDoesNotWakeColdBubble(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	k.Reg.SetMuteRules(a, []notify.Rule{{ID: "r1", Source: "pump", SubjectRe: "^opt_out$", Window: time.Hour}})

	// Open the window with one delivery, then let it go cold.
	k.WebhookDeliver(a, "pump", "opt_out", "e1", true)
	_ = k.runner.Kill(a)
	k.smu.Lock()
	delete(k.sessions, a)
	k.smu.Unlock()

	// Second event inside the window must not page the bubble back in.
	k.WebhookDeliver(a, "pump", "opt_out", "e2", true)
	if k.IsHot(a) {
		t.Fatal("muted urgent webhook must NOT wake a cold bubble -- that is the rewarm cost")
	}
	if k.Store.UnreadCount(a) != 2 {
		t.Fatalf("both messages must still be filed, got %d", k.Store.UnreadCount(a))
	}
}

// TestSmallMessageIsInlinedAndNeedsNoInboxCall: a short body rides along with
// the notice, saving the recipient the inbox() round-trip entirely.
func TestSmallMessageIsInlinedAndNeedsNoInboxCall(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	k.EnsureAlive(a)
	k.Send(addr.Root, a, "status", "build green", 0, true)

	out := fr.Session(a).Written()
	if !strings.Contains(out, "build green") {
		t.Fatalf("short body must be inlined into the notice, got %q", out)
	}
	if strings.Contains(out, "call the inbox() tool") {
		t.Fatal("an inlined delivery must not also tell the bubble to call inbox()")
	}
}

// TestDrainCoalescedAnnouncesTheBatch: non-urgent follow-ups inside the
// coalescing window are batched, and the periodic drain is what actually
// announces them. Without the drain a closed batch would stay silent until the
// next message happened to arrive, which on a quiet fleet could be never.
func TestDrainCoalescedAnnouncesTheBatch(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	k.EnsureAlive(a)

	k.Send(addr.Root, a, "first", "opens the window", 0, false)
	k.Send(addr.Root, a, "follow", "batched", 0, false)
	k.Send(addr.Root, a, "follow", "batched", 0, false)
	if n := strings.Count(fr.Session(a).Written(), "📬"); n != 1 {
		t.Fatalf("followers must be coalesced, not announced one by one: %d notices", n)
	}

	// Nothing is due until the window closes.
	k.DrainCoalesced()
	if n := strings.Count(fr.Session(a).Written(), "📬"); n != 1 {
		t.Fatalf("drain must not announce an open window: %d notices", n)
	}

	time.Sleep(notify.CoalesceWindow + 200*time.Millisecond)
	k.DrainCoalesced()
	out := fr.Session(a).Written()
	if n := strings.Count(out, "📬"); n != 2 {
		t.Fatalf("the closed batch should be announced exactly once, got %d notices in %q", n, out)
	}
	if !strings.Contains(out, "follow") {
		t.Fatalf("the drained batch should name the buffered traffic, got %q", out)
	}
}

// TestDrainCoalescedNeverWakesColdBubble: a batch of non-urgent mail is by
// definition not worth a prompt-cache rewarm.
func TestDrainCoalescedNeverWakesColdBubble(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	k.EnsureAlive(a)
	k.Send(addr.Root, a, "first", "opens the window", 0, false)
	k.Send(addr.Root, a, "follow", "batched", 0, false)

	_ = k.runner.Kill(a)
	k.smu.Lock()
	delete(k.sessions, a)
	k.smu.Unlock()

	time.Sleep(notify.CoalesceWindow + 200*time.Millisecond)
	k.DrainCoalesced()
	if k.IsHot(a) {
		t.Fatal("a coalesced batch must never page a cold bubble in")
	}
	if k.Store.UnreadCount(a) != 2 {
		t.Fatalf("both messages must still be filed, got %d", k.Store.UnreadCount(a))
	}
}

// TestNoticeCountNeverExceedsCeiling is the 632fe95 regression gate: whatever
// the send path does, the recipient can never be handed more than the INV-1
// burst of notices.
func TestNoticeCountNeverExceedsCeiling(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	k.EnsureAlive(a)
	for i := 0; i < 178; i++ {
		k.Send(addr.Root, a, "s", "b", 0, true)
	}
	got := strings.Count(fr.Session(a).Written(), "📬")
	if got > notify.DefaultCeilingBurst {
		t.Fatalf("notices = %d, exceeds INV-1 ceiling %d", got, notify.DefaultCeilingBurst)
	}
}
