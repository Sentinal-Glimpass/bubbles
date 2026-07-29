package main

import (
	"sync"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/health"
	"github.com/Sentinal-Glimpass/bubbles/internal/kernel"
)

// stuckThreshold is how long a hot bubble must sit on unconsumed mail,
// producing byte-identical output, before it is reported as wedged. Five
// minutes is deliberately long: a bubble genuinely thinking through a large
// edit can be quiet for minutes, and the cost of a wrong entry in an operator
// panel is a human disturbing a working bubble.
const stuckThreshold = 5 * time.Minute

// stuckEvery is the sampling cadence. It must be comfortably shorter than
// stuckThreshold so that "unchanged across consecutive samples" is a statement
// about a short window inside a long quiet period, not about the whole of it.
const stuckEvery = 30 * time.Second

// stuckTracker owns the one piece of state the pure detector cannot hold: the
// previous sample set. Each Step takes a fresh snapshot of the already-hot
// bubbles, compares it with the last one, and stores the verdict.
//
// It REPORTS ONLY. Nothing in this file — or in anything reading Stuck() — may
// kill, restart, notify or wake a bubble. The list exists to be rendered.
type stuckTracker struct {
	k   *kernel.Kernel
	cfg health.Config
	now func() time.Time // injectable so tests never sleep

	mu    sync.Mutex
	prev  []health.Sample
	stuck []addr.Address
}

func newStuckTracker(k *kernel.Kernel) *stuckTracker {
	return &stuckTracker{k: k, cfg: health.Config{Threshold: stuckThreshold}, now: time.Now}
}

// Step is the periodic body registered on the supervisor. Sampling only walks
// the live session table (kernel.StuckSamples), so a cold bubble is never paged
// in just to be looked at.
func (s *stuckTracker) Step() {
	cur := s.k.StuckSamples()
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stuck = health.Stuck(s.cfg, s.prev, cur, now)
	s.prev = cur
}

// Stuck returns the most recent verdict. Safe for the TUI goroutine to call.
func (s *stuckTracker) Stuck() []addr.Address {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]addr.Address(nil), s.stuck...)
}
