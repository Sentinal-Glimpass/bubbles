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
	if err := k.Send(scout, addr.Root, "found 3 bugs", "details"); err != nil {
		t.Fatalf("scout->root: %v", err)
	}
	if len(pings) != 1 || pings[0].From != scout {
		t.Fatalf("pings = %+v", pings)
	}

	// Workers can't talk before introduction.
	if err := k.Send(scout, refactor, "hi", ""); !errors.Is(err, ErrNotContact) {
		t.Fatalf("got %v want ErrNotContact", err)
	}
	if err := k.Introduce(addr.Root, scout, refactor); err != nil {
		t.Fatalf("introduce: %v", err)
	}

	// Worker -> worker: lands in the inbox AND queues a non-interrupting notice.
	if err := k.Send(scout, refactor, "take the API layer", "thanks"); err != nil {
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

func TestIntroduceRootOnly(t *testing.T) {
	k := New(runner.NewFake())
	if err := k.Introduce("0.1", "0.2", "0.3"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("got %v want ErrNotAllowed", err)
	}
}
