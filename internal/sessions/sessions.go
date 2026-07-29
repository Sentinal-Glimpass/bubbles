// Package sessions holds the fleet's live session table: which bubbles
// currently have a running process, guarded by one mutex.
//
// It is deliberately the table and NOTHING else. It never launches, never
// kills, never measures, and never writes to a PTY — those all belong to the
// kernel, which owns the process table and the runner. The reason is lock
// discipline: Kill and MemBytes can block for an unbounded time (they shell out
// to the OS), and this package's mutex must never be held across one. Keeping
// those operations out of here makes that a structural property rather than a
// convention someone has to remember.
//
// The one exception is Session.Alive(), a cheap non-blocking process check.
// Live evaluates it under the lock; IsHot deliberately evaluates it AFTER
// unlocking. Both match exactly what the kernel did before this table was
// extracted — do not "tidy" IsHot by pulling Alive inside the mutex.
package sessions

import (
	"sync"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
)

// Live is one resident session, captured under the table lock so nothing else
// is done while the table is held.
type Live struct {
	Addr    addr.Address
	Session runner.Session
}

// Table maps bubble addresses to their running sessions.
type Table struct {
	mu sync.Mutex
	m  map[addr.Address]runner.Session
}

// New returns an empty table.
func New() *Table {
	return &Table{m: map[addr.Address]runner.Session{}}
}

// Get returns a's session, or nil if it has none. The session may be dead —
// callers that care ask it, deliberately OUTSIDE this lock.
func (t *Table) Get(a addr.Address) runner.Session {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.m[a]
}

// Set records a's session, replacing any previous one.
func (t *Table) Set(a addr.Address, s runner.Session) {
	t.mu.Lock()
	t.m[a] = s
	t.mu.Unlock()
}

// Delete drops a from the table. It does NOT close or kill anything: the caller
// does that outside the lock.
func (t *Table) Delete(a addr.Address) {
	t.mu.Lock()
	delete(t.m, a)
	t.mu.Unlock()
}

// DeleteAll drops several addresses in one critical section. Used by the paging
// path, where the whole victim set leaves the table before any process is
// killed (killing happens after, outside the lock).
func (t *Table) DeleteAll(addrs []addr.Address) {
	if len(addrs) == 0 {
		return
	}
	t.mu.Lock()
	for _, a := range addrs {
		delete(t.m, a)
	}
	t.mu.Unlock()
}

// Take removes a from the table and hands back whatever session it had, so the
// caller can close it after the lock is released. Returns nil if there was none.
func (t *Table) Take(a addr.Address) runner.Session {
	t.mu.Lock()
	s := t.m[a]
	delete(t.m, a)
	t.mu.Unlock()
	return s
}

// IsHot reports whether a has a live (resident) session. Alive() is asked
// outside the lock — it is a process check, not a table read.
func (t *Table) IsHot(a addr.Address) bool {
	s := t.Get(a)
	return s != nil && s.Alive()
}

// Live snapshots every live non-root worker session. Only the map walk and the
// cheap Alive() check happen under the lock: anything that measures a session
// (MemBytes shells out to the OS) or consults the registry takes its own locks
// and must never nest inside this one, so it is done by the caller afterwards.
func (t *Table) Live() []Live {
	t.mu.Lock()
	defer t.mu.Unlock()
	live := make([]Live, 0, len(t.m))
	for a, s := range t.m {
		if a == addr.Root || s == nil || !s.Alive() {
			continue
		}
		live = append(live, Live{Addr: a, Session: s})
	}
	return live
}
