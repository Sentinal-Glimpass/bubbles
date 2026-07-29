package kernel

import (
	"testing"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/costmeter"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
)

// TestCacheTTLSparesWarmIdleSession: past IdleTimeout is not, on its own, a
// reason to kill a session whose prompt cache is still alive — with no memory
// pressure demanding the RAM, that eviction converts a free wake into a paid
// one. Inside CacheTTL the bubble stays hot; past it, it pages out as before.
func TestCacheTTLSparesWarmIdleSession(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	k.IdleTimeout = 10 * time.Minute
	k.CacheTTL = 30 * time.Minute

	warm, _ := k.Spawn(addr.Root, "warm", "/tmp/w", runner.SpawnOpts{Name: "warm"})
	cold, _ := k.Spawn(addr.Root, "cold", "/tmp/c", runner.SpawnOpts{Name: "cold"})
	k.EnsureAlive(warm)
	k.EnsureAlive(cold)
	fr.Session(warm).SetLastActivity(time.Now().Add(-15 * time.Minute)) // past IdleTimeout, cache still warm
	fr.Session(cold).SetLastActivity(time.Now().Add(-45 * time.Minute)) // cache long dead

	k.EvictIdle()
	if !k.IsHot(warm) {
		t.Fatal("a session idle less than CacheTTL still holds a live prompt cache and must not be idle-evicted")
	}
	if k.IsHot(cold) {
		t.Fatal("a session idle past CacheTTL costs nothing to evict and should page out")
	}
}

// TestCacheTTLZeroKeepsTodaysBehaviour: CacheTTL == 0 disables the protection
// entirely, so idle eviction is exactly the plain IdleTimeout rule it was
// before. This is the pin that says the new field is opt-in.
func TestCacheTTLZeroKeepsTodaysBehaviour(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	k.IdleTimeout = 10 * time.Minute
	k.CacheTTL = 0

	a, _ := k.Spawn(addr.Root, "a", "/tmp/a", runner.SpawnOpts{Name: "a"})
	k.EnsureAlive(a)
	fr.Session(a).SetLastActivity(time.Now().Add(-15 * time.Minute))

	k.EvictIdle()
	if k.IsHot(a) {
		t.Fatal("with CacheTTL == 0 an idle session must page out exactly as before")
	}
}

// budgetKernel builds a kernel with n hot worker bubbles, each with the given
// resident size and the same last-activity stamp, so eviction tests vary ONE
// thing (rewarm cost) and never accidentally trade on idleness.
func budgetKernel(t *testing.T, mem uint64, names ...string) (*runner.FakeRunner, *Kernel, []addr.Address) {
	t.Helper()
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	same := time.Now().Add(-time.Minute)
	var out []addr.Address
	for _, n := range names {
		a, _ := k.Spawn(addr.Root, n, "/tmp/"+n, runner.SpawnOpts{Name: n})
		k.EnsureAlive(a)
		fr.Session(a).SetMem(mem)
		fr.Session(a).SetLastActivity(same)
		out = append(out, a)
	}
	return fr, k, out
}

// TestEnforceBudgetEvictsCheapestToRewarm: at equal idleness and equal size,
// the bubble that costs least to rewarm goes first. Under plain LRU the choice
// was arbitrary, and half the time it killed the 500k-token session whose
// rewarm dwarfs the RAM it gave back.
func TestEnforceBudgetEvictsCheapestToRewarm(t *testing.T) {
	_, k, as := budgetKernel(t, 600, "rich", "poor")
	rich, poor := as[0], as[1]
	k.MemBudget = 1000 // 1200 resident: one of them must go
	k.CacheTTL = 30 * time.Minute
	k.Cost.Set(rich, costmeter.FContextTokens, 500_000)
	k.Cost.Set(poor, costmeter.FContextTokens, 1_000)

	k.EnforceBudget()
	if k.IsHot(poor) {
		t.Fatal("the cheap-to-rewarm bubble should have been paged out first")
	}
	if !k.IsHot(rich) {
		t.Fatal("the expensive-to-rewarm bubble should have been spared")
	}
	if got := k.Cost.Snapshot()[poor].Evictions; got != 1 {
		t.Fatalf("evictions for the paged-out bubble = %d, want 1", got)
	}
}

// TestEnforceBudgetStillEvictsWhenEverythingIsExpensive: cost decides WHO goes,
// never HOW MANY. A budget is not negotiable just because every candidate has
// a warm cache.
func TestEnforceBudgetStillEvictsWhenEverythingIsExpensive(t *testing.T) {
	_, k, as := budgetKernel(t, 600, "a", "b")
	k.MemBudget = 1000
	k.CacheTTL = 30 * time.Minute
	for _, a := range as {
		k.Cost.Set(a, costmeter.FContextTokens, 900_000) // all warm, all costly
	}

	k.EnforceBudget()
	hot := 0
	for _, a := range as {
		if k.IsHot(a) {
			hot++
		}
	}
	if hot != 1 {
		t.Fatalf("hot sessions after enforcement = %d, want 1 (budget must still be met)", hot)
	}
}

// TestEnforceBudgetAggressivenessUnchanged: with CacheTTL == 0 the policy must
// evict exactly as many bubbles as the old LRU did — only the ORDER may differ.
// This is the pin against cost-awareness quietly becoming cost-timidity.
func TestEnforceBudgetAggressivenessUnchanged(t *testing.T) {
	for _, ttl := range []time.Duration{0, 30 * time.Minute} {
		_, k, as := budgetKernel(t, 600, "a", "b", "c") // 1800 resident
		k.MemBudget = 1000                              // must fall to <= 1000: two of the three go
		k.CacheTTL = ttl
		k.Cost.Set(as[0], costmeter.FContextTokens, 400_000)
		k.Cost.Set(as[2], costmeter.FContextTokens, 5_000)

		k.EnforceBudget()
		hot := 0
		for _, a := range as {
			if k.IsHot(a) {
				hot++
			}
		}
		if hot != 1 {
			t.Fatalf("CacheTTL=%s: hot sessions = %d, want 1", ttl, hot)
		}
	}
}

// TestEnforceBudgetExemptsAlwaysOn: an always-on receiver is never paged out
// for budget, and its RAM is not budgeted against the workers — pre-existing
// law that routing through the policy must not quietly repeal.
func TestEnforceBudgetExemptsAlwaysOn(t *testing.T) {
	_, k, as := budgetKernel(t, 600, "recv", "worker")
	recv, worker := as[0], as[1]
	k.Reg.SetAlwaysOn(recv, true)
	k.MemBudget = 1000

	k.EnforceBudget()
	if !k.IsHot(recv) {
		t.Fatal("an always-on receiver must never be paged out for budget")
	}
	if !k.IsHot(worker) {
		t.Fatal("the worker alone (600) fits the budget: always-on RAM is not budgeted against it")
	}
}

// TestEvictIdleCountsEvictions: every page-out is metered, or a paging policy
// change is unfalsifiable from the outside.
func TestEvictIdleCountsEvictions(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	k.IdleTimeout = 10 * time.Minute

	a, _ := k.Spawn(addr.Root, "a", "/tmp/a", runner.SpawnOpts{Name: "a"})
	k.EnsureAlive(a)
	fr.Session(a).SetLastActivity(time.Now().Add(-time.Hour))

	k.EvictIdle()
	if got := k.Cost.Snapshot()[a].Evictions; got != 1 {
		t.Fatalf("evictions after an idle page-out = %d, want 1", got)
	}
}

// TestRewarmCountedOnlyForPagedOutBubbles: a rewarm is a relaunch of a bubble
// that HAD been running and was paged out — that is the launch which re-pays
// uncached input for the whole conversation. A bubble's first-ever launch has
// no cache to have lost and must not be counted, or the metric the phase is
// judged by inflates with every spawn.
func TestRewarmCountedOnlyForPagedOutBubbles(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	k.IdleTimeout = 10 * time.Minute

	a, _ := k.Spawn(addr.Root, "a", "/tmp/a", runner.SpawnOpts{Name: "a"})
	k.EnsureAlive(a) // first-ever launch: nothing was thrown away
	if got := k.Cost.Snapshot()[a].Rewarms; got != 0 {
		t.Fatalf("rewarms after a first launch = %d, want 0", got)
	}

	fr.Session(a).SetLastActivity(time.Now().Add(-time.Hour))
	k.EvictIdle()
	if k.IsHot(a) {
		t.Fatal("precondition: the bubble should have paged out")
	}

	k.EnsureAlive(a) // paged back in: the cache is gone and is re-paid here
	if got := k.Cost.Snapshot()[a].Rewarms; got != 1 {
		t.Fatalf("rewarms after paging back in = %d, want 1", got)
	}

	k.EnsureAlive(a) // already hot: no relaunch, no rewarm
	if got := k.Cost.Snapshot()[a].Rewarms; got != 1 {
		t.Fatalf("rewarms after a no-op EnsureAlive = %d, want 1", got)
	}
}

// TestIdlenessIsMeasuredByOutputNotDelivery pins the key eviction is sorted on.
//
// Idleness comes from the session's LastActivity (the process producing output),
// NOT from when the kernel last delivered something to it. A bubble that was
// just handed a message but has produced nothing since is still idle: the notice
// sits in its terminal until it takes a turn, and a bubble that never takes that
// turn must not be able to hold RAM forever by being messaged at. Both eviction
// paths agree on this one definition — EvictIdle always used it, EnforceBudget
// now does too (it previously used a logical last-USED stamp, since deleted).
//
// If you change this key, this test is where you find out.
func TestIdlenessIsMeasuredByOutputNotDelivery(t *testing.T) {
	fr, k, as := budgetKernel(t, 600, "silent", "working")
	silent, working := as[0], as[1]
	k.MemBudget = 1000 // 1200 resident: one must go
	k.CacheTTL = 0     // pure coldest-first, so this test measures ONLY the key

	fr.Session(silent).SetLastActivity(time.Now().Add(-time.Hour)) // messaged below, but never speaks
	fr.Session(working).SetLastActivity(time.Now())                // actively producing output

	if _, err := k.Send(addr.Root, silent, "ping", "you have work", 0, true); err != nil {
		t.Fatalf("send: %v", err)
	}

	k.EnforceBudget()
	if k.IsHot(silent) {
		t.Fatal("a bubble that has produced no output is idle however recently it was messaged")
	}
	if !k.IsHot(working) {
		t.Fatal("the session actually producing output should have been kept")
	}
}

// TestRelaunchSessionIsNotARewarm: a config bounce discards the cache too, but
// its rate tracks operator edits, not the paging policy. Counting it would give
// FRewarms a floor of noise independent of eviction — and FRewarms is the number
// the paging policy is judged by.
func TestRelaunchSessionIsNotARewarm(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0

	a, _ := k.Spawn(addr.Root, "a", "/tmp/a", runner.SpawnOpts{Name: "a"})
	k.EnsureAlive(a) // first launch: bubble now has a stored SessionID

	k.RelaunchSession(a)
	if !k.IsHot(a) {
		t.Fatal("precondition: RelaunchSession should leave the bubble hot")
	}
	if got := k.Cost.Snapshot()[a].Rewarms; got != 0 {
		t.Fatalf("rewarms after a deliberate config bounce = %d, want 0", got)
	}

	// A paging-induced wake through EnsureAlive still counts, so the opt-out is
	// narrow rather than a hole in the metric.
	fr.Session(a).Die()
	k.EnsureAlive(a)
	if got := k.Cost.Snapshot()[a].Rewarms; got != 1 {
		t.Fatalf("rewarms after paging back in = %d, want 1", got)
	}
}
