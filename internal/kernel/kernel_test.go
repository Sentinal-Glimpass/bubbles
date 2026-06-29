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
