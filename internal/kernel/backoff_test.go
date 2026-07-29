package kernel

import (
	"sync"
	"testing"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/bus"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
)

// crashLoopFixture builds a kernel with a stubbed clock and one spawned worker.
// The clock is a pointer the test moves by hand: the backoff is a decision about
// elapsed time, and a test that asserted on it by sleeping would be both slow
// and flaky.
type crashLoopFixture struct {
	k    *Kernel
	fr   *runner.FakeRunner
	a    addr.Address
	now  *time.Time
	mu   sync.Mutex
	root []bus.Message
}

func newCrashLoop(t *testing.T) *crashLoopFixture {
	t.Helper()
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0 // keep fresh-launch probe out of the test path
	base := time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC)
	f := &crashLoopFixture{k: k, fr: fr, now: &base}
	k.SetClock(func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		return *f.now
	})
	k.Bus.Subscribe(addr.Root, func(m bus.Message) {
		f.mu.Lock()
		f.root = append(f.root, m)
		f.mu.Unlock()
	})
	a, err := k.Spawn(addr.Root, "scout", "/nonexistent", runner.SpawnOpts{Persona: "scout"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	f.a = a
	return f
}

func (f *crashLoopFixture) advance(d time.Duration) {
	f.mu.Lock()
	*f.now = f.now.Add(d)
	f.mu.Unlock()
}

func (f *crashLoopFixture) rootPings() []bus.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bus.Message(nil), f.root...)
}

// A launch that fails must not be retried on the very next call: that is the
// loop that re-pays a boot context on every sweep.
func TestBackoffSuppressesImmediateRetry(t *testing.T) {
	f := newCrashLoop(t)
	f.fr.FailLaunch = true

	if s := f.k.EnsureAlive(f.a); s != nil {
		t.Fatal("EnsureAlive returned a session for a failing launch")
	}
	attempted := len(f.fr.Launches)
	if attempted == 0 {
		t.Fatal("first EnsureAlive did not attempt a launch")
	}

	if s := f.k.EnsureAlive(f.a); s != nil {
		t.Fatal("EnsureAlive returned a session for a failing launch")
	}
	if len(f.fr.Launches) != attempted {
		t.Fatalf("retried inside the backoff window: launches %d -> %d", attempted, len(f.fr.Launches))
	}

	// Once the window elapses it tries again — backoff delays, it does not park.
	f.advance(f.k.RelaunchBackoff + time.Second)
	f.k.EnsureAlive(f.a)
	if len(f.fr.Launches) == attempted {
		t.Fatal("no relaunch attempted after the backoff window elapsed")
	}
}

// The delay grows per consecutive failure and is capped.
func TestBackoffDelayGrowsAndIsCapped(t *testing.T) {
	k := New(runner.NewFake())
	k.RelaunchBackoff = 10 * time.Second
	k.RelaunchBackoffCap = 25 * time.Second
	want := []time.Duration{0, 10 * time.Second, 20 * time.Second, 25 * time.Second, 25 * time.Second, 25 * time.Second}
	for fails, w := range want {
		if got := k.relaunchDelay(fails); got != w {
			t.Fatalf("relaunchDelay(%d) = %v want %v", fails, got, w)
		}
	}
	// A base already past the cap never exceeds it.
	k.RelaunchBackoff = time.Hour
	if got := k.relaunchDelay(1); got != 25*time.Second {
		t.Fatalf("base above cap: got %v want the cap", got)
	}
}

// The growth is real end to end, not just in the pure helper: the window after
// the second failure is wider than the window after the first.
func TestBackoffWindowWidensAcrossFailures(t *testing.T) {
	f := newCrashLoop(t)
	f.k.RelaunchBackoff = 10 * time.Second
	f.k.RelaunchBackoffCap = time.Hour
	f.fr.FailLaunch = true

	f.k.EnsureAlive(f.a) // failure 1 -> 10s window
	f.advance(11 * time.Second)
	f.k.EnsureAlive(f.a) // failure 2 -> 20s window
	n := len(f.fr.Launches)

	f.advance(11 * time.Second) // enough for the FIRST window, not the second
	f.k.EnsureAlive(f.a)
	if len(f.fr.Launches) != n {
		t.Fatal("second window did not grow: retried after only the first window's delay")
	}
	f.advance(10 * time.Second) // now past 20s
	f.k.EnsureAlive(f.a)
	if len(f.fr.Launches) == n {
		t.Fatal("no retry after the grown window elapsed")
	}
}

// One good launch resets everything: a bubble that failed four times and then
// came up is fully back to normal, counter and delay both.
func TestBackoffSuccessResetsCompletely(t *testing.T) {
	f := newCrashLoop(t)
	f.fr.FailLaunch = true
	for i := 0; i < 4; i++ {
		f.k.EnsureAlive(f.a)
		f.advance(2 * f.k.RelaunchBackoffCap)
	}
	tr, ok := f.k.RelaunchTroubleFor(f.a)
	if !ok || tr.Fails != 4 {
		t.Fatalf("before success: trouble = %+v ok=%v want 4 fails", tr, ok)
	}

	f.fr.FailLaunch = false
	if s := f.k.EnsureAlive(f.a); s == nil {
		t.Fatal("a healthy launch after failures returned no session")
	}
	if tr, ok := f.k.RelaunchTroubleFor(f.a); ok {
		t.Fatalf("success did not clear the failure state: %+v", tr)
	}
	if got := f.k.RelaunchTroubles(); len(got) != 0 {
		t.Fatalf("RelaunchTroubles = %+v want empty", got)
	}

	// And the NEXT failure starts from the base delay again, not from where the
	// old streak left off.
	f.fr.FailLaunch = true
	f.fr.Session(f.a).Die()
	f.k.EnsureAlive(f.a)
	tr, ok = f.k.RelaunchTroubleFor(f.a)
	if !ok || tr.Fails != 1 {
		t.Fatalf("after reset: trouble = %+v ok=%v want 1 fail", tr, ok)
	}
	if want := f.now.Add(f.k.RelaunchBackoff); !tr.NextAttempt.Equal(want) {
		t.Fatalf("NextAttempt = %v want %v (the base delay)", tr.NextAttempt, want)
	}
}

// Past the threshold the bubble stops being relaunched, and that decision is
// reported rather than silent.
func TestBackoffGivesUpAndReports(t *testing.T) {
	f := newCrashLoop(t)
	f.fr.FailLaunch = true
	for i := 0; i < relaunchGiveUpAfter; i++ {
		f.k.EnsureAlive(f.a)
		f.advance(2 * f.k.RelaunchBackoffCap)
	}
	tr, ok := f.k.RelaunchTroubleFor(f.a)
	if !ok || !tr.GaveUp {
		t.Fatalf("trouble = %+v ok=%v want GaveUp", tr, ok)
	}

	// No further relaunch, no matter how much time passes.
	n := len(f.fr.Launches)
	f.advance(72 * time.Hour)
	if s := f.k.EnsureAlive(f.a); s != nil {
		t.Fatal("EnsureAlive relaunched a bubble the fleet gave up on")
	}
	if len(f.fr.Launches) != n {
		t.Fatalf("relaunched after giving up: launches %d -> %d", n, len(f.fr.Launches))
	}

	// Reported: root is pinged exactly once, and the state is queryable.
	pings := f.rootPings()
	if len(pings) != 1 {
		t.Fatalf("root pings = %d want exactly 1 give-up report: %+v", len(pings), pings)
	}
	if pings[0].From != f.a || pings[0].Subject != "relaunch given up" {
		t.Fatalf("give-up ping = %+v", pings[0])
	}
	if got := f.k.RelaunchTroubles(); len(got) != 1 || !got[0].GaveUp {
		t.Fatalf("RelaunchTroubles = %+v want one gave-up entry", got)
	}

	// The operator's reset gesture puts it back in play.
	f.fr.FailLaunch = false
	f.k.SetEnabled(f.a, true)
	if _, ok := f.k.RelaunchTroubleFor(f.a); ok {
		t.Fatal("re-enabling did not clear the give-up")
	}
	if s := f.k.EnsureAlive(f.a); s == nil {
		t.Fatal("bubble did not relaunch after the operator reset it")
	}
}

// FRewarms counts genuine cold-cache wakes and nothing else. Neither a failed
// launch nor a retry the backoff suppressed may touch it.
func TestBackoffDoesNotInflateRewarms(t *testing.T) {
	f := newCrashLoop(t)
	// Give the bubble a real session first, so that later relaunches are the
	// "was paged out" kind that WOULD be metered had they succeeded.
	if s := f.k.EnsureAlive(f.a); s == nil {
		t.Fatal("initial launch failed")
	}
	before := f.k.Cost.Snapshot()[f.a].Rewarms

	f.fr.FailLaunch = true
	f.fr.Session(f.a).Die()
	for i := 0; i < relaunchGiveUpAfter; i++ {
		f.k.EnsureAlive(f.a) // fails
		f.k.EnsureAlive(f.a) // suppressed
		f.advance(2 * f.k.RelaunchBackoffCap)
	}
	if got := f.k.Cost.Snapshot()[f.a].Rewarms; got != before {
		t.Fatalf("Rewarms = %d want %d: failed and suppressed relaunches must count nothing", got, before)
	}
	if got := f.k.Cost.Snapshot()[f.a]; got.Evictions != 0 {
		t.Fatalf("failed relaunches must not meter evictions either: %+v", got)
	}
}

// KeepAlive touches every always-on receiver on every sweep. A crash-looping
// always-on bubble is the worst case, so the sweep must not defeat the counter.
func TestBackoffAppliesToAlwaysOn(t *testing.T) {
	f := newCrashLoop(t)
	f.k.Reg.SetAlwaysOn(f.a, true)
	f.fr.FailLaunch = true

	f.k.KeepAlive() // first sweep: one real attempt
	n := len(f.fr.Launches)
	if n == 0 {
		t.Fatal("KeepAlive did not attempt a launch")
	}
	for i := 0; i < 10; i++ { // ten more sweeps inside the window
		f.k.KeepAlive()
	}
	if len(f.fr.Launches) != n {
		t.Fatalf("KeepAlive defeated the backoff: launches %d -> %d", n, len(f.fr.Launches))
	}

	// And it still reaches the terminal state despite being swept constantly.
	for i := 0; i < relaunchGiveUpAfter; i++ {
		f.advance(2 * f.k.RelaunchBackoffCap)
		f.k.KeepAlive()
	}
	tr, ok := f.k.RelaunchTroubleFor(f.a)
	if !ok || !tr.GaveUp {
		t.Fatalf("always-on bubble never gave up: %+v ok=%v", tr, ok)
	}
	n = len(f.fr.Launches)
	for i := 0; i < 10; i++ {
		f.advance(time.Hour)
		f.k.KeepAlive()
	}
	if len(f.fr.Launches) != n {
		t.Fatalf("KeepAlive kept relaunching a given-up bubble: %d -> %d", n, len(f.fr.Launches))
	}
}

// A healthy bubble must be completely unaffected: no state, no suppression, no
// changed return value.
func TestBackoffLeavesHealthyBubbleAlone(t *testing.T) {
	f := newCrashLoop(t)
	for i := 0; i < 5; i++ {
		if s := f.k.EnsureAlive(f.a); s == nil {
			t.Fatalf("healthy EnsureAlive %d returned nil", i)
		}
		f.fr.Session(f.a).Die()
	}
	if got := f.k.RelaunchTroubles(); len(got) != 0 {
		t.Fatalf("healthy bubble accumulated crash-loop state: %+v", got)
	}
}

// The counter is read before the launch and written after it, with the lock
// released in between. That split is a check-then-act sequence: a round of
// concurrent relaunch attempts for the SAME address must record exactly ONE
// failure between them, or a crash loop would give up N times too early and a
// single unlucky burst of sends would park a healthy bubble. -race cannot see
// this — it is a logic race, not a memory race — so it is asserted directly,
// against the read/act/write protocol ensureAlive uses.
func TestBackoffConcurrentFailuresCountOnce(t *testing.T) {
	f := newCrashLoop(t)
	const callers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	var attempted int64
	var amu sync.Mutex
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			epoch, ok := f.k.relaunchAllowed(f.a) // read, then "launch"
			if !ok {
				return
			}
			amu.Lock()
			attempted++
			amu.Unlock()
			f.k.noteRelaunchFailed(f.a, epoch) // ...write the outcome back
		}()
	}
	close(start)
	wg.Wait()

	if attempted < 2 {
		t.Skip("attempts did not actually overlap; nothing to prove")
	}
	tr, ok := f.k.RelaunchTroubleFor(f.a)
	if !ok {
		t.Fatal("no failure recorded at all")
	}
	if tr.Fails != 1 {
		t.Fatalf("Fails = %d after %d overlapping attempts, want 1: one round is one failure", tr.Fails, attempted)
	}
	if tr.GaveUp {
		t.Fatal("gave up after a single round of concurrent attempts")
	}
}

// A success landing while a failure from the same round is still in flight must
// win: the bubble is demonstrably up, so the stale verdict is dropped rather
// than re-parking a bubble that just recovered.
func TestBackoffStaleFailureAfterSuccessIsDropped(t *testing.T) {
	f := newCrashLoop(t)
	f.fr.FailLaunch = true
	for i := 0; i < 4; i++ { // build a real streak, so there is state to invalidate
		f.k.EnsureAlive(f.a)
		f.advance(2 * f.k.RelaunchBackoffCap)
	}
	epoch, ok := f.k.relaunchAllowed(f.a)
	if !ok {
		t.Fatal("window elapsed but relaunch was not allowed")
	}
	f.k.noteRelaunchSuccess(f.a)       // a concurrent attempt came up first
	f.k.noteRelaunchFailed(f.a, epoch) // the straggler reports its stale failure
	if tr, ok := f.k.RelaunchTroubleFor(f.a); ok {
		t.Fatalf("stale failure recorded over a success: %+v", tr)
	}
}

// Deleting a bubble must forget its crash-loop state. Nothing else can: once the
// registry entry is gone, SetEnabled and ClearRelaunchFailures are unreachable
// for that address and no relaunch can ever succeed to clear it, so a surviving
// entry pins the fleet-health panel red for a bubble that no longer exists.
func TestDeleteBubbleClearsCrashLoopState(t *testing.T) {
	f := newCrashLoop(t)
	child, err := f.k.SpawnUnder(addr.Root, f.a, "helper", "/nonexistent", runner.SpawnOpts{Persona: "helper"})
	if err != nil {
		t.Fatalf("spawn child: %v", err)
	}
	f.fr.FailLaunch = true

	// Both the named bubble and its child pick up crash-loop state.
	f.k.EnsureAlive(f.a)
	f.k.EnsureAlive(child)
	if got := len(f.k.RelaunchTroubles()); got != 2 {
		t.Fatalf("setup: want 2 troubled bubbles, got %d", got)
	}

	f.k.DeleteBubble(f.a)

	if got := f.k.RelaunchTroubles(); len(got) != 0 {
		t.Fatalf("crash-loop state survived DeleteBubble: %+v", got)
	}
	if _, ok := f.k.RelaunchTroubleFor(f.a); ok {
		t.Fatal("deleted bubble still has crash-loop state")
	}
	if _, ok := f.k.RelaunchTroubleFor(child); ok {
		t.Fatal("deleted subtree child still has crash-loop state")
	}
}

// A retry the backoff suppresses must still release the dead session's PTY and
// fds. The gate returns early, so if the close sits behind it the fds leak on
// every suppressed sweep — and leak permanently once the bubble is given up on,
// because no later attempt ever gets past the gate.
func TestSuppressedRetryStillClosesDeadSession(t *testing.T) {
	f := newCrashLoop(t)
	f.fr.FailLaunch = true

	// One failed relaunch puts the address into a suppression window.
	f.k.EnsureAlive(f.a)
	if tr, ok := f.k.RelaunchTroubleFor(f.a); !ok || tr.Fails != 1 {
		t.Fatalf("setup: want one recorded failure, got %+v (%v)", tr, ok)
	}

	// A dead session for the same address, as a crashed process would leave.
	dead := &runner.FakeSession{}
	dead.Die()
	f.k.setSession(f.a, dead)

	if s := f.k.EnsureAlive(f.a); s != nil {
		t.Fatal("EnsureAlive returned a session while suppressed")
	}
	if !dead.Closed() {
		t.Fatal("suppressed retry left the dead session's PTY open")
	}
}

// Every suppression path in this repo increments a costmeter counter, so that a
// decision NOT to act is as visible in the telemetry as an action. The
// crash-loop gate is a suppression path.
func TestSuppressedRelaunchIsMetered(t *testing.T) {
	f := newCrashLoop(t)
	f.fr.FailLaunch = true

	f.k.EnsureAlive(f.a) // failure 1: a real attempt, not a suppression
	if got := f.k.Cost.Snapshot()[f.a].RelaunchesSuppressed; got != 0 {
		t.Fatalf("an attempted relaunch was metered as suppressed (%d)", got)
	}

	f.k.EnsureAlive(f.a) // inside the window: suppressed
	f.k.EnsureAlive(f.a)
	if got := f.k.Cost.Snapshot()[f.a].RelaunchesSuppressed; got != 2 {
		t.Fatalf("RelaunchesSuppressed = %d want 2", got)
	}
	// FRewarms stays untouched: nothing was warmed.
	if got := f.k.Cost.Snapshot()[f.a].Rewarms; got != 0 {
		t.Fatalf("suppression inflated FRewarms (%d)", got)
	}
}
