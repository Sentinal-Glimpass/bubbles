package kernel

import (
	"errors"
	"strings"
	"testing"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/bus"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
)

func TestFleetEndToEnd(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)

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
	if _, err := k.Send(scout, addr.Root, "found 3 bugs", "details", 0); err != nil {
		t.Fatalf("scout->root: %v", err)
	}
	if len(pings) != 1 || pings[0].From != scout {
		t.Fatalf("pings = %+v", pings)
	}

	// Workers can't talk before introduction.
	if _, err := k.Send(scout, refactor, "hi", "", 0); !errors.Is(err, ErrNotContact) {
		t.Fatalf("got %v want ErrNotContact", err)
	}
	if err := k.Introduce(addr.Root, scout, refactor); err != nil {
		t.Fatalf("introduce: %v", err)
	}

	// Worker -> worker: lands in the inbox AND queues a non-interrupting notice.
	if _, err := k.Send(scout, refactor, "take the API layer", "thanks", 0); err != nil {
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
	p, _ := k.Spawn(addr.Root, "p", "/tmp/p", runner.SpawnOpts{Persona: "p"})
	c, _ := k.SpawnUnder(addr.Root, p, "c", "/tmp/c", runner.SpawnOpts{Persona: "c"})

	if k.Caps.CanSend(c, p) {
		t.Fatal("child should not reach parent before being messaged")
	}
	id, err := k.Send(p, c, "do X", "", 0) // parent messages child
	if err != nil {
		t.Fatalf("parent->child: %v", err)
	}
	if !k.Caps.CanSend(c, p) {
		t.Fatal("child should be able to reply after the parent messaged it")
	}
	if _, err := k.Send(c, p, "done", "", id); err != nil { // child replies (threaded)
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

	// delete removes the group + session, but contacts remain
	k.DeleteGroup("team")
	if _, ok := k.Groups.Get("team"); ok {
		t.Fatal("group should be gone")
	}
	if _, ok := k.Reg.Get(sess); ok {
		t.Fatal("group session bubble should be removed")
	}
	if !k.Caps.CanSend(sess, a) {
		t.Fatal("deleting a group must NOT remove contacts")
	}
}

func TestGroupIntroduceAll(t *testing.T) {
	k := New(runner.NewFake())
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
	b, _ := k.Reg.Get(a)
	origID := b.SessionID
	orig := fr.Session(a)
	orig.Die() // the process crashes

	if _, err := k.Send(addr.Root, a, "ping", "body", 0); err != nil {
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
	b, _ := k.Reg.Get(a)
	origID := b.SessionID
	fr.Session(a).Die()

	if _, err := k.Send(addr.Root, a, "ping", "body", 0); err != nil {
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

// TestSendLiveBubbleNoRelaunch: a live recipient is never relaunched.
func TestSendLiveBubbleNoRelaunch(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	n0 := len(fr.Launches)
	if _, err := k.Send(addr.Root, a, "ping", "body", 0); err != nil {
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
	if _, err := k.SpawnUnder(addr.Root, addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w", Model: "opus"}); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	last := fr.Launches[len(fr.Launches)-1]
	if last.Opts.Model != "opus" {
		t.Fatalf("model = %q want opus", last.Opts.Model)
	}
}
