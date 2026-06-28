package caps

import (
	"errors"
	"testing"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

func TestContactsAndSend(t *testing.T) {
	s := New()
	s.AddContact("0.1", addr.Root) // fresh bubble knows only root
	if !s.CanSend("0.1", addr.Root) {
		t.Fatal("0.1 should reach root")
	}
	if s.CanSend("0.1", "0.2") {
		t.Fatal("0.1 should not reach 0.2 before introduction")
	}
	s.Introduce("0.1", "0.2")
	if !s.CanSend("0.1", "0.2") || !s.CanSend("0.2", "0.1") {
		t.Fatal("introduction should make contact mutual")
	}
}

func TestRootCanSendAnyone(t *testing.T) {
	s := New()
	if !s.CanSend(addr.Root, "0.7") {
		t.Fatal("root should reach anyone")
	}
}

func TestSpawnBudget(t *testing.T) {
	s := New()
	if !s.CanSpawn(addr.Root) {
		t.Fatal("root should always spawn")
	}
	if s.CanSpawn("0.1") {
		t.Fatal("ungranted bubble should not spawn")
	}
	s.GrantSpawn("0.1", 1)
	if !s.CanSpawn("0.1") {
		t.Fatal("granted bubble should spawn")
	}
	if err := s.ConsumeSpawn("0.1"); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if err := s.ConsumeSpawn("0.1"); !errors.Is(err, ErrNoBudget) {
		t.Fatalf("second consume got %v want ErrNoBudget", err)
	}
}
