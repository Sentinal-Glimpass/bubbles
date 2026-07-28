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

	// Control. Without this the test would pass just as well if the wake path
	// were broken outright, or if some unrelated gate (INV-2, the ceiling)
	// happened to suppress the event -- which is exactly how this test passed
	// while mute rules were silently matching nothing at all. An urgent event
	// that does NOT match the rule must still wake the same cold bubble, so
	// the only difference between the two cases is the mute veto.
	k.WebhookDeliver(a, "pump", "page_me", "e3", true)
	if !k.IsHot(a) {
		t.Fatal("an UNMUTED urgent webhook must still wake a cold bubble -- otherwise this test proves nothing about mute")
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

// TestInlinedDeliveryDoesNotStallTheNextMessage is the INV-2 shrinkage
// regression. An inlined message leaves the notifiable set the moment it is
// delivered, so recording the PRE-delivery backlog as the announced high-water
// strands that high-water above a backlog that no longer exists — and the next
// genuine message is deduped away against it. The bubble then misses a real
// message until the stale-notice sweep, because an unrelated short message
// happened to arrive just before it.
func TestInlinedDeliveryDoesNotStallTheNextMessage(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	k.EnsureAlive(a)

	// A short body is inlined, and the notice tells the bubble it is up to
	// date — so it has no reason to call inbox().
	k.Send(addr.Root, a, "status", "build green", 0, true)
	if n := strings.Count(fr.Session(a).Written(), "📬"); n != 1 {
		t.Fatalf("precondition: the short message should be inlined once, got %d notices", n)
	}

	// A second, unrelated message must be announced immediately.
	k.Send(addr.Root, a, "deploy failed", "rollback needed", 0, true)
	out := fr.Session(a).Written()
	if n := strings.Count(out, "📬"); n != 2 {
		t.Fatalf("the message after an inlined one must be announced, not deduped against a backlog that no longer exists: %d notices in %q", n, out)
	}
	if !strings.Contains(out, "deploy failed") {
		t.Fatalf("the second message should be the one announced, got %q", out)
	}
}

// TestMuteSuppressionDoesNotStallTheNextMessage: a mute-suppressed message
// also leaves the notifiable set (SetMuted), so the same shrinkage question
// applies. It does not strand the high-water, because a suppressed message is
// never announced and so never raises it — this pins that.
func TestMuteSuppressionDoesNotStallTheNextMessage(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	k.Reg.SetMuteRules(a, []notify.Rule{{ID: "r1", Source: "pump", SubjectRe: "^noise$", Window: time.Hour}})
	k.EnsureAlive(a)

	k.WebhookDeliver(a, "pump", "noise", "first opens the window", true)
	n0 := strings.Count(fr.Session(a).Written(), "📬")
	k.WebhookDeliver(a, "pump", "noise", "swallowed", true) // muted
	if n := strings.Count(fr.Session(a).Written(), "📬"); n != n0 {
		t.Fatalf("a muted message must not notify, got %d notices (was %d)", n, n0)
	}

	// Real mail right after must still get through.
	k.Send(addr.Root, a, "deploy failed", "rollback needed", 0, true)
	out := fr.Session(a).Written()
	if strings.Count(out, "📬") != n0+1 {
		t.Fatalf("real mail after a muted message must be announced, got %q", out)
	}
	if !strings.Contains(out, "deploy failed") {
		t.Fatalf("the real message should be the one announced, got %q", out)
	}
	if k.Store.UnreadCount(a) != 3 {
		t.Fatalf("all three messages must still be filed, got %d", k.Store.UnreadCount(a))
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
