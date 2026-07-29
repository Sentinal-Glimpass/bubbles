// Package supervisor is a named, panic-safe registry of periodic checks.
//
// It is pure policy and scheduling: it imports nothing from the kernel or the
// TUI and performs no I/O of its own. Callers supply plain
// func(context.Context) error closures, and supply the time — there is no
// time.Now() in this package, so behaviour is fully deterministic in tests.
//
// The reason this package exists: the process runs many background loops, and
// an unrecovered panic in any one of them terminates the whole daemon. RunDue
// recovers per check, so one check panicking neither propagates out of RunDue
// nor prevents the other due checks from running in that same call. The panic
// is never swallowed silently — it is recorded on the check's Status (with the
// check name and the stack) and reported through Snapshot and Failing.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"time"
)

// Check is one named periodic job. Fn must be safe to call concurrently with
// nothing else in this package; the registry never calls the same Check twice
// at once.
type Check struct {
	Name  string
	Every time.Duration
	Fn    func(context.Context) error
}

// Status is the observed outcome of a check's most recent run.
type Status struct {
	Name        string
	LastRun     time.Time
	LastErr     error // nil = last run succeeded
	Consecutive int   // consecutive failures, 0 after a success
	Panicked    bool  // last run panicked (recovered)
	Runs        int64
}

// entry is a registered check plus its schedule and mutable status. All fields
// are guarded by Registry.mu except Fn and the immutable name/interval, which
// are written once at registration.
type entry struct {
	check   Check
	nextDue time.Time
	running bool // a RunDue call currently owns this check
	status  Status
}

// Registry holds the registered checks and their statuses.
type Registry struct {
	mu     sync.Mutex
	checks map[string]*entry
	now    func() time.Time
}

// New returns an empty Registry. now supplies the registry's notion of the
// current time and is used only to seed each check's first due time at
// Register; every subsequent scheduling decision uses the at value passed to
// RunDue. now must not be nil.
func New(now func() time.Time) *Registry {
	if now == nil {
		panic("supervisor.New: now must not be nil")
	}
	return &Registry{checks: make(map[string]*entry), now: now}
}

// Register adds a check. It returns an error on an empty name, a duplicate
// name, a nil Fn, or Every <= 0. The check first becomes due one Every after
// the registry's current time.
func (r *Registry) Register(c Check) error {
	if c.Name == "" {
		return errors.New("supervisor: check name must not be empty")
	}
	if c.Every <= 0 {
		return fmt.Errorf("supervisor: check %q: Every must be > 0, got %v", c.Name, c.Every)
	}
	if c.Fn == nil {
		return fmt.Errorf("supervisor: check %q: Fn must not be nil", c.Name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.checks[c.Name]; dup {
		return fmt.Errorf("supervisor: check %q already registered", c.Name)
	}
	r.checks[c.Name] = &entry{
		check:   c,
		nextDue: r.now().Add(c.Every),
		status:  Status{Name: c.Name},
	}
	return nil
}

// RunDue runs every check whose interval has elapsed as of at, in name order.
//
// The registry lock is released before any check's Fn is invoked: checks do
// I/O and must never run under the lock. Each check runs under its own
// recover, so a panic in one check is recorded and the remaining due checks
// still run. RunDue itself never panics and never returns an error — the
// report is the recorded Status.
//
// RunDue is a no-op if ctx is already done, so a check is not re-run after
// cancellation.
func (r *Registry) RunDue(ctx context.Context, at time.Time) {
	if ctx == nil || ctx.Err() != nil {
		return
	}
	for _, e := range r.claimDue(at) {
		err, panicked := runOne(ctx, e.check)
		r.record(e, at, err, panicked)
	}
}

// claimDue returns the due, not-currently-running checks in name order and
// marks each as running so a concurrent RunDue cannot pick the same one up.
func (r *Registry) claimDue(at time.Time) []*entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	var due []*entry
	for _, e := range r.checks {
		if e.running || at.Before(e.nextDue) {
			continue
		}
		e.running = true
		due = append(due, e)
	}
	sort.Slice(due, func(i, j int) bool { return due[i].check.Name < due[j].check.Name })
	return due
}

// record stores the outcome of one run and re-arms the check's schedule.
func (r *Registry) record(e *entry, at time.Time, err error, panicked bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e.running = false
	e.nextDue = at.Add(e.check.Every)
	e.status.LastRun = at
	e.status.LastErr = err
	e.status.Panicked = panicked
	e.status.Runs++
	if err != nil {
		e.status.Consecutive++
	} else {
		e.status.Consecutive = 0
	}
}

// runOne invokes a check's Fn, converting a panic into an error that carries
// the check name and the stack of the panicking goroutine.
func runOne(ctx context.Context, c Check) (err error, panicked bool) {
	defer func() {
		if v := recover(); v != nil {
			panicked = true
			err = fmt.Errorf("supervisor: check %q panicked: %v\n%s", c.Name, v, stack())
		}
	}()
	return c.Fn(ctx), false
}

// stack renders the current goroutine's stack, growing the buffer as needed.
func stack() []byte {
	buf := make([]byte, 8<<10)
	for {
		n := runtime.Stack(buf, false)
		if n < len(buf) {
			return buf[:n]
		}
		buf = make([]byte, 2*len(buf))
	}
}

// Snapshot returns a copy of every check's status, sorted by name. It is safe
// to call concurrently with RunDue (for instance from the TUI goroutine), and
// mutating the returned slice or its elements cannot affect the registry.
func (r *Registry) Snapshot() []Status {
	r.mu.Lock()
	out := make([]Status, 0, len(r.checks))
	for _, e := range r.checks {
		out = append(out, e.status)
	}
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Failing reports how many checks' most recent run failed or panicked. Checks
// that have never run are not counted.
func (r *Registry) Failing() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.checks {
		if e.status.Runs > 0 && (e.status.LastErr != nil || e.status.Panicked) {
			n++
		}
	}
	return n
}
