package runner

import (
	"sync"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

// FakeSession records everything written to it (for tests).
type FakeSession struct {
	mu       sync.Mutex
	written  []byte
	closed   bool
	dead     bool          // simulate a crashed process (Alive() -> false)
	mem      uint64        // simulated resident memory (tests set it)
	cpu      time.Duration // simulated cumulative CPU (tests set it)
	lastAct  time.Time     // simulated last-activity stamp (tests set it)
	output   string        // simulated recent output (tests set it)
	notReady bool          // simulate a session that hasn't rendered its input yet
}

// RecentOutput returns the simulated recent output.
func (s *FakeSession) RecentOutput() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.output
}

// SetOutput sets the simulated recent output (test helper).
func (s *FakeSession) SetOutput(o string) {
	s.mu.Lock()
	s.output = o
	s.mu.Unlock()
}

// InputReady: fake sessions are ready by default (no startup menu in tests);
// SetInputReady(false) simulates one still booting, so the deferred-write path
// can be exercised.
func (s *FakeSession) InputReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.notReady
}

// SetInputReady sets whether the simulated session accepts input (test
// helper). Default is ready, so existing tests are unaffected.
func (s *FakeSession) SetInputReady(ready bool) {
	s.mu.Lock()
	s.notReady = !ready
	s.mu.Unlock()
}

// CPUTime returns the simulated cumulative CPU.
func (s *FakeSession) CPUTime() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cpu
}

// SetCPU sets the simulated cumulative CPU (test helper).
func (s *FakeSession) SetCPU(d time.Duration) {
	s.mu.Lock()
	s.cpu = d
	s.mu.Unlock()
}

// LastActivity returns the simulated last-activity time (defaults to now).
func (s *FakeSession) LastActivity() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAct
}

// SetLastActivity sets the simulated last-activity time (test helper).
func (s *FakeSession) SetLastActivity(t time.Time) {
	s.mu.Lock()
	s.lastAct = t
	s.mu.Unlock()
}

// MemBytes returns the simulated resident memory.
func (s *FakeSession) MemBytes() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mem
}

// SetMem sets the simulated resident memory (test helper).
func (s *FakeSession) SetMem(n uint64) {
	s.mu.Lock()
	s.mem = n
	s.mu.Unlock()
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
	LostResume bool         // when true, a Launch with Resume=true stays alive but reports "No conversation found" (claude has no record of the id)
	Launches   []FakeLaunch // every Launch call, in order
}

func NewFake() *FakeRunner {
	return &FakeRunner{sessions: map[addr.Address]*FakeSession{}}
}

func (r *FakeRunner) Launch(a addr.Address, dir string, opts SpawnOpts) (Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Launches = append(r.Launches, FakeLaunch{Addr: a, Dir: dir, Opts: opts})
	s := &FakeSession{lastAct: time.Now()} // fresh sessions look active until a test ages them
	if opts.Resume && r.FailResume {
		s.dead = true // the resumed conversation no longer exists -> process exits at once
	}
	if opts.Resume && r.LostResume {
		s.output = "No conversation found with session ID: " + opts.SessionID // claude has no record of it
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
