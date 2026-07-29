package supervisor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

var base = time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

// fixed returns a clock stuck at base, so nothing in these tests depends on
// the wall clock.
func fixed() func() time.Time { return func() time.Time { return base } }

// mustRegister fails the test if the check cannot be registered.
func mustRegister(t *testing.T, r *Registry, c Check) {
	t.Helper()
	if err := r.Register(c); err != nil {
		t.Fatalf("Register(%q): %v", c.Name, err)
	}
}

// statusOf returns the named check's status from a snapshot.
func statusOf(t *testing.T, r *Registry, name string) Status {
	t.Helper()
	for _, s := range r.Snapshot() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no status for check %q", name)
	return Status{}
}

func TestRunDueRunsOnlyWhenDue(t *testing.T) {
	r := New(fixed())
	var runs int
	mustRegister(t, r, Check{Name: "a", Every: 10 * time.Second, Fn: func(context.Context) error {
		runs++
		return nil
	}})

	// Not yet due: registered at base, first due at base+10s.
	r.RunDue(context.Background(), base.Add(9*time.Second))
	if runs != 0 {
		t.Fatalf("check ran before it was due: runs=%d", runs)
	}

	r.RunDue(context.Background(), base.Add(10*time.Second))
	if runs != 1 {
		t.Fatalf("check did not run when due: runs=%d", runs)
	}

	// Re-armed relative to the run time; not due again until +10s.
	r.RunDue(context.Background(), base.Add(19*time.Second))
	if runs != 1 {
		t.Fatalf("check re-ran before next due: runs=%d", runs)
	}
	r.RunDue(context.Background(), base.Add(20*time.Second))
	if runs != 2 {
		t.Fatalf("check did not run at next due time: runs=%d", runs)
	}

	got := statusOf(t, r, "a")
	if got.Runs != 2 || got.LastErr != nil || got.Panicked {
		t.Fatalf("unexpected status: %+v", got)
	}
	if !got.LastRun.Equal(base.Add(20 * time.Second)) {
		t.Fatalf("LastRun = %v, want %v", got.LastRun, base.Add(20*time.Second))
	}
}

// TestPanicIsIsolated is the whole point of this package: a panicking check
// must neither propagate out of RunDue nor stop the other due checks from
// running in that same RunDue call.
func TestPanicIsIsolated(t *testing.T) {
	r := New(fixed())
	// Names chosen so the panicking check sorts in the middle: checks both
	// before and after it must still run.
	var beforeRuns, afterRuns int
	mustRegister(t, r, Check{Name: "a-before", Every: time.Second, Fn: func(context.Context) error {
		beforeRuns++
		return nil
	}})
	mustRegister(t, r, Check{Name: "m-boom", Every: time.Second, Fn: func(context.Context) error {
		panic("kaboom")
	}})
	mustRegister(t, r, Check{Name: "z-after", Every: time.Second, Fn: func(context.Context) error {
		afterRuns++
		return nil
	}})

	// If the panic propagated, the test binary would die here.
	r.RunDue(context.Background(), base.Add(time.Second))

	if beforeRuns != 1 {
		t.Errorf("check sorting before the panicking one ran %d times, want 1", beforeRuns)
	}
	if afterRuns != 1 {
		t.Errorf("check sorting after the panicking one ran %d times, want 1", afterRuns)
	}

	boom := statusOf(t, r, "m-boom")
	if !boom.Panicked {
		t.Errorf("Panicked = false, want true")
	}
	if boom.LastErr == nil {
		t.Fatalf("LastErr = nil, want the recovered panic")
	}
	if boom.Consecutive != 1 || boom.Runs != 1 {
		t.Errorf("Consecutive=%d Runs=%d, want 1 and 1", boom.Consecutive, boom.Runs)
	}
	msg := boom.LastErr.Error()
	if !strings.Contains(msg, "m-boom") {
		t.Errorf("recorded error %q does not name the check", msg)
	}
	if !strings.Contains(msg, "kaboom") {
		t.Errorf("recorded error %q does not carry the panic value", msg)
	}
	if !strings.Contains(msg, "supervisor.runOne") {
		t.Errorf("recorded error %q does not carry a stack", msg)
	}

	// The healthy checks are untouched by their neighbour's panic.
	if s := statusOf(t, r, "a-before"); s.LastErr != nil || s.Panicked {
		t.Errorf("healthy check contaminated: %+v", s)
	}
	if r.Failing() != 1 {
		t.Errorf("Failing() = %d, want 1", r.Failing())
	}
}

func TestPanicThenSuccessClearsStatus(t *testing.T) {
	r := New(fixed())
	fail := true
	mustRegister(t, r, Check{Name: "flaky", Every: time.Second, Fn: func(context.Context) error {
		if fail {
			panic("first run explodes")
		}
		return nil
	}})

	r.RunDue(context.Background(), base.Add(time.Second))
	if s := statusOf(t, r, "flaky"); !s.Panicked || s.Consecutive != 1 {
		t.Fatalf("after panic: %+v", s)
	}

	fail = false
	r.RunDue(context.Background(), base.Add(2*time.Second))
	s := statusOf(t, r, "flaky")
	if s.Panicked {
		t.Errorf("Panicked not cleared after a good run")
	}
	if s.LastErr != nil {
		t.Errorf("LastErr = %v, want nil", s.LastErr)
	}
	if s.Consecutive != 0 {
		t.Errorf("Consecutive = %d, want 0", s.Consecutive)
	}
	if r.Failing() != 0 {
		t.Errorf("Failing() = %d, want 0", r.Failing())
	}
}

func TestErrorAccumulatesAndResets(t *testing.T) {
	r := New(fixed())
	boom := errors.New("nope")
	var err error = boom
	mustRegister(t, r, Check{Name: "e", Every: time.Second, Fn: func(context.Context) error { return err }})

	for i := 1; i <= 3; i++ {
		r.RunDue(context.Background(), base.Add(time.Duration(i)*time.Second))
		s := statusOf(t, r, "e")
		if s.Consecutive != i {
			t.Fatalf("after %d failures Consecutive = %d", i, s.Consecutive)
		}
		if !errors.Is(s.LastErr, boom) {
			t.Fatalf("LastErr = %v, want %v", s.LastErr, boom)
		}
		if s.Panicked {
			t.Fatalf("a returned error must not be reported as a panic")
		}
	}
	if r.Failing() != 1 {
		t.Errorf("Failing() = %d, want 1", r.Failing())
	}

	err = nil
	r.RunDue(context.Background(), base.Add(4*time.Second))
	s := statusOf(t, r, "e")
	if s.Consecutive != 0 || s.LastErr != nil {
		t.Errorf("success did not reset: %+v", s)
	}
	if s.Runs != 4 {
		t.Errorf("Runs = %d, want 4", s.Runs)
	}
	if r.Failing() != 0 {
		t.Errorf("Failing() = %d, want 0", r.Failing())
	}
}

func TestRegisterRejectsBadChecks(t *testing.T) {
	ok := func(context.Context) error { return nil }
	r := New(fixed())
	mustRegister(t, r, Check{Name: "dup", Every: time.Second, Fn: ok})

	cases := []struct {
		name string
		c    Check
	}{
		{"duplicate name", Check{Name: "dup", Every: time.Second, Fn: ok}},
		{"empty name", Check{Name: "", Every: time.Second, Fn: ok}},
		{"zero interval", Check{Name: "zero", Every: 0, Fn: ok}},
		{"negative interval", Check{Name: "neg", Every: -time.Second, Fn: ok}},
		{"nil Fn", Check{Name: "nilfn", Every: time.Second, Fn: nil}},
	}
	for _, tc := range cases {
		if err := r.Register(tc.c); err == nil {
			t.Errorf("Register(%s) = nil, want an error", tc.name)
		}
	}
	if got := len(r.Snapshot()); got != 1 {
		t.Errorf("rejected checks were registered anyway: %d entries", got)
	}
}

func TestFailingCountsOnlyFailingChecks(t *testing.T) {
	r := New(fixed())
	mustRegister(t, r, Check{Name: "good", Every: time.Second, Fn: func(context.Context) error { return nil }})
	mustRegister(t, r, Check{Name: "bad", Every: time.Second, Fn: func(context.Context) error { return errors.New("x") }})
	mustRegister(t, r, Check{Name: "panicky", Every: time.Second, Fn: func(context.Context) error { panic("p") }})
	// Never becomes due within this test, so it never runs.
	mustRegister(t, r, Check{Name: "never", Every: time.Hour, Fn: func(context.Context) error { return errors.New("unreached") }})

	if r.Failing() != 0 {
		t.Fatalf("Failing() before any run = %d, want 0", r.Failing())
	}
	r.RunDue(context.Background(), base.Add(time.Second))
	if got := r.Failing(); got != 2 {
		t.Errorf("Failing() = %d, want 2 (bad + panicky)", got)
	}
	if s := statusOf(t, r, "never"); s.Runs != 0 {
		t.Errorf("check that was not due ran: %+v", s)
	}
}

func TestSnapshotSortedAndCopied(t *testing.T) {
	r := New(fixed())
	for _, n := range []string{"charlie", "alpha", "bravo"} {
		mustRegister(t, r, Check{Name: n, Every: time.Second, Fn: func(context.Context) error { return nil }})
	}
	r.RunDue(context.Background(), base.Add(time.Second))

	snap := r.Snapshot()
	want := []string{"alpha", "bravo", "charlie"}
	if len(snap) != len(want) {
		t.Fatalf("Snapshot() has %d entries, want %d", len(snap), len(want))
	}
	for i, n := range want {
		if snap[i].Name != n {
			t.Fatalf("Snapshot()[%d].Name = %q, want %q", i, snap[i].Name, n)
		}
	}

	// Mutating the result must not reach the registry.
	snap[0].Name = "mutated"
	snap[0].Runs = 999
	snap[0].Consecutive = 42
	snap[0].Panicked = true
	snap[0].LastErr = errors.New("injected")

	again := r.Snapshot()
	if again[0].Name != "alpha" || again[0].Runs != 1 || again[0].Consecutive != 0 ||
		again[0].Panicked || again[0].LastErr != nil {
		t.Fatalf("mutating a Snapshot affected the registry: %+v", again[0])
	}
	if r.Failing() != 0 {
		t.Errorf("Failing() = %d after mutating a snapshot, want 0", r.Failing())
	}
}

func TestCancelledContextStopsFurtherRuns(t *testing.T) {
	r := New(fixed())
	var runs int
	ctx, cancel := context.WithCancel(context.Background())
	mustRegister(t, r, Check{Name: "c", Every: time.Second, Fn: func(ctx context.Context) error {
		runs++
		return ctx.Err()
	}})

	r.RunDue(ctx, base.Add(time.Second))
	if runs != 1 {
		t.Fatalf("runs = %d, want 1", runs)
	}

	cancel()
	r.RunDue(ctx, base.Add(2*time.Second))
	r.RunDue(ctx, base.Add(3*time.Second))
	if runs != 1 {
		t.Errorf("check re-ran after the context was done: runs = %d", runs)
	}
	if s := statusOf(t, r, "c"); s.Runs != 1 {
		t.Errorf("Runs = %d, want 1", s.Runs)
	}
}

// TestFnDoesNotRunUnderRegistryLock proves the lock is released before Fn is
// invoked: a check that calls back into the registry would deadlock otherwise.
func TestFnDoesNotRunUnderRegistryLock(t *testing.T) {
	r := New(fixed())
	done := make(chan struct{})
	mustRegister(t, r, Check{Name: "reentrant", Every: time.Second, Fn: func(context.Context) error {
		_ = r.Snapshot()
		_ = r.Failing()
		close(done)
		return nil
	}})

	go r.RunDue(context.Background(), base.Add(time.Second))
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Fn appears to run under the registry lock: deadlocked")
	}
}

// TestConcurrentSnapshotDuringRun is the race-detector's target: the TUI
// goroutine reads statuses while checks run.
func TestConcurrentSnapshotDuringRun(t *testing.T) {
	r := New(fixed())
	release := make(chan struct{})
	mustRegister(t, r, Check{Name: "slow", Every: time.Second, Fn: func(context.Context) error {
		<-release
		return errors.New("done")
	}})
	mustRegister(t, r, Check{Name: "panicky", Every: time.Second, Fn: func(context.Context) error {
		panic("concurrent boom")
	}})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.RunDue(context.Background(), base.Add(time.Second))
	}()
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.Snapshot()
			_ = r.Failing()
		}()
	}
	close(release)
	wg.Wait()

	if got := r.Failing(); got != 2 {
		t.Errorf("Failing() = %d, want 2", got)
	}
}

// TestConcurrentRunDueDoesNotDoubleRun checks the registry never invokes the
// same Check twice at once.
func TestConcurrentRunDueDoesNotDoubleRun(t *testing.T) {
	r := New(fixed())
	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	gate := make(chan struct{})
	mustRegister(t, r, Check{Name: "once", Every: time.Second, Fn: func(context.Context) error {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		<-gate
		mu.Lock()
		inFlight--
		mu.Unlock()
		return nil
	}})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.RunDue(context.Background(), base.Add(time.Second))
		}()
	}
	close(gate)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if maxInFlight > 1 {
		t.Errorf("same check ran %d times concurrently, want at most 1", maxInFlight)
	}
}
