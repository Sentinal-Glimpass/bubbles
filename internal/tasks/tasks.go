// Package tasks is the assignment ledger: every assign_task lands here, and the
// kernel drives each task through its verification route (worker → checks/
// verifier → assigner). The store is the authoritative task state — completion
// notices typed into terminals are advisory; tasks() reads this.
package tasks

import (
	"fmt"
	"sync"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

// State is a task's position in the verification route.
type State string

const (
	// Open: assigned, the worker is (re)working it. Rejections return here.
	Open State = "open"
	// Checking: submitted; deterministic checks and/or the verifier are running.
	Checking State = "checking"
	// Done: verified and delivered to the assigner.
	Done State = "done"
	// Cancelled: withdrawn by the assigner (or root).
	Cancelled State = "cancelled"
)

// Task is one assignment with its acceptance contract.
type Task struct {
	ID       string       // "t1", "t2", ... (monotonic)
	Assigner addr.Address // who assigned (receives the verified completion)
	Worker   addr.Address // who does the work (the only address that may submit)
	Verifier addr.Address // kernel-spawned checklist judge ("" = deterministic-only task)
	Brief    string       // the charter given to the worker
	CheckCmd string       // shell command that must exit 0 in the worker's dir ("" = none)
	Checklist []string    // items the verifier must confirm (empty = no verifier)
	State    State
	Rounds   int    // reject → resubmit count
	Summary  string // the worker's latest submission summary
}

// Store holds all tasks, queried by participant.
type Store struct {
	mu  sync.Mutex
	seq int
	ver int64
	all []*Task
}

func New() *Store { return &Store{} }

// Version increments on every change, so a periodic saver can skip idle writes.
func (s *Store) Version() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ver
}

// Snapshot returns a copy of all tasks and the ID sequence, for persistence.
func (s *Store) Snapshot() ([]Task, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Task, len(s.all))
	for i, t := range s.all {
		out[i] = *t
	}
	return out, s.seq
}

// Load replaces the store from a saved snapshot (restart restore). The sequence
// continues so restored task IDs are never reused.
func (s *Store) Load(ts []Task, seq int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.all = make([]*Task, len(ts))
	for i := range ts {
		cp := ts[i]
		s.all[i] = &cp
		var n int
		if _, err := fmt.Sscanf(cp.ID, "t%d", &n); err == nil && n > seq {
			seq = n
		}
	}
	s.seq = seq
	s.ver++
}

// Create files a new open task and returns it (ID assigned).
func (s *Store) Create(t Task) Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	s.ver++
	t.ID = fmt.Sprintf("t%d", s.seq)
	t.State = Open
	cp := t
	s.all = append(s.all, &cp)
	return t
}

// Get returns a copy of the task by ID.
func (s *Store) Get(id string) (Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.find(id); t != nil {
		return *t, true
	}
	return Task{}, false
}

func (s *Store) find(id string) *Task {
	for _, t := range s.all {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// SetState moves a task; returns false if the task doesn't exist.
func (s *Store) SetState(id string, st State) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.find(id)
	if t == nil {
		return false
	}
	t.State = st
	s.ver++
	return true
}

// Reject bounces a submitted task back to the worker: state → open, rounds++.
func (s *Store) Reject(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.find(id)
	if t == nil {
		return false
	}
	t.State = Open
	t.Rounds++
	s.ver++
	return true
}

// SetSummary records the worker's latest submission text.
func (s *Store) SetSummary(id, summary string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.find(id); t != nil {
		t.Summary = summary
		s.ver++
	}
}

// SetVerifier records the kernel-spawned verifier's address.
func (s *Store) SetVerifier(id string, v addr.Address) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.find(id); t != nil {
		t.Verifier = v
		s.ver++
	}
}

// OpenBetween returns the ID of an open/checking task where worker is the
// Worker and assigner is the Assigner ("" if none) — the Send-annotation hook.
func (s *Store) OpenBetween(worker, assigner addr.Address) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.all {
		if t.Worker == worker && t.Assigner == assigner && (t.State == Open || t.State == Checking) {
			return t.ID
		}
	}
	return ""
}

// For returns copies of every task the address participates in (assigner,
// worker, or verifier), oldest first.
func (s *Store) For(a addr.Address) []Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Task
	for _, t := range s.all {
		if t.Assigner == a || t.Worker == a || t.Verifier == a {
			out = append(out, *t)
		}
	}
	return out
}

// Active returns copies of tasks still in flight WITH a live verifier bubble
// (state open/checking), oldest first — drives the TUI's task section, which
// empties as verifiers are reaped on completion.
func (s *Store) Active() []Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Task
	for _, t := range s.all {
		if t.Verifier != "" && (t.State == Open || t.State == Checking) {
			out = append(out, *t)
		}
	}
	return out
}

// PurgeParticipant cancels open tasks involving a deleted bubble so the route
// never waits on an address that no longer exists. Returns affected task IDs.
func (s *Store) PurgeParticipant(a addr.Address) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for _, t := range s.all {
		if t.State != Open && t.State != Checking {
			continue
		}
		if t.Assigner == a || t.Worker == a {
			t.State = Cancelled
			ids = append(ids, t.ID)
			s.ver++
		} else if t.Verifier == a {
			t.Verifier = "" // verifier died: task degrades to deterministic-only
			s.ver++
		}
	}
	return ids
}
