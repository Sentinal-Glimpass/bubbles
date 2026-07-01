package runner

import (
	"sync"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

// FakeSession records everything written to it (for tests).
type FakeSession struct {
	mu      sync.Mutex
	written []byte
	closed  bool
	dead    bool // simulate a crashed process (Alive() -> false)
}

func (s *FakeSession) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.written = append(s.written, p...)
	return len(p), nil
}

func (s *FakeSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *FakeSession) Written() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.written)
}

func (s *FakeSession) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Alive reports whether the (simulated) process is still running.
func (s *FakeSession) Alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed && !s.dead
}

// Die simulates the process crashing (test helper).
func (s *FakeSession) Die() {
	s.mu.Lock()
	s.dead = true
	s.mu.Unlock()
}

// FakeLaunch records a Launch call (test introspection).
type FakeLaunch struct {
	Addr addr.Address
	Dir  string
	Opts SpawnOpts
}

// FakeRunner is an in-memory Runner for tests — no real processes.
type FakeRunner struct {
	mu         sync.Mutex
	sessions   map[addr.Address]*FakeSession
	FailResume bool         // when true, a Launch with Resume=true yields a dead session (simulates a missing session id)
	Launches   []FakeLaunch // every Launch call, in order
}

func NewFake() *FakeRunner {
	return &FakeRunner{sessions: map[addr.Address]*FakeSession{}}
}

func (r *FakeRunner) Launch(a addr.Address, dir string, opts SpawnOpts) (Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Launches = append(r.Launches, FakeLaunch{Addr: a, Dir: dir, Opts: opts})
	s := &FakeSession{}
	if opts.Resume && r.FailResume {
		s.dead = true // the resumed conversation no longer exists -> process exits at once
	}
	r.sessions[a] = s
	return s, nil
}

func (r *FakeRunner) Kill(a addr.Address) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[a]; ok {
		s.closed = true
	}
	return nil
}

// Session returns the FakeSession launched for a (test helper).
func (r *FakeRunner) Session(a addr.Address) *FakeSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[a]
}
