package kernel

import (
	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/costmeter"
	"github.com/Sentinal-Glimpass/bubbles/internal/paging"
	"github.com/Sentinal-Glimpass/bubbles/internal/sessions"
)

// This file holds the kernel side of paging: it turns the live session table
// into the pure policy's inputs (internal/paging) and applies the verdict. The
// policy owns WHO is evicted and in what order; the kernel owns the process
// table, the locking, and the killing.

// gatherLive snapshots every live non-root worker session. Only the map read
// happens under the table lock: measuring a session (MemBytes shells out to the
// OS) and consulting the registry both take their own locks and must never nest
// inside the session lock.
func (k *Kernel) gatherLive() []sessions.Live { return k.sessions.Live() }

// candidates measures the gathered sessions and builds the policy's view of
// them, returning the candidates and the resident total OF THE EVICTABLE ONES.
//
// The total is summed HERE, over the very same slice that is handed to
// paging.Victims, from this one measurement pass. That is not tidiness: Victims
// subtracts each victim's MemBytes from an unsigned running total, so a total
// sourced from a different pass (or covering a different set of sessions) could
// underflow and wrap to ~1.8e19, which would read as "still over budget" and
// evict the entire fleet. Always-on sessions are counted in neither the total
// nor the victims — their RAM has never been budgeted against the workers.
//
// ContextTokens comes from the cost meter, where the context pump publishes it
// each sweep. A bubble it has not reached yet is left at 0 on purpose: the
// policy reads 0 as "unknown" and scores it as the average of the known, which
// neither shields an unmeasured bubble nor makes every fresh one the first to die.
func (k *Kernel) candidates(live []sessions.Live) ([]paging.Candidate, uint64) {
	var tokens map[addr.Address]costmeter.Counters
	if k.Cost != nil {
		tokens = k.Cost.Snapshot()
	}
	now := k.clockNow()
	out := make([]paging.Candidate, 0, len(live))
	var total uint64
	for _, e := range live {
		mem := e.Session.MemBytes()
		idle := now.Sub(e.Session.LastActivity())
		if idle < 0 {
			idle = 0 // a clock skew must not read as a maximally warm cache
		}
		c := paging.Candidate{
			Addr:          e.Addr,
			MemBytes:      mem,
			ContextTokens: tokens[e.Addr].ContextTokens,
			IdleFor:       idle,
			AlwaysOn:      k.isAlwaysOn(e.Addr),
		}
		if !c.AlwaysOn {
			total += mem
		}
		out = append(out, c)
	}
	return out, total
}

// pageOut drops each victim from the session table (under the table lock) and
// then kills its process (outside it — a Kill can block, and no lock is ever
// held across one). Every page-out is metered: the whole point of a paging policy change is
// that evictions and rewarms can be compared before and after.
func (k *Kernel) pageOut(victims []addr.Address) {
	if len(victims) == 0 {
		return
	}
	k.sessions.DeleteAll(victims)
	for _, v := range victims {
		_ = k.runner.Kill(v) // page out; registry + session id persist -> resumes on use
		if k.Cost != nil {
			k.Cost.Add(v, costmeter.FEvictions, 1)
		}
	}
}

// pagingConfig is the policy tuning the kernel currently runs under.
func (k *Kernel) pagingConfig() paging.Config {
	return paging.Config{MemBudget: k.MemBudget, CacheTTL: k.CacheTTL}
}

// noteRewarm records that a paged-out bubble has just been relaunched, paying
// full uncached input for its whole conversation again. It is only ever called
// once a launch has actually succeeded — a failed launch costs nothing and
// warms nothing. Paired with FEvictions this is how the paging policy is
// judged: evictions roughly flat while rewarms fall is the win.
func (k *Kernel) noteRewarm(a addr.Address, wasPagedOut bool) {
	if !wasPagedOut || k.Cost == nil {
		return
	}
	k.Cost.Add(a, costmeter.FRewarms, 1)
}
