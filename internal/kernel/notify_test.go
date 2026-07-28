package kernel

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/inbox"
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

// TestConcurrentDeliveriesAnnounceOnce is the double-notification race gate.
// Two sends to the same hot bubble race: if the announced level is READ, then
// Decide runs, and only later is the level MARKED, both callers observe zero
// and both write a notice. -race will never report it -- it is a logic race,
// not a memory race -- so this test is the only thing standing between the
// design and a silent regression to two turns per backlog.
//
// Bodies are longer than InlineMaxBytes on purpose: an inlined message is
// consumed and returns the announced level to 0, which legitimately lets the
// second message announce. The accumulating-backlog case is the one with an
// unambiguous answer, so it is the one asserted here.
func TestConcurrentDeliveriesAnnounceOnce(t *testing.T) {
	for trial := 0; trial < 50; trial++ {
		fr := runner.NewFake()
		k := New(fr)
		k.RelaunchProbe = 0
		a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
		k.EnsureAlive(a)

		long := strings.Repeat("x", notify.InlineMaxBytes+1)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start // barrier: both are in flight before either proceeds
				k.Send(addr.Root, a, fmt.Sprintf("m%d", i), long, 0, true)
			}(i)
		}
		close(start)
		wg.Wait()

		if n := strings.Count(fr.Session(a).Written(), "📬"); n != 1 {
			t.Fatalf("trial %d: two concurrent deliveries produced %d notices, want exactly 1 "+
				"— the announced level must be read, decided and claimed in one critical section", trial, n)
		}
	}
}

// TestDrainCoalescedKeepsBatchWhenSessionIsDead: Policy.Pending CONSUMES the
// coalescing buffer as it returns it, so a liveness check after the call would
// throw a due batch away with no notice and no counter. The messages must stay
// notifiable so the ordinary drain still reports them.
func TestDrainCoalescedKeepsBatchWhenSessionIsDead(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	k.EnsureAlive(a)
	k.Send(addr.Root, a, "first", "opens the window", 0, false)
	k.Send(addr.Root, a, "follow", "batched", 0, false)

	// The session dies while the batch is still buffering.
	fr.Session(a).Die()
	time.Sleep(notify.CoalesceWindow + 200*time.Millisecond)
	k.DrainCoalesced()

	// The bubble comes back; the batch must still be there to announce.
	k.EnsureAlive(a)
	k.DrainCoalesced()
	if !strings.Contains(fr.Session(a).Written(), "📬") {
		t.Fatalf("a batch due while the session was dead was consumed and lost, got %q", fr.Session(a).Written())
	}
}

// TestTypingHoldSpendsNoPolicyState: the typing hold must be evaluated before
// Decide, or the held message burns an INV-1 token and opens a coalescing
// window that no write ever justified.
func TestTypingHoldSpendsNoPolicyState(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	k.TypingWindow = time.Hour
	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	k.EnsureAlive(a)
	k.SetFocus(a)
	k.NoteKeystroke()

	for i := 0; i < 20; i++ { // far more than the INV-1 burst
		k.Send(addr.Root, a, "held", "body", 0, true)
	}
	if w := fr.Session(a).Written(); strings.Contains(w, "📬") {
		t.Fatalf("nothing may be typed while the operator is typing, got %q", w)
	}

	// The operator pauses and the backlog flushes. If the held sends had been
	// run through Decide, the ceiling would already be drained and this would
	// be silently capped.
	k.TypingWindow = time.Nanosecond
	k.FlushHeldIfIdle()
	if !strings.Contains(fr.Session(a).Written(), "unread") {
		t.Fatalf("the held backlog must flush once typing pauses, got %q", fr.Session(a).Written())
	}
	if c := k.Cost.Snapshot()[a]; c.NoticesCapped != 0 {
		t.Fatalf("a held message must not spend an INV-1 token, got %d capped", c.NoticesCapped)
	}
}

// TestPooledMessageSpendsNoPolicyState: same guarantee for the paged-out arm,
// which also never writes. A pooled message that opened a coalescing window
// would make later arrivals buffer against a window the kernel never honoured.
func TestPooledMessageSpendsNoPolicyState(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	k.EnsureAlive(a)    // gives it a SessionID
	fr.Session(a).Die() // now paged out: cold, but previously run

	for i := 0; i < 20; i++ {
		k.Send(addr.Root, a, "pooled", "body", 0, false)
	}
	if c := k.Cost.Snapshot()[a]; c.NoticesCapped != 0 || c.NoticesWritten != 0 {
		t.Fatalf("a pooled message must spend no policy state: written=%d capped=%d", c.NoticesWritten, c.NoticesCapped)
	}
	if k.Store.UnreadCount(a) != 20 {
		t.Fatalf("every pooled message must still be filed, got %d", k.Store.UnreadCount(a))
	}

	// The drain pages it back in and announces the backlog.
	k.DrainInboxes()
	if !strings.Contains(fr.Session(a).Written(), "unread") {
		t.Fatalf("the drain must announce the pooled backlog, got %q", fr.Session(a).Written())
	}
}

// TestDeferredWriteThatNeverLandsDoesNotConsumeTheMessage: an inlined body is
// marked non-notifiable only once it has actually been typed. A session that
// dies while the deferred write is waiting must leave the message intact, or
// the content is in no terminal and no longer announces.
func TestDeferredWriteThatNeverLandsDoesNotConsumeTheMessage(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	k.EnsureAlive(a)
	fr.Session(a).SetInputReady(false) // still booting: the write is deferred

	k.Send(addr.Root, a, "status", "build green", 0, true)
	fr.Session(a).Die() // it dies before ever accepting input
	time.Sleep(500 * time.Millisecond)

	if w := fr.Session(a).Written(); strings.Contains(w, "📬") {
		t.Fatalf("nothing should have been written to a session that never became ready, got %q", w)
	}
	if n := k.Store.NotifiableCount(a); n != 1 {
		t.Fatalf("a message whose write never landed must stay notifiable, got %d", n)
	}
	if c := k.Cost.Snapshot()[a]; c.DeliveriesInline != 0 || c.NoticesWritten != 0 {
		t.Fatalf("a write that never happened must not be counted: written=%d inline=%d", c.NoticesWritten, c.DeliveriesInline)
	}
}

// TestSweepDoesNotWakeForMutedOnlyBacklog is the headline guarantee of this
// phase made true FLEET-WIDE. The send path already refuses to wake a cold
// bubble for muted traffic, but the 45s/10m recovery sweep keyed off
// UnreadCount — which counts muted messages, correctly, since they are unread
// and inbox() still shows them. So a bubble whose entire backlog was
// mute-suppressed webhook events was paged back in by the sweep anyway, paying
// the exact prompt-cache rewarm muting exists to prevent. The sweep must key
// off NotifiableCount.
func TestSweepDoesNotWakeForMutedOnlyBacklog(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	k.Reg.SetMuteRules(a, []notify.Rule{{ID: "r1", Source: "pump", SubjectRe: "^noise$", Window: time.Hour}})

	// The first match opens the mute window and is delivered (inlined, so it
	// spends its notification); everything after it inside the window is
	// swallowed. Then the bubble goes cold.
	k.WebhookDeliver(a, "pump", "noise", "e0", true)
	_ = k.runner.Kill(a)
	k.smu.Lock()
	delete(k.sessions, a)
	k.smu.Unlock()
	for i := 1; i < 4; i++ {
		k.WebhookDeliver(a, "pump", "noise", fmt.Sprintf("e%d", i), true)
	}
	if k.IsHot(a) {
		t.Fatal("precondition: muted events must not have woken it on the send path")
	}
	if n := k.Store.NotifiableCount(a); n != 0 {
		t.Fatalf("precondition: the whole backlog should be non-notifiable, got %d", n)
	}
	if k.Store.UnreadCount(a) != 4 {
		t.Fatalf("precondition: all 4 must be filed and unread, got %d", k.Store.UnreadCount(a))
	}

	k.DrainInboxes() // the full sweep, which pages cold bubbles in

	if k.IsHot(a) {
		t.Fatal("the recovery sweep woke a bubble whose entire backlog is muted -- that is the rewarm cost muting is meant to avoid")
	}
	// No message is dropped and UnreadCount stays truthful: mute suppresses the
	// NOTIFICATION, never the mail.
	if k.Store.UnreadCount(a) != 4 {
		t.Fatalf("muted messages must stay unread and filed, got %d", k.Store.UnreadCount(a))
	}
	if got := k.Inbox(a); len(got) != 4 {
		t.Fatalf("every muted message must still be readable via inbox(), got %d: %v", len(got), got)
	}
	if c := k.Cost.Snapshot()[a]; c.NoticesSuppressed == 0 {
		t.Fatal("every suppression must be metered; NoticesSuppressed is 0")
	}
}

// TestSweepStillWakesForRealBacklog is the control for the test above, and the
// anti-stall direction of the 632fe95 pair: making the sweep notifiable-keyed
// must not make it blind. An UNMUTED cold backlog is still paged in and told.
func TestSweepStillWakesForRealBacklog(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	k.EnsureAlive(a)    // give it a SessionID so the next sends stay pooled
	fr.Session(a).Die() // cold
	k.Send(addr.Root, a, "real", "work to do", 0, false)

	k.DrainInboxes()
	if !k.IsHot(a) {
		t.Fatal("the sweep must still wake a cold bubble that has genuinely notifiable mail")
	}
	if !strings.Contains(fr.Session(a).Written(), "unread") {
		t.Fatalf("it must be told about the backlog, got %q", fr.Session(a).Written())
	}
}

// TestSendAndRecoverDoNotDoubleNotify: the send path and the 45s sweep are one
// decision point. Before unification each had its own — deliverMessage's Decide
// and RecoverUnread's recoverNudge — and the same backlog could be announced by
// both, seen live as two notices for one webhook event.
func TestSendAndRecoverDoNotDoubleNotify(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	k.EnsureAlive(a)

	k.Send(addr.Root, a, "real", strings.Repeat("x", notify.InlineMaxBytes+1), 0, true)
	k.RecoverUnread(true) // the sweep, racing the send path
	k.RecoverUnread(true) // and again: still one backlog, still one notice

	if got := strings.Count(fr.Session(a).Written(), "📬"); got != 1 {
		t.Fatalf("notices = %d, want 1 -- send path and recovery sweep double-notified", got)
	}
}

// TestRecoveryCoversStaleFocus: focus is never unset when a terminal client
// detaches mid-dive, so skipping the focused bubble unconditionally exempted
// the operator's own bubble from recovery for hours (3cfb0d9). The skip must be
// keyed on the operator actually being PRESENT.
func TestRecoveryCoversStaleFocus(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	k.EnsureAlive(a)
	k.SetFocus(a) // dived in, but no keystroke has ever been recorded: stale

	k.Store.Append(inbox.Message{From: addr.Root, To: a, Subject: "pending"})
	k.RecoverUnread(true)
	if !strings.Contains(fr.Session(a).Written(), "unread") {
		t.Fatalf("a focused-but-abandoned bubble must still be recovered, got %q", fr.Session(a).Written())
	}
}

// TestRecoverySkipsTheLiveOperator is the other half: while the operator is
// actually typing, the sweep must not submit a line into their half-written
// prompt.
func TestRecoverySkipsTheLiveOperator(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	k.EnsureAlive(a)
	k.SetFocus(a)
	k.NoteKeystroke() // the operator is present

	k.Store.Append(inbox.Message{From: addr.Root, To: a, Subject: "pending"})
	k.RecoverUnread(true)
	if strings.Contains(fr.Session(a).Written(), "unread") {
		t.Fatalf("the sweep must not type into the bubble the operator is live in, got %q", fr.Session(a).Written())
	}
}

// TestRecoveryCoversFocusAbandonedAfterTyping is the ACTUAL 3cfb0d9 scenario,
// and the only test that exercises the elapsed-window branch of
// operatorPresent. TestRecoveryCoversStaleFocus above uses a focus that never
// saw a keystroke (lastKey == 0), which a naive `return last != 0` would also
// satisfy — so it does not pin the behaviour that commit exists for. The
// production case is the operator who typed and then walked away, or whose
// terminal detached mid-dive: lastKey is non-zero and nothing will ever clear
// the focus, so only the elapsed window keeps recovery alive for that bubble.
func TestRecoveryCoversFocusAbandonedAfterTyping(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	k.EnsureAlive(a)
	k.SetFocus(a)
	// The operator typed — and then left. Nothing unsets focus.
	k.lastKey.Store(time.Now().Add(-5 * time.Minute).UnixNano())
	if k.operatorPresent() {
		t.Fatal("precondition: a keystroke 5m old must not count as present")
	}

	k.Store.Append(inbox.Message{From: addr.Root, To: a, Subject: "pending"})
	k.RecoverUnread(true)
	if !strings.Contains(fr.Session(a).Written(), "unread") {
		t.Fatalf("a focus abandoned after typing must not exempt the bubble from recovery -- "+
			"this is the multi-hour stall of 3cfb0d9; got %q", fr.Session(a).Written())
	}
}

// TestSweepUnclaimsWhenTheBubbleCannotBeReached: the sweep claims the
// announcement BEFORE it writes (that is what makes it race-safe against the
// send path), so a claim that turns out to be unwritable must be given back.
// Otherwise the backlog is recorded as advertised when no notice exists and the
// bubble goes silent until the staleness sweep — the stall direction. This is
// the cold-sweep path specifically, where EnsureAlive returns nil.
func TestSweepUnclaimsWhenTheBubbleCannotBeReached(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	k.EnsureAlive(a) // give it a SessionID so the send stays pooled
	fr.Session(a).Die()
	k.Store.Append(inbox.Message{From: addr.Root, To: a, Subject: "pending"})
	k.SetEnabled(a, false) // now nothing can launch it: EnsureAlive returns nil

	k.DrainInboxes()
	if k.IsHot(a) {
		t.Fatal("precondition: a disabled bubble must not launch")
	}
	if got := k.announced(a); got != 0 {
		t.Fatalf("announced = %d, want 0 -- a claim whose notice never landed must be given back", got)
	}

	// Proof that the give-back matters: once it is reachable again, the very
	// next sweep announces, with no wait for the 2m staleness net.
	k.SetEnabled(a, true)
	k.DrainInboxes()
	if !strings.Contains(fr.Session(a).Written(), "unread") {
		t.Fatalf("the backlog must announce as soon as the bubble is reachable, got %q", fr.Session(a).Written())
	}
}
