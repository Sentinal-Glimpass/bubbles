// Package sched is the fleet's durable wake scheduler: time-triggered messages
// that the always-alive daemon delivers to a target bubble (waking it if cold),
// so a bubble can be an event-driven worker without a permanently-awake session.
package sched

import (
	"crypto/rand"
	"fmt"
	"sync"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

// Trigger is when a schedule fires: a repeating Interval, OR a daily clock time
// (DailyMin = minutes past local midnight, used when Interval == 0).
type Trigger struct {
	Interval time.Duration `json:"interval,omitempty"`
	DailyMin int           `json:"dailyMin,omitempty"`
}

// Next returns the first fire time strictly after `after`.
func (t Trigger) Next(after time.Time) time.Time {
	if t.Interval > 0 {
		return after.Add(t.Interval)
	}
	h, m := t.DailyMin/60, t.DailyMin%60
	n := time.Date(after.Year(), after.Month(), after.Day(), h, m, 0, 0, after.Location())
	if !n.After(after) {
		n = n.Add(24 * time.Hour)
	}
	return n
}

// String renders the trigger for display ("every 15m0s" / "daily 08:00").
func (t Trigger) String() string {
	if t.Interval > 0 {
		return "every " + t.Interval.String()
	}
	return fmt.Sprintf("daily %02d:%02d", t.DailyMin/60, t.DailyMin%60)
}

// ParseTrigger builds a Trigger from an "every" duration ("15m", "2h") or a
// "daily" clock time ("08:00", "23:30"). Exactly one must be set.
func ParseTrigger(every, daily string) (Trigger, error) {
	if every != "" && daily != "" {
		return Trigger{}, fmt.Errorf("set only one of every/daily")
	}
	if every != "" {
		d, err := time.ParseDuration(every)
		if err != nil {
			return Trigger{}, fmt.Errorf("bad interval %q: %w", every, err)
		}
		if d < 30*time.Second {
			return Trigger{}, fmt.Errorf("interval must be >= 30s")
		}
		return Trigger{Interval: d}, nil
	}
	if daily != "" {
		var h, m int
		if _, err := fmt.Sscanf(daily, "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			return Trigger{}, fmt.Errorf("bad daily time %q (want HH:MM)", daily)
		}
		return Trigger{DailyMin: h*60 + m}, nil
	}
	return Trigger{}, fmt.Errorf("need every=<dur> or daily=HH:MM")
}

// Schedule is one durable wake.
type Schedule struct {
	ID       string       `json:"id"`
	From     addr.Address `json:"from"`   // creator (authority / who the wake is "from")
	Target   addr.Address `json:"target"` // bubble to wake
	Subject  string       `json:"subject"`
	Body     string       `json:"body,omitempty"`
	Urgent   bool         `json:"urgent,omitempty"`
	Trigger  Trigger      `json:"trigger"`
	NextFire time.Time    `json:"nextFire"`
	Created  time.Time    `json:"created"`
}

// Store holds all schedules.
type Store struct {
	mu  sync.Mutex
	all map[string]*Schedule
	ver int64
}

func New() *Store { return &Store{all: map[string]*Schedule{}} }

// Version increments on every change (for change-gated persistence).
func (s *Store) Version() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ver
}

// Add records a schedule, computing its first fire from now.
func (s *Store) Add(now time.Time, from, target addr.Address, subject, body string, trig Trigger, urgent bool) *Schedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc := &Schedule{
		ID: newID(), From: from, Target: target, Subject: subject, Body: body,
		Urgent: urgent, Trigger: trig, NextFire: trig.Next(now), Created: now,
	}
	s.all[sc.ID] = sc
	s.ver++
	return sc
}

func (s *Store) Remove(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.all[id]; ok {
		delete(s.all, id)
		s.ver++
		return true
	}
	return false
}

func (s *Store) Get(id string) (Schedule, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, ok := s.all[id]
	if !ok {
		return Schedule{}, false
	}
	return *sc, true
}

func (s *Store) All() []Schedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Schedule, 0, len(s.all))
	for _, sc := range s.all {
		out = append(out, *sc)
	}
	return out
}

// Due returns every schedule whose NextFire has passed, advancing each to its
// next future fire (so a long daemon outage fires a schedule once on catch-up,
// not once per missed tick).
func (s *Store) Due(now time.Time) []Schedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	var due []Schedule
	for _, sc := range s.all {
		if !sc.NextFire.After(now) {
			due = append(due, *sc)
			sc.NextFire = sc.Trigger.Next(now)
			s.ver++
		}
	}
	return due
}

// PurgeBubble drops schedules that target or were created by a (called when a
// bubble is deleted, so dead schedules don't linger).
func (s *Store) PurgeBubble(a addr.Address) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sc := range s.all {
		if sc.Target == a || sc.From == a {
			delete(s.all, id)
			s.ver++
		}
	}
}

// Snapshot / Load persist the store across restarts.
func (s *Store) Snapshot() []Schedule { return s.All() }

func (s *Store) Load(scs []Schedule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.all = make(map[string]*Schedule, len(scs))
	for i := range scs {
		cp := scs[i]
		s.all[cp.ID] = &cp
	}
	s.ver++
}

func newID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("s-%x", b)
}
