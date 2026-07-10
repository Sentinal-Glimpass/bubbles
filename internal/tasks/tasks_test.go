package tasks

import (
	"testing"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

func TestCreateGetAndSequence(t *testing.T) {
	s := New()
	a := s.Create(Task{Assigner: "0", Worker: "0.1", Brief: "do x"})
	b := s.Create(Task{Assigner: "0", Worker: "0.2", Brief: "do y"})
	if a.ID != "t1" || b.ID != "t2" {
		t.Fatalf("ids = %s, %s", a.ID, b.ID)
	}
	got, ok := s.Get("t1")
	if !ok || got.Brief != "do x" || got.State != Open {
		t.Fatalf("get t1 = %+v ok=%v", got, ok)
	}
}

func TestRejectIncrementsRounds(t *testing.T) {
	s := New()
	tk := s.Create(Task{Worker: "0.1"})
	s.SetState(tk.ID, Checking)
	if !s.Reject(tk.ID) {
		t.Fatal("reject failed")
	}
	got, _ := s.Get(tk.ID)
	if got.State != Open || got.Rounds != 1 {
		t.Fatalf("after reject: %+v", got)
	}
}

func TestOpenBetween(t *testing.T) {
	s := New()
	tk := s.Create(Task{Assigner: "0.1", Worker: "0.1.2"})
	if id := s.OpenBetween("0.1.2", "0.1"); id != tk.ID {
		t.Fatalf("open between = %q", id)
	}
	if id := s.OpenBetween("0.1", "0.1.2"); id != "" {
		t.Fatalf("reversed direction should be empty, got %q", id)
	}
	s.SetState(tk.ID, Done)
	if id := s.OpenBetween("0.1.2", "0.1"); id != "" {
		t.Fatalf("done task should not annotate, got %q", id)
	}
}

func TestSnapshotLoadContinuesSequence(t *testing.T) {
	s := New()
	s.Create(Task{Worker: "0.1"})
	s.Create(Task{Worker: "0.2"})
	snap, seq := s.Snapshot()

	s2 := New()
	s2.Load(snap, seq)
	c := s2.Create(Task{Worker: "0.3"})
	if c.ID != "t3" {
		t.Fatalf("sequence not continued: %s", c.ID)
	}
}

func TestForListsAllParticipations(t *testing.T) {
	s := New()
	s.Create(Task{Assigner: "0", Worker: "0.1", Verifier: "0.9"})
	s.Create(Task{Assigner: "0.1", Worker: "0.1.1"})
	if n := len(s.For(addr.Address("0.1"))); n != 2 {
		t.Fatalf("0.1 participates in %d tasks, want 2", n)
	}
	if n := len(s.For(addr.Address("0.9"))); n != 1 {
		t.Fatalf("verifier participates in %d, want 1", n)
	}
}

func TestPurgeParticipant(t *testing.T) {
	s := New()
	a := s.Create(Task{Assigner: "0", Worker: "0.1", Verifier: "0.9"})
	b := s.Create(Task{Assigner: "0.1", Worker: "0.2"})
	s.PurgeParticipant("0.9") // verifier deleted → task degrades, stays open
	got, _ := s.Get(a.ID)
	if got.State != Open || got.Verifier != "" {
		t.Fatalf("verifier purge: %+v", got)
	}
	s.PurgeParticipant("0.2") // worker deleted → task cancelled
	got, _ = s.Get(b.ID)
	if got.State != Cancelled {
		t.Fatalf("worker purge: %+v", got)
	}
}
