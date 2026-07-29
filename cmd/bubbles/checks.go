package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/kernel"
	"github.com/Sentinal-Glimpass/bubbles/internal/supervisor"
)

// This file is the single inventory of the process's periodic background work.
//
// Every sweep used to be its own inline `go func(){ t := time.NewTicker(D);
// for range t.C { ... } }()` in runApp. That shape had no name, no way to
// observe it, and — because nothing in this repo recovers — a panic in any one
// of them killed the whole daemon. They now all live in one supervisor.Registry
// so a panic is recovered, recorded and attributed to a named check.
//
// Intervals here are copied verbatim from the loops they replace; changing one
// is a behaviour change, not a refactor.

// checkPhase says when a check may start running.
type checkPhase int

const (
	// phaseBoot checks are registered as soon as the kernel is wired up.
	phaseBoot checkPhase = iota
	// phaseAfterLoad checks must not run until the persisted fleet/inbox/tasks/
	// schedules have been loaded. The savers in particular: a saver that ticked
	// during a slow boot would overwrite a persisted file with empty in-memory
	// state, a race that really did wipe schedules and mail on large fleets.
	phaseAfterLoad
)

// bgCheck is a registry check plus the boot phase it belongs to.
type bgCheck struct {
	supervisor.Check
	phase checkPhase
}

// checkDeps is everything the background checks close over. The three TUI
// pollers arrive pre-built (see samplerStep / claudeUsageStep /
// headroomStatsStep) because each keeps setup state that must be created once,
// not per tick.
type checkDeps struct {
	k         *kernel.Kernel
	baseDir   string
	health    *HealthManager
	inboxPoll time.Duration // messagePollMinutes() minutes, resolved by the caller

	stuck *stuckTracker // hot-but-wedged detector; keeps the previous sample set

	sampler       func() // resource sampler -> TUI usage + fleet-health panel
	claudeUsage   func() // account /usage -> TUI (no-op when not logged in)
	headroomStats func() // compression savings -> TUI (no-op unless --headroom)
}

// plain adapts a background sweep that neither takes a context nor reports an
// error to the registry's Fn signature. None of the migrated loops ever
// returned anything; wrapping keeps that honest rather than inventing errors.
func plain(fn func()) func(context.Context) error {
	return func(context.Context) error { fn(); return nil }
}

// saverStep builds one persistence saver: it writes only when the store's
// version has moved since the last write, exactly as the old startSaver
// closure did (including the -1 seed, so the first tick re-saves the
// just-loaded state idempotently rather than skipping it).
func saverStep(baseDir string, k *kernel.Kernel, version func() int64, save func(string, *kernel.Kernel) error) func() {
	var last int64 = -1
	return func() {
		if v := version(); v != last {
			last = v
			_ = save(baseDir, k)
		}
	}
}

// backgroundChecks returns every periodic check the process runs, in a fixed
// order. It is pure: it builds closures and does no I/O, so the completeness
// test can enumerate the whole inventory without a running fleet.
func backgroundChecks(d checkDeps) []bgCheck {
	k := d.k
	return []bgCheck{
		// periodic memory sweep: catch sessions that grow past the budget over time
		{Check: supervisor.Check{Name: "budget", Every: 5 * time.Second, Fn: plain(k.EnforceBudget)}, phase: phaseBoot},
		// periodic idle sweep: page out sessions that have gone quiet
		{Check: supervisor.Check{Name: "idle", Every: 60 * time.Second, Fn: plain(k.EvictIdle)}, phase: phaseBoot},
		// deliver a held message to the focused bubble as soon as you stop typing
		{Check: supervisor.Check{Name: "flush-held", Every: 1 * time.Second, Fn: plain(k.FlushHeldIfIdle)}, phase: phaseBoot},
		// write coalesced batches once their window closes, so a burst of
		// non-urgent follow-ups costs one notice instead of one per message — and
		// doesn't sit silent until the next message happens to arrive. Cadence is
		// well under notify.CoalesceWindow so a closed batch is announced
		// promptly. Never wakes a cold bubble.
		{Check: supervisor.Check{Name: "coalesce-drain", Every: 1 * time.Second, Fn: plain(k.DrainCoalesced)}, phase: phaseBoot},
		// periodic inbox drain: page in cold bubbles with pending mail so none go unanswered
		{Check: supervisor.Check{Name: "inbox-drain", Every: d.inboxPoll, Fn: plain(k.DrainInboxes)}, phase: phaseBoot},
		// fast recovery: re-nudge already-running bubbles whose notice never landed (cheap PTY write)
		{Check: supervisor.Check{Name: "recover-unread", Every: 45 * time.Second, Fn: plain(func() { k.RecoverUnread(true) })}, phase: phaseBoot},
		// fire durable wake schedules: the always-alive daemon wakes bubbles on their triggers
		{Check: supervisor.Check{Name: "schedules", Every: 20 * time.Second, Fn: plain(k.FireDue)}, phase: phaseBoot},
		// dashboard pollers: they feed the TUI's top-right panel and send to
		// whichever program is currently running (nil while diving into a bubble).
		{Check: supervisor.Check{Name: "sampler", Every: 2 * time.Second, Fn: plain(d.sampler)}, phase: phaseBoot},
		{Check: supervisor.Check{Name: "claude-usage", Every: 1 * time.Second, Fn: plain(d.claudeUsage)}, phase: phaseBoot},
		{Check: supervisor.Check{Name: "headroom-stats", Every: 3 * time.Second, Fn: plain(d.headroomStats)}, phase: phaseBoot},
		// stuck-bubble scan: sample the already-hot bubbles and record which look
		// hot-but-wedged (unread mail, no new output). Observation only — it
		// never wakes, nudges or kills anything; the list feeds the TUI panel.
		{Check: supervisor.Check{Name: "stuck-scan", Every: stuckEvery, Fn: plain(d.stuck.Step)}, phase: phaseBoot},
		// fleet-health manager: background upkeep (transcript trimming and the
		// context pump today). Off the request path, on its own slow cadence.
		{Check: supervisor.Check{Name: "health-sweep", Every: 2 * time.Minute, Fn: plain(d.health.Sweep)}, phase: phaseAfterLoad},
		// keep always-on receivers alive: relaunch any that die. This is the ONE
		// sanctioned background path that may call EnsureAlive (kernel.go:367) —
		// always-on bubbles are defined as never allowed to be cold.
		{Check: supervisor.Check{Name: "keep-alive", Every: 30 * time.Second, Fn: plain(k.KeepAlive)}, phase: phaseAfterLoad},
		// persistence savers, one per store, each gated on a version change.
		{Check: supervisor.Check{Name: "save-inbox", Every: 2 * time.Second, Fn: plain(saverStep(d.baseDir, k, k.Store.Version, saveInbox))}, phase: phaseAfterLoad},
		{Check: supervisor.Check{Name: "save-tasks", Every: 2 * time.Second, Fn: plain(saverStep(d.baseDir, k, k.Tasks.Version, saveTasks))}, phase: phaseAfterLoad},
		{Check: supervisor.Check{Name: "save-schedules", Every: 2 * time.Second, Fn: plain(saverStep(d.baseDir, k, k.Sched.Version, saveSchedules))}, phase: phaseAfterLoad},
	}
}

// registerPhase registers every check belonging to phase. A registration error
// means a programming mistake (duplicate name, non-positive interval); it is
// reported and the remaining checks still register, because losing one sweep is
// better than losing all of them.
func registerPhase(reg *supervisor.Registry, checks []bgCheck, phase checkPhase) {
	for _, c := range checks {
		if c.phase != phase {
			continue
		}
		if err := reg.Register(c.Check); err != nil {
			fmt.Fprintf(os.Stderr, "bubbles: %v\n", err)
		}
	}
}

// supervisorTick is how often the driver asks the registry what is due. It has
// to be well under the shortest check interval (1s) so a 1s check isn't
// systematically late; the registry, not this tick, decides what actually runs.
const supervisorTick = 1 * time.Second

// runChecks drives the registry until ctx is cancelled.
//
// Each tick's sweep runs in ITS OWN goroutine, deliberately: RunDue waits for
// the whole batch it claimed, and the driver must not stall behind a check
// blocked on I/O — a hung HTTP fetch, a stalled PTY. Per-check isolation within
// a batch is RunDue's job (it runs each claimed check in its own goroutine);
// this goroutine is what keeps the TICKER itself free. Together they preserve
// the property the old one-goroutine-per-loop code had: a wedged sweep delays
// only itself.
//
// Spawning per tick is safe because claimDue marks a check running inside the
// same critical section in which it tests dueness: a check whose previous run
// is still in flight is simply not claimed again, so a wedged check cannot pile
// up and the next tick runs everything else. That is also why there is no
// per-check timeout here — a check that legitimately outruns its interval must
// be allowed to finish.
//
// The driver itself cannot die: RunDue never panics (it recovers per check)
// and never returns an error, and the goroutine holds no state to corrupt.
func runChecks(ctx context.Context, reg *supervisor.Registry, tick time.Duration) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			go reg.RunDue(ctx, now)
		}
	}
}
