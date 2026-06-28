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

	// Worker pings root.
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

	// Root introduces them, then they text each other (delivery is injected).
	if err := k.Introduce(addr.Root, scout, refactor); err != nil {
		t.Fatalf("introduce: %v", err)
	}
	if err := k.Send(scout, refactor, "take the API layer", "thanks"); err != nil {
		t.Fatalf("scout->refactor: %v", err)
	}
	if got := fr.Session(refactor).Written(); !strings.Contains(got, "from "+scout.String()) ||
		!strings.Contains(got, "take the API layer") {
		t.Fatalf("refactor session got %q", got)
	}
	if err := k.Send(refactor, scout, "on it", ""); err != nil {
		t.Fatalf("refactor->scout: %v", err)
	}
	if got := fr.Session(scout).Written(); !strings.Contains(got, "from "+refactor.String()) {
		t.Fatalf("scout session got %q", got)
	}
}

func TestIntroduceRootOnly(t *testing.T) {
	k := New(runner.NewFake())
	if err := k.Introduce("0.1", "0.2", "0.3"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("got %v want ErrNotAllowed", err)
	}
}
