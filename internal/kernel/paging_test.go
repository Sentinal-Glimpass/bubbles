package kernel

import (
	"testing"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
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
