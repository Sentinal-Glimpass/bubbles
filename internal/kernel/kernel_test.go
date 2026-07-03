package kernel

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/bus"
	"github.com/Sentinal-Glimpass/bubbles/internal/registry"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
)

func TestFleetEndToEnd(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0 // keep fresh-launch probe out of the test path

	// Root's inbox captures pings.
	var pings []bus.Message
	k.Bus.Subscribe(addr.Root, func(m bus.Message) { pings = append(pings, m) })

	// Spawn two workers under root.
	scout, err := k.Spawn(addr.Root, "scout", "/tmp/scout", runner.SpawnOpts{Persona: "scout"})
	if err != nil {
		t.Fatalf("spawn scout: %v", err)
	}
	if scout != "0.1" {
		t.Fatalf("scout addr = %q want 0.1", scout)
	}
	refactor, err := k.Spawn(addr.Root, "refactor", "/tmp/refactor", runner.SpawnOpts{Persona: "refactor"})
	if err != nil {
		t.Fatalf("spawn refactor: %v", err)
	}

	// Worker -> root: blinks the dashboard (bus) and lands in root's inbox.
	if _, err := k.Send(scout, addr.Root, "found 3 bugs", "details", 0, true); err != nil {
		t.Fatalf("scout->root: %v", err)
	}
	if len(pings) != 1 || pings[0].From != scout {
		t.Fatalf("pings = %+v", pings)
	}

	// Workers can't talk before introduction.
	if _, err := k.Send(scout, refactor, "hi", "", 0, true); !errors.Is(err, ErrNotContact) {
		t.Fatalf("got %v want ErrNotContact", err)
	}
	if err := k.Introduce(addr.Root, scout, refactor); err != nil {
		t.Fatalf("introduce: %v", err)
	}

	// Worker -> worker: lands in the inbox AND queues a non-interrupting notice.
	if _, err := k.Send(scout, refactor, "take the API layer", "thanks", 0, true); err != nil {
		t.Fatalf("scout->refactor: %v", err)
	}
	if w := fr.Session(refactor).Written(); !strings.Contains(w, "📬 New message") ||
		!strings.Contains(w, "(scout)") || !strings.Contains(w, "1 unread") {
		t.Fatalf("expected a 'you have mail' notice, got %q", w)
	}
	// The full message is read via the inbox, not the notice.
	in := k.Inbox(refactor)
	if len(in) != 1 || !strings.Contains(in[0], "from "+scout.String()+" (scout)") ||
		!strings.Contains(in[0], "take the API layer") {
		t.Fatalf("refactor inbox = %v", in)
	}
	if len(k.Inbox(refactor)) != 0 {
		t.Fatal("inbox should be empty after reading")
	}
}

func TestNestedSpawnParentReachesChildren(t *testing.T) {
	k := New(runner.NewFake())
	k.RelaunchProbe = 0 // keep fresh-launch probe out of the test path
	p, _ := k.Spawn(addr.Root, "p", "/tmp/p", runner.SpawnOpts{Persona: "p"}) // 0.1 under root
	c1, _ := k.SpawnUnder(addr.Root, p, "c1", "/tmp/c1", runner.SpawnOpts{Persona: "c1"})
	c2, _ := k.SpawnUnder(addr.Root, p, "c2", "/tmp/c2", runner.SpawnOpts{Persona: "c2"})

	// parent can reach each child...
	if !k.Caps.CanSend(p, c1) || !k.Caps.CanSend(p, c2) {
		t.Fatal("parent should reach its children")
	}
	// ...but not vice versa, and no siblings or ancestors
	if k.Caps.CanSend(c1, p) {
		t.Fatal("child should NOT auto-reach its parent")
	}
	if k.Caps.CanSend(c1, c2) || k.Caps.CanSend(c2, c1) {
		t.Fatal("siblings should NOT be connected")
	}
	// children still reach root
	if !k.Caps.CanSend(c1, addr.Root) || !k.Caps.CanSend(c2, addr.Root) {
		t.Fatal("children should reach root")
	}
}

func TestReplyGrant(t *testing.T) {
	k := New(runner.NewFake())
	k.RelaunchProbe = 0 // keep fresh-launch probe out of the test path
	p, _ := k.Spawn(addr.Root, "p", "/tmp/p", runner.SpawnOpts{Persona: "p"})
	c, _ := k.SpawnUnder(addr.Root, p, "c", "/tmp/c", runner.SpawnOpts{Persona: "c"})

	if k.Caps.CanSend(c, p) {
		t.Fatal("child should not reach parent before being messaged")
	}
	id, err := k.Send(p, c, "do X", "", 0, true) // parent messages child
	if err != nil {
		t.Fatalf("parent->child: %v", err)
	}
	if !k.Caps.CanSend(c, p) {
		t.Fatal("child should be able to reply after the parent messaged it")
	}
	if _, err := k.Send(c, p, "done", "", id, true); err != nil { // child replies (threaded)
		t.Fatalf("child reply: %v", err)
	}
	// parent's status for that message shows "replied"
	st := k.Status(p)
	if len(st) != 1 || !strings.Contains(st[0], "replied") {
		t.Fatalf("status = %v want one 'replied'", st)
	}
}

func TestGroups(t *testing.T) {
	k := New(runner.NewFake())
	k.RelaunchProbe = 0 // keep fresh-launch probe out of the test path
	a, _ := k.Spawn(addr.Root, "a", "/tmp/a", runner.SpawnOpts{Persona: "a"})
	b, _ := k.Spawn(addr.Root, "b", "/tmp/b", runner.SpawnOpts{Persona: "b"})

	// grouping alone shares no contacts
	k.CreateGroup("team", []addr.Address{a, b}, false)
	if k.Caps.CanSend(a, b) {
		t.Fatal("plain group should not introduce members")
	}

	// session reaches all members
	sess, err := k.AttachGroupSession("team", "/tmp/team", runner.SpawnOpts{Persona: "#team"})
	if err != nil {
		t.Fatalf("attach session: %v", err)
	}
	if !k.Caps.CanSend(sess, a) || !k.Caps.CanSend(sess, b) {
		t.Fatal("group session should reach every member")
	}
	if _, ok := k.Reg.Get(sess); !ok {
		t.Fatal("group session bubble should be in the registry")
	}

	// delete removes the group + session, but contacts and members remain
	k.DeleteGroup("team", false)
	if _, ok := k.Groups.Get("team"); ok {
		t.Fatal("group should be gone")
	}
	if _, ok := k.Reg.Get(sess); ok {
		t.Fatal("group session bubble should be removed")
	}
	if !k.Caps.CanSend(sess, a) {
		t.Fatal("deleting a group must NOT remove contacts")
	}
	if _, ok := k.Reg.Get(a); !ok {
		t.Fatal("deleting a group without deleteMembers must keep the member bubbles")
	}
}

func TestGroupIntroduceAll(t *testing.T) {
	k := New(runner.NewFake())
	k.RelaunchProbe = 0 // keep fresh-launch probe out of the test path
	a, _ := k.Spawn(addr.Root, "a", "/tmp/a", runner.SpawnOpts{Persona: "a"})
	b, _ := k.Spawn(addr.Root, "b", "/tmp/b", runner.SpawnOpts{Persona: "b"})
	k.CreateGroup("team", []addr.Address{a, b}, true) // introduce all
	if !k.Caps.CanSend(a, b) || !k.Caps.CanSend(b, a) {
		t.Fatal("introduce-all should make members mutual contacts")
	}
}

func TestStartRoot(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0 // keep fresh-launch probe out of the test path
	if err := k.StartRoot("/tmp/x"); err != nil {
		t.Fatalf("StartRoot: %v", err)
	}
	first := fr.Session(addr.Root)
	if first == nil {
		t.Fatal("root session not launched")
	}
	if err := k.StartRoot("/tmp/x"); err != nil || fr.Session(addr.Root) != first {
		t.Fatal("StartRoot should be idempotent")
	}
	if b, _ := k.Reg.Get(addr.Root); b.Dir != "/tmp/x" || b.SessionID == "" {
		t.Fatalf("root not configured: dir=%q sid=%q", b.Dir, b.SessionID)
	}
}

func TestIntroduceRootOnly(t *testing.T) {
	k := New(runner.NewFake())
	k.RelaunchProbe = 0 // keep fresh-launch probe out of the test path
	if err := k.Introduce("0.1", "0.2", "0.3"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("got %v want ErrNotAllowed", err)
	}
}

// TestSendHealsResumableBubble: a message to a crashed bubble relaunches it via
// --resume (same session id), then injects the notice into the new session.
func TestSendHealsResumableBubble(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0

	a, err := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	k.EnsureAlive(a) // lazy: first use launches it
	b, _ := k.Reg.Get(a)
	origID := b.SessionID
	orig := fr.Session(a)
	orig.Die() // the process crashes

	if _, err := k.Send(addr.Root, a, "ping", "body", 0, true); err != nil {
		t.Fatalf("send: %v", err)
	}
	ns := fr.Session(a)
	if ns == orig {
		t.Fatal("dead recipient should have been relaunched")
	}
	if !ns.Alive() {
		t.Fatal("relaunched session should be alive")
	}
	if !strings.Contains(ns.Written(), "📬 New message") {
		t.Fatalf("notice not injected into healed session: %q", ns.Written())
	}
	last := fr.Launches[len(fr.Launches)-1]
	if !last.Opts.Resume || last.Opts.SessionID != origID {
		t.Fatalf("expected a --resume of %q, got %+v", origID, last.Opts)
	}
	if b2, _ := k.Reg.Get(a); b2.SessionID != origID {
		t.Fatalf("session id should be unchanged on a successful resume, got %q", b2.SessionID)
	}
}

// TestSendHealsWithFreshFallback: when the resume fails (session id gone), Send
// falls back to a fresh session with a NEW id.
func TestSendHealsWithFreshFallback(t *testing.T) {
	fr := runner.NewFake()
	fr.FailResume = true // any --resume yields a dead session
	k := New(fr)
	k.RelaunchProbe = 0

	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	k.EnsureAlive(a) // lazy: first (fresh) launch; FailResume only affects --resume
	b, _ := k.Reg.Get(a)
	origID := b.SessionID
	fr.Session(a).Die()

	if _, err := k.Send(addr.Root, a, "ping", "body", 0, true); err != nil {
		t.Fatalf("send: %v", err)
	}
	ns := fr.Session(a)
	if !ns.Alive() {
		t.Fatal("fresh fallback session should be alive")
	}
	if !strings.Contains(ns.Written(), "📬 New message") {
		t.Fatalf("notice not injected into fresh session: %q", ns.Written())
	}
	b2, _ := k.Reg.Get(a)
	if b2.SessionID == origID {
		t.Fatalf("fresh fallback should assign a new session id, still %q", origID)
	}
	last := fr.Launches[len(fr.Launches)-1]
	if last.Opts.Resume || last.Opts.SessionID != b2.SessionID {
		t.Fatalf("expected a fresh (non-resume) launch with the new id, got %+v", last.Opts)
	}
}

// TestFocusHoldsWhileTyping: while the operator is ACTIVELY TYPING in a bubble,
// incoming messages are NOT injected (that would submit their half-typed line);
// they stay in the inbox and are flushed once typing pauses (FlushHeldIfIdle).
func TestFocusHoldsWhileTyping(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	k.TypingWindow = time.Hour // treat the operator as continuously typing

	a, _ := k.Spawn(addr.Root, "", "/tmp/a", runner.SpawnOpts{Name: "a"})
	k.EnsureAlive(a) // hot
	k.SetFocus(a)
	k.NoteKeystroke() // the operator is typing; with the huge window it stays "active"

	// even an URGENT message must not type into the bubble mid-keystroke
	if _, err := k.Send(addr.Root, a, "urgent!", "body", 0, true); err != nil {
		t.Fatalf("send: %v", err)
	}
	if strings.Contains(fr.Session(a).Written(), "New message") {
		t.Fatalf("must not inject while typing, got %q", fr.Session(a).Written())
	}
	if k.Store.UnreadCount(a) != 1 {
		t.Fatal("the message should still be filed in the inbox")
	}
	// still typing -> the idle flush does nothing
	k.FlushHeldIfIdle()
	if strings.Contains(fr.Session(a).Written(), "unread") {
		t.Fatal("must not flush while still typing")
	}

	// operator pauses -> the backlog is delivered
	k.TypingWindow = time.Nanosecond // now idle
	k.FlushHeldIfIdle()
	if !strings.Contains(fr.Session(a).Written(), "unread") {
		t.Fatalf("pausing should flush the held backlog, got %q", fr.Session(a).Written())
	}
}

// TestFocusDeliversWhenIdle: if the operator is on the bubble but NOT typing,
// messages are delivered immediately so they see them arrive.
func TestFocusDeliversWhenIdle(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0

	a, _ := k.Spawn(addr.Root, "", "/tmp/a", runner.SpawnOpts{Name: "a"})
	k.EnsureAlive(a)
	k.SetFocus(a) // just entered, not typing -> idle

	if _, err := k.Send(addr.Root, a, "fyi", "body", 0, true); err != nil {
		t.Fatalf("send: %v", err)
	}
	if !strings.Contains(fr.Session(a).Written(), "New message") {
		t.Fatal("an idle operator should see the message delivered to the focused bubble")
	}
}

// TestUnfocusedStillDelivers: a message to a bubble the operator is NOT in is
// delivered immediately (the focus hold is specific to the focused bubble).
func TestUnfocusedStillDelivers(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0

	a, _ := k.Spawn(addr.Root, "", "/tmp/a", runner.SpawnOpts{Name: "a"})
	b, _ := k.Spawn(addr.Root, "", "/tmp/b", runner.SpawnOpts{Name: "b"})
	k.EnsureAlive(a)
	k.EnsureAlive(b)
	k.SetFocus(a) // operator is in a, not b

	if _, err := k.Send(addr.Root, b, "hi", "body", 0, true); err != nil {
		t.Fatalf("send: %v", err)
	}
	if !strings.Contains(fr.Session(b).Written(), "New message") {
		t.Fatal("a message to a non-focused bubble should be delivered immediately")
	}
}

// TestDeliverWhenReady: a notice is held until the session has produced output
// (booted) and only then typed in — so a message that boots a cold bubble isn't
// lost into a still-initializing claude.
func TestDeliverWhenReady(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	a, _ := k.Spawn(addr.Root, "", "/tmp/a", runner.SpawnOpts{Name: "a"})
	k.EnsureAlive(a) // fake session, no output yet ("not booted")

	go func() { // it "boots" (produces output) shortly
		time.Sleep(120 * time.Millisecond)
		fr.Session(a).SetOutput("claude UI painted")
	}()
	k.deliverWhenReady(a, []byte("📬 New message"))

	if !strings.Contains(fr.Session(a).Written(), "New message") {
		t.Fatalf("should deliver once the session is up, got %q", fr.Session(a).Written())
	}
}

// TestResumeFallbackOnLostSession: when --resume comes back alive but claude has
// no record of the id ("No conversation found"), EnsureAlive falls back to a
// FRESH session with a new id instead of leaving the bubble stuck on the error.
func TestResumeFallbackOnLostSession(t *testing.T) {
	fr := runner.NewFake()
	fr.LostResume = true // any --resume reports "No conversation found" (but stays alive)
	k := New(fr)
	k.RelaunchProbe = 0

	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Name: "w", Goal: "carry on"})
	k.EnsureAlive(a) // first launch is fresh (LostResume only affects --resume)
	b, _ := k.Reg.Get(a)
	origID := b.SessionID
	fr.Session(a).Die() // it pages out / crashes

	s := k.EnsureAlive(a)
	if s == nil || !s.Alive() {
		t.Fatal("should have recovered with a live session")
	}
	last := fr.Launches[len(fr.Launches)-1]
	if last.Opts.Resume {
		t.Fatalf("a lost-session resume should fall back to a FRESH launch, got %+v", last.Opts)
	}
	if last.Opts.Goal != "carry on" {
		t.Fatalf("fresh fallback should re-seed the goal, got %+v", last.Opts)
	}
	if b2, _ := k.Reg.Get(a); b2.SessionID == origID {
		t.Fatalf("fresh fallback should assign a new session id, still %q", origID)
	}
}

// TestSendLiveBubbleNoRelaunch: a live recipient is never relaunched.
func TestSendLiveBubbleNoRelaunch(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0 // keep fresh-launch probe out of the test path
	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	k.EnsureAlive(a) // page in first
	n0 := len(fr.Launches)
	if _, err := k.Send(addr.Root, a, "ping", "body", 0, true); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(fr.Launches) != n0 {
		t.Fatalf("a live bubble should not be relaunched (launches %d -> %d)", n0, len(fr.Launches))
	}
}

// TestSpawnGrantDepthOne: root grants spawn (depth 1); the grantee can spawn but
// its children cannot — an AI can't hand its spawn grant down.
func TestSpawnGrantDepthOne(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0 // keep fresh-launch probe out of the test path

	// root spawns a manager WITH the grant
	mgr, err := k.SpawnUnder(addr.Root, addr.Root, "mgr", "/tmp/mgr", runner.SpawnOpts{Persona: "mgr", GrantSpawn: true})
	if err != nil {
		t.Fatalf("spawn mgr: %v", err)
	}
	if !k.Caps.CanSpawn(mgr) {
		t.Fatal("granted manager should be able to spawn")
	}
	// the manager spawns a worker (no grant flag passed by an AI)
	worker, err := k.Spawn(mgr, "worker", "/tmp/worker", runner.SpawnOpts{Persona: "worker"})
	if err != nil {
		t.Fatalf("mgr spawn worker: %v", err)
	}
	if k.Caps.CanSpawn(worker) {
		t.Fatal("a depth-1 manager's child must NOT inherit the spawn ability")
	}

	// a bubble spawned WITHOUT the grant cannot spawn at all
	plain, _ := k.SpawnUnder(addr.Root, addr.Root, "plain", "/tmp/plain", runner.SpawnOpts{Persona: "plain"})
	if k.Caps.CanSpawn(plain) {
		t.Fatal("ungranted bubble should not be able to spawn")
	}
}

// TestSpawnPassesModel: the chosen model reaches the runner.
func TestSpawnPassesModel(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0 // keep fresh-launch probe out of the test path
	a, err := k.SpawnUnder(addr.Root, addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w", Model: "opus"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	k.EnsureAlive(a) // lazy: the launch happens on first use
	last := fr.Launches[len(fr.Launches)-1]
	if last.Opts.Model != "opus" {
		t.Fatalf("model = %q want opus", last.Opts.Model)
	}
}

// TestDeleteBubbleSubtree: deleting a bubble removes it and its descendants, and
// purges group membership; root is never deletable.
func TestDeleteBubbleSubtree(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0 // keep fresh-launch probe out of the test path
	parent, _ := k.Spawn(addr.Root, "parent", "/tmp/p", runner.SpawnOpts{Persona: "parent"})       // 0.1
	child, _ := k.SpawnUnder(addr.Root, parent, "child", "/tmp/c", runner.SpawnOpts{Persona: "c"}) // 0.1.1
	other, _ := k.Spawn(addr.Root, "other", "/tmp/o", runner.SpawnOpts{Persona: "other"})          // 0.2
	k.CreateGroup("team", []addr.Address{parent, other}, false)
	k.EnsureAlive(parent) // launch so there are sessions to kill
	k.EnsureAlive(child)

	removed := k.DeleteBubble(parent)
	if len(removed) != 2 {
		t.Fatalf("removed = %v want [child parent]", removed)
	}
	if _, ok := k.Reg.Get(parent); ok {
		t.Fatal("parent should be removed")
	}
	if _, ok := k.Reg.Get(child); ok {
		t.Fatal("child (subtree) should be removed")
	}
	if !fr.Session(parent).Closed() || !fr.Session(child).Closed() {
		t.Fatal("deleted bubbles' sessions should be killed")
	}
	g, _ := k.Groups.Get("team")
	if len(g.Members) != 1 || g.Members[0] != other {
		t.Fatalf("group membership should be purged of deleted bubble: %+v", g.Members)
	}
	if r := k.DeleteBubble(addr.Root); r != nil {
		t.Fatal("root must not be deletable")
	}
	if _, ok := k.Reg.Get(other); !ok {
		t.Fatal("unrelated bubble should survive")
	}
}

// TestDeleteGroupWithMembers: deleteMembers=true removes the member bubbles too.
func TestDeleteGroupWithMembers(t *testing.T) {
	k := New(runner.NewFake())
	k.RelaunchProbe = 0 // keep fresh-launch probe out of the test path
	a, _ := k.Spawn(addr.Root, "a", "/tmp/a", runner.SpawnOpts{Persona: "a"})
	b, _ := k.Spawn(addr.Root, "b", "/tmp/b", runner.SpawnOpts{Persona: "b"})
	k.CreateGroup("team", []addr.Address{a, b}, false)

	k.DeleteGroup("team", true)
	if _, ok := k.Groups.Get("team"); ok {
		t.Fatal("group should be gone")
	}
	if _, ok := k.Reg.Get(a); ok {
		t.Fatal("member a should be deleted with the group")
	}
	if _, ok := k.Reg.Get(b); ok {
		t.Fatal("member b should be deleted with the group")
	}
}


// TestMemBudgetEviction: sessions are packed by ACTUAL memory against MemBudget;
// the coldest page out (state preserved) only when the sum exceeds the budget —
// small sessions don't reserve a fixed slot.
func TestMemBudgetEviction(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0 // keep fresh-launch probe out of the test path
	k.MemBudget = 1000 // bytes (tiny, for the test)
	k.RelaunchProbe = 0

	a, _ := k.Spawn(addr.Root, "a", "/tmp/a", runner.SpawnOpts{Persona: "a"})
	b, _ := k.Spawn(addr.Root, "b", "/tmp/b", runner.SpawnOpts{Persona: "b"})
	c, _ := k.Spawn(addr.Root, "c", "/tmp/c", runner.SpawnOpts{Persona: "c"})
	k.EnsureAlive(a) // lazy: page each in so they have a session to measure
	k.EnsureAlive(b)
	k.EnsureAlive(c)
	fr.Session(a).SetMem(300)
	fr.Session(b).SetMem(300)
	fr.Session(c).SetMem(300) // total 900 <= 1000: all fit, nothing evicted

	k.EnforceBudget()
	if !k.IsHot(a) || !k.IsHot(b) || !k.IsHot(c) {
		t.Fatal("three small sessions (900) should all fit under budget 1000")
	}

	// one balloons -> total 1400 > 1000 -> evict coldest (a, then b) until it fits
	fr.Session(c).SetMem(800) // 300 + 300 + 800 = 1400
	k.EnforceBudget()
	if k.IsHot(a) || k.IsHot(b) {
		t.Fatal("coldest sessions should page out until the sum fits the budget")
	}
	if !k.IsHot(c) {
		t.Fatal("the recently-used session should stay resident")
	}
	if _, ok := k.Reg.Get(a); !ok {
		t.Fatal("paged-out bubble keeps its record (resumes on use)")
	}

	// used-recency wins: touch b back in, and it should outlive an idle one
	if s := k.EnsureAlive(b); s == nil {
		t.Fatal("b should page back in on use")
	}
	fr.Session(b).SetMem(300) // now hot: b(300) + c(800) = 1100 > 1000 -> evict coldest
	k.EnforceBudget()
	if k.IsHot(c) && k.IsHot(b) {
		t.Fatal("1100 exceeds budget 1000: one of them must page out")
	}
}

// TestLazySpawnColdUntilUsed: spawning creates a cold record (0 RAM, no process,
// no session id). First use (a message) launches it fresh with its goal.
func TestLazySpawnColdUntilUsed(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0 // keep fresh-launch probe out of the test path

	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w", Goal: "do the thing"})
	if k.IsHot(a) {
		t.Fatal("spawn must NOT launch (lazy) — the bubble should be cold")
	}
	if len(fr.Launches) != 0 {
		t.Fatalf("no launch should happen at spawn, got %d", len(fr.Launches))
	}
	if b, _ := k.Reg.Get(a); b.SessionID != "" {
		t.Fatal("no session id should be assigned until first launch")
	}

	// A message pages it in — launched fresh, with its goal as the initial prompt.
	if _, err := k.Send(addr.Root, a, "hi", "", 0, true); err != nil {
		t.Fatalf("send: %v", err)
	}
	if !k.IsHot(a) {
		t.Fatal("a message should have paged the bubble in")
	}
	if len(fr.Launches) != 1 {
		t.Fatalf("first use should launch exactly once, got %d", len(fr.Launches))
	}
	if last := fr.Launches[0]; last.Opts.Resume || last.Opts.Goal != "do the thing" {
		t.Fatalf("first launch should be fresh with the goal, got %+v", last.Opts)
	}
	if b, _ := k.Reg.Get(a); b.SessionID == "" {
		t.Fatal("session id should be assigned on first launch (so later use resumes)")
	}
}

// TestNonUrgentBootsFreshBubble: a non-urgent message to a NEVER-LAUNCHED bubble
// boots it — a freshly-spawned worker awaiting its first task must start when a
// charter is delegated to it, not sit cold until the next drain.
func TestNonUrgentBootsFreshBubble(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0 // keep fresh-launch probe out of the test path

	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Name: "w", Goal: "do the thing"})

	if _, err := k.Send(addr.Root, a, "start", "body", 0, false); err != nil {
		t.Fatalf("send: %v", err)
	}
	if !k.IsHot(a) {
		t.Fatal("a non-urgent message to a never-launched bubble should boot it")
	}
	if len(fr.Launches) != 1 {
		t.Fatalf("expected exactly one launch, got %d", len(fr.Launches))
	}
	if last := fr.Launches[0]; last.Opts.Resume || last.Opts.Goal != "do the thing" {
		t.Fatalf("first launch should be fresh with the goal, got %+v", last.Opts)
	}
	if !strings.Contains(fr.Session(a).Written(), "📬 New message") {
		t.Fatal("the freshly-booted bubble should be nudged to read")
	}
}

// TestNonUrgentPoolsPagedOutBubble: a non-urgent message to a PREVIOUSLY-RUN,
// now paged-out bubble (it has a session id) stays pooled until DrainInboxes —
// we don't wake a whole sleeping fleet on every message.
func TestNonUrgentPoolsPagedOutBubble(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0

	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	k.EnsureAlive(a)    // launch once so it gets a session id
	fr.Session(a).Die() // then it pages out / crashes -> cold but has a SessionID
	n0 := len(fr.Launches)

	// non-urgent -> filed, but the paged-out bubble is NOT relaunched
	if _, err := k.Send(addr.Root, a, "later", "body", 0, false); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(fr.Launches) != n0 {
		t.Fatalf("a pooled message should not relaunch a paged-out bubble (launches %d -> %d)", n0, len(fr.Launches))
	}
	if k.Store.UnreadCount(a) != 1 {
		t.Fatalf("the message should be pooled in the inbox, unread=%d", k.Store.UnreadCount(a))
	}

	// the drain cycle delivers it: pages the bubble back in and nudges it to read
	k.DrainInboxes()
	if !k.IsHot(a) {
		t.Fatal("DrainInboxes should page in a bubble that has pending mail")
	}
	if w := fr.Session(a).Written(); !strings.Contains(w, "unread message") {
		t.Fatalf("drain should nudge the bubble to read, got %q", w)
	}
}

// TestNonUrgentDeliversToHotBubble: a non-urgent message to an already-hot bubble
// is delivered immediately (no waiting for the drain) — pooling only defers COLD
// recipients.
func TestNonUrgentDeliversToHotBubble(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0 // keep fresh-launch probe out of the test path

	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	k.EnsureAlive(a) // it's hot (running)

	if _, err := k.Send(addr.Root, a, "fyi", "body", 0, false); err != nil {
		t.Fatalf("send: %v", err)
	}
	if !strings.Contains(fr.Session(a).Written(), "📬 New message") {
		t.Fatal("a non-urgent message to a HOT bubble should be delivered immediately, not deferred to the drain")
	}
}

// TestResumeUsesCurrentSessionID: if the session switched conversations (/resume)
// while running, a later relaunch resumes the CURRENT id (from the hook), not the
// stale launch id, and the registry is updated.
func TestResumeUsesCurrentSessionID(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	current := ""
	k.CurrentSessionID = func(a addr.Address) string { return current } // stands in for the session hook

	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	k.EnsureAlive(a) // fresh launch assigns an id

	current = "resumed-abc" // user /resumes another conversation inside the session
	fr.Session(a).Die()     // it goes cold/crashes

	if s := k.EnsureAlive(a); s == nil || !s.Alive() {
		t.Fatal("should relaunch")
	}
	last := fr.Launches[len(fr.Launches)-1]
	if !last.Opts.Resume || last.Opts.SessionID != "resumed-abc" {
		t.Fatalf("should resume the current (resumed) id, got %+v", last.Opts)
	}
	if b, _ := k.Reg.Get(a); b.SessionID != "resumed-abc" {
		t.Fatalf("registry should be updated to the resumed id, got %q", b.SessionID)
	}

	// SyncSessionIDs also pulls the current id (for persistence of a hot bubble)
	current = "resumed-xyz"
	k.SyncSessionIDs()
	if b, _ := k.Reg.Get(a); b.SessionID != "resumed-xyz" {
		t.Fatalf("SyncSessionIDs should refresh to %q, got %q", "resumed-xyz", b.SessionID)
	}
}

// TestEditDeleteBySubtreeOnly: a bubble may edit/delete only bubbles in its OWN
// subtree (its spawned descendants) — not siblings, not its parent, not root.
func TestEditDeleteBySubtreeOnly(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0 // keep fresh-launch probe out of the test path

	mgr, _ := k.SpawnUnder(addr.Root, addr.Root, "", "/tmp/m", runner.SpawnOpts{Name: "mgr", GrantSpawn: true}) // 0.1
	w1, _ := k.Spawn(mgr, "", "/tmp/w1", runner.SpawnOpts{Name: "w1"})                                          // 0.1.1
	w2, _ := k.Spawn(mgr, "", "/tmp/w2", runner.SpawnOpts{Name: "w2"})                                          // 0.1.2
	other, _ := k.SpawnUnder(addr.Root, addr.Root, "", "/tmp/o", runner.SpawnOpts{Name: "other"})               // 0.2

	// edit own child: allowed, fields update (empty ones unchanged)
	if err := k.EditBy(mgr, w1, "worker-one", "opus", "new charter"); err != nil {
		t.Fatalf("mgr edit own child: %v", err)
	}
	b, _ := k.Reg.Get(w1)
	if b.Name != "worker-one" || b.Model != "opus" || b.Goal != "new charter" {
		t.Fatalf("edit not applied: %+v", b)
	}
	if err := k.EditBy(mgr, w1, "", "", ""); err != nil || b.Name != "worker-one" {
		t.Fatalf("empty fields must be unchanged: err=%v name=%q", err, b.Name)
	}

	// edit outside the subtree: denied
	if err := k.EditBy(mgr, other, "hijack", "", ""); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("edit of a non-descendant should be ErrNotAllowed, got %v", err)
	}
	if err := k.EditBy(w1, mgr, "revolt", "", ""); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("a child must not edit its parent, got %v", err)
	}

	// prefix trap: 0.1 must not control 0.10
	ten := addr.Address("0.10")
	k.Reg.Restore(registry.Bubble{Addr: ten, Persona: "ten"})
	if err := k.EditBy(mgr, ten, "x", "", ""); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("0.1 must not edit 0.10 (prefix trap), got %v", err)
	}

	// delete outside the subtree: denied; own child: allowed (subtree removed)
	if _, err := k.DeleteBy(mgr, other); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("delete of a non-descendant should be ErrNotAllowed, got %v", err)
	}
	if _, err := k.DeleteBy(w1, addr.Root); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("root must never be deletable, got %v", err)
	}
	sub, _ := k.SpawnUnder(addr.Root, w2, "", "/tmp/s", runner.SpawnOpts{Name: "sub"}) // 0.1.2.1 (grandchild)
	victims, err := k.DeleteBy(mgr, w2)
	if err != nil || len(victims) != 2 {
		t.Fatalf("mgr should delete its child + grandchild, got victims=%v err=%v", victims, err)
	}
	if _, ok := k.Reg.Get(w2); ok {
		t.Fatal("deleted child should be gone")
	}
	if _, ok := k.Reg.Get(sub); ok {
		t.Fatal("deleted grandchild should be gone")
	}
	// root can delete anything
	if _, err := k.DeleteBy(addr.Root, other); err != nil {
		t.Fatalf("root delete: %v", err)
	}
	// deleting a missing bubble errors cleanly
	if _, err := k.DeleteBy(mgr, w2); err == nil {
		t.Fatal("deleting an already-removed bubble should error")
	}
}

// TestDeletePurgesContacts: deleting a bubble removes it from EVERY other
// bubble's contacts, so it can't linger as a nameless ghost (the zombie-contacts
// bug). Also verifies Contacts filters out any address with no registry entry.
func TestDeletePurgesContacts(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0 // keep fresh-launch probe out of the test path

	a, _ := k.Spawn(addr.Root, "a", "/tmp/a", runner.SpawnOpts{Name: "a"})
	b, _ := k.Spawn(addr.Root, "b", "/tmp/b", runner.SpawnOpts{Name: "b"})
	k.Introduce(addr.Root, a, b) // a<->b mutual contacts

	if !contains(k.Contacts(a), b) {
		t.Fatal("a should have b as a contact after introduce")
	}
	k.DeleteBubble(b)
	if contains(k.Contacts(a), b) {
		t.Fatalf("deleted bubble must be purged from a's contacts, got %v", k.Contacts(a))
	}
	// even a hand-injected stale cap edge is filtered because b has no registry entry
	k.Caps.AddContact(a, b)
	if contains(k.Contacts(a), b) {
		t.Fatal("Contacts must filter addresses with no registry entry (ghost)")
	}
}

// TestCompact: a running bubble's compact() types the /compact command into its
// own session; a cold bubble reports it isn't running.
func TestCompact(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	a, _ := k.Spawn(addr.Root, "", "/tmp/a", runner.SpawnOpts{Name: "a"})

	if err := k.Compact(a, "keep the schema"); err == nil {
		t.Fatal("compacting a cold bubble should error")
	}
	k.EnsureAlive(a) // hot
	if err := k.Compact(a, "keep the schema"); err != nil {
		t.Fatalf("compact: %v", err)
	}
	w := fr.Session(a).Written()
	if !strings.Contains(w, "/compact keep the schema") {
		t.Fatalf("compact should type the slash command, got %q", w)
	}
	// no focus -> bare /compact
	if err := k.Compact(a, "  "); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !strings.Contains(fr.Session(a).Written(), "/compact") {
		t.Fatal("bare compact should still type /compact")
	}
}

// TestForget: a bubble can drop a contact from its own list; root is never
// forgettable.
func TestForget(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0 // keep fresh-launch probe out of the test path
	a, _ := k.Spawn(addr.Root, "a", "/tmp/a", runner.SpawnOpts{Name: "a"})
	b, _ := k.Spawn(addr.Root, "b", "/tmp/b", runner.SpawnOpts{Name: "b"})
	k.Introduce(addr.Root, a, b)

	if err := k.Forget(a, b); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if contains(k.Contacts(a), b) {
		t.Fatal("forgotten contact should be gone from a")
	}
	if !contains(k.Contacts(b), a) { // one-directional: b still keeps a
		t.Fatal("forget should only drop the caller's edge, not the reverse")
	}
	if err := k.Forget(a, addr.Root); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("root must not be forgettable, got %v", err)
	}
}

// TestEvictIdle: a session with no output past IdleTimeout is paged out; an
// actively-working one (recent output) stays hot.
func TestEvictIdle(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0 // keep fresh-launch probe out of the test path
	k.IdleTimeout = 10 * time.Minute
	k.RelaunchProbe = 0

	idle, _ := k.Spawn(addr.Root, "idle", "/tmp/i", runner.SpawnOpts{Name: "idle"})
	busy, _ := k.Spawn(addr.Root, "busy", "/tmp/b", runner.SpawnOpts{Name: "busy"})
	k.EnsureAlive(idle)
	k.EnsureAlive(busy)
	fr.Session(idle).SetLastActivity(time.Now().Add(-30 * time.Minute)) // silent for 30m
	fr.Session(busy).SetLastActivity(time.Now())                        // just produced output

	k.EvictIdle()
	if k.IsHot(idle) {
		t.Fatal("an idle session (no output for 30m) should page out")
	}
	if !k.IsHot(busy) {
		t.Fatal("an actively-working session should stay hot")
	}
	if _, ok := k.Reg.Get(idle); !ok {
		t.Fatal("paged-out idle bubble keeps its record (resumes on use)")
	}
}

// TestSampleUsage: reports mem + CPU for live workers, excludes root and cold.
func TestSampleUsage(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0 // keep fresh-launch probe out of the test path
	a, _ := k.Spawn(addr.Root, "a", "/tmp/a", runner.SpawnOpts{Name: "worker-a"})
	k.Spawn(addr.Root, "b", "/tmp/b", runner.SpawnOpts{Name: "b"}) // stays cold
	k.EnsureAlive(a)
	fr.Session(a).SetMem(500)
	fr.Session(a).SetCPU(3 * time.Second)

	u := k.SampleUsage()
	if len(u) != 1 {
		t.Fatalf("only the live worker should be sampled, got %d", len(u))
	}
	if u[0].Addr != a || u[0].Name != "worker-a" || u[0].Mem != 500 || u[0].CPU != 3*time.Second {
		t.Fatalf("usage = %+v", u[0])
	}
}

func contains(xs []addr.Address, x addr.Address) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// TestIntroduceBySubtree: a bubble can introduce two of its OWN descendants, but
// not bubbles outside its subtree; root can introduce anyone.
func TestIntroduceBySubtree(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0

	mgr, _ := k.SpawnUnder(addr.Root, addr.Root, "", "/tmp/m", runner.SpawnOpts{Name: "mgr", GrantSpawn: true}) // 0.1
	w1, _ := k.Spawn(mgr, "", "/tmp/w1", runner.SpawnOpts{Name: "w1"})                                          // 0.1.1
	w2, _ := k.Spawn(mgr, "", "/tmp/w2", runner.SpawnOpts{Name: "w2"})                                          // 0.1.2
	other, _ := k.SpawnUnder(addr.Root, addr.Root, "", "/tmp/o", runner.SpawnOpts{Name: "other"})               // 0.2

	if err := k.IntroduceBy(mgr, w1, w2); err != nil {
		t.Fatalf("mgr introduce its own children: %v", err)
	}
	if !k.Caps.CanSend(w1, w2) || !k.Caps.CanSend(w2, w1) {
		t.Fatal("introduced siblings should be mutual contacts")
	}
	// can't introduce a bubble outside the subtree
	if err := k.IntroduceBy(mgr, w1, other); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("introducing outside the subtree should be denied, got %v", err)
	}
	// a worker can't introduce its parent's peers
	if err := k.IntroduceBy(w1, mgr, other); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("non-owner introduce should be denied, got %v", err)
	}
	// root may introduce anyone
	if err := k.IntroduceBy(addr.Root, mgr, other); err != nil {
		t.Fatalf("root introduce: %v", err)
	}
}

// TestBroadcastBy: a broadcast reaches every descendant (not the sender, not
// peers outside the subtree), and each recipient can reply.
func TestBroadcastBy(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0

	mgr, _ := k.SpawnUnder(addr.Root, addr.Root, "", "/tmp/m", runner.SpawnOpts{Name: "mgr", GrantSpawn: true}) // 0.1
	w1, _ := k.Spawn(mgr, "", "/tmp/w1", runner.SpawnOpts{Name: "w1"})                                          // 0.1.1
	sub, _ := k.SpawnUnder(addr.Root, w1, "", "/tmp/s", runner.SpawnOpts{Name: "sub"})                          // 0.1.1.1 (grandchild)
	other, _ := k.SpawnUnder(addr.Root, addr.Root, "", "/tmp/o", runner.SpawnOpts{Name: "other"})              // 0.2

	n := k.BroadcastBy(mgr, "standup", "post status", false)
	if n != 2 { // w1 and sub, not mgr itself, not other
		t.Fatalf("broadcast reached %d want 2", n)
	}
	if k.Store.UnreadCount(w1) != 1 || k.Store.UnreadCount(sub) != 1 {
		t.Fatal("every descendant should have received the broadcast")
	}
	if k.Store.UnreadCount(other) != 0 {
		t.Fatal("a bubble outside the subtree must not receive it")
	}
	if k.Store.UnreadCount(mgr) != 0 {
		t.Fatal("the sender should not message itself")
	}
	// recipients can reply to the broadcaster
	if !k.Caps.CanSend(w1, mgr) {
		t.Fatal("a broadcast recipient should be able to reply to the sender")
	}
}

// TestNudgeDedup: overlapping nudges for the same backlog don't stack; a new
// message past the announced level nudges again; reading resets it.
func TestNudgeDedup(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0

	a, _ := k.Spawn(addr.Root, "", "/tmp/a", runner.SpawnOpts{Name: "a"})
	k.EnsureAlive(a) // hot

	k.Send(addr.Root, a, "m1", "", 0, true)
	first := strings.Count(fr.Session(a).Written(), "📬 New message")
	if first != 1 {
		t.Fatalf("first message should nudge once, got %d", first)
	}
	// a redundant drain for the same unread level must NOT add another notice
	k.DrainInboxes()
	if strings.Count(fr.Session(a).Written(), "unread message") != 0 {
		t.Fatal("drain re-announced an already-nudged backlog")
	}
	// a second message (unread grows) DOES nudge again
	k.Send(addr.Root, a, "m2", "", 0, true)
	if strings.Count(fr.Session(a).Written(), "📬 New message") != 2 {
		t.Fatal("a new message past the announced level should nudge again")
	}
	// reading resets: the next message nudges even at the same count
	k.Inbox(a)
	k.Send(addr.Root, a, "m3", "", 0, true)
	if strings.Count(fr.Session(a).Written(), "📬 New message") != 3 {
		t.Fatal("after reading, a new message should nudge again")
	}
}

// TestSpawnNameDescription: the new spawn sets Name + Goal (initial instruction);
// Label() prefers Name; a legacy bubble with only Persona falls back to Persona.
func TestSpawnNameDescription(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0 // keep fresh-launch probe out of the test path

	a, _ := k.Spawn(addr.Root, "", "/tmp/a", runner.SpawnOpts{Name: "support", Goal: "triage the tickets"})
	b, _ := k.Reg.Get(a)
	if b.Name != "support" || b.Goal != "triage the tickets" {
		t.Fatalf("name/goal not set: name=%q goal=%q", b.Name, b.Goal)
	}
	if b.Label() != "support" {
		t.Fatalf("Label should prefer Name, got %q", b.Label())
	}
	k.EnsureAlive(a) // first launch uses the description as the initial prompt
	if last := fr.Launches[len(fr.Launches)-1]; last.Opts.Goal != "triage the tickets" {
		t.Fatalf("first launch should carry the description as goal, got %+v", last.Opts)
	}

	// backward compat: a bubble with Persona but no Name shows Persona
	c, _ := k.Spawn(addr.Root, "legacy", "/tmp/c", runner.SpawnOpts{Persona: "legacy"})
	cb, _ := k.Reg.Get(c)
	if cb.Name != "" || cb.Label() != "legacy" {
		t.Fatalf("legacy bubble should fall back to Persona: Name=%q Label=%q", cb.Name, cb.Label())
	}
}
