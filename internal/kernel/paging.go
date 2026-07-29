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

// gatherLive snapshots every live session. The lock discipline now lives in
// internal/sessions: only the map read happens under the table lock, because
// measuring a session (MemBytes shells out to the OS) and consulting the
// registry both take their own locks and must never nest inside it.
func (k *Kernel) gatherLive() []sessions.Live { return k.sessions.Live() }

// candidates builds the policy's view of the gathered sessions. The resident
// total is no longer computed here: paging.Victims sums it internally, over the
// very candidates it may evict, so no caller can hand it a total covering a
// different set of sessions.
//
// measureMem controls whether each session's MemBytes is probed. The probe
// shells out to the OS (cgroup/proc) once per live session, so the callers that
// feed a policy which ignores MemBytes — IdleVictims — pass false and skip the
// syscalls entirely rather than duplicating this function.
//
// ContextTokens comes from the cost meter, where the context pump publishes it
// each sweep. A bubble it has not reached yet is left at 0 on purpose: the
// policy reads 0 as "unknown" and scores it as the average of the known, which
// neither shields an unmeasured bubble nor makes every fresh one the first to die.
func (k *Kernel) candidates(live []sessions.Live, measureMem bool) []paging.Candidate {
	var tokens map[addr.Address]costmeter.Counters
	if k.Cost != nil {
		tokens = k.Cost.Snapshot()
	}
	now := k.clockNow()
	out := make([]paging.Candidate, 0, len(live))
	for _, e := range live {
		var mem uint64
		if measureMem {
			mem = e.Session.MemBytes()
		}
		idle := now.Sub(e.Session.LastActivity())
		if idle < 0 {
			// Backwards clock skew (or a LastActivity stamped in the future)
			// clamps to zero idleness, i.e. the bubble reads as maximally WARM:
			// last to be idle-evicted, and — under the waste score — last in the
			// budget ordering too. That is deliberately the safe direction. The
			// clamp bounds the distortion to one sweep's worth of skew instead of
			// letting a negative duration produce a nonsense score, and being
			// read as warm can only reorder WHO is evicted; it can never exempt
			// the fleet from the budget, which drains candidates until the
			// resident total fits no matter how each one scores.
			idle = 0
		}
		out = append(out, paging.Candidate{
			Addr:          e.Addr,
			MemBytes:      mem,
			ContextTokens: tokens[e.Addr].ContextTokens,
			IdleFor:       idle,
			AlwaysOn:      k.isAlwaysOn(e.Addr),
		})
	}
	return out
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
	return paging.Config{MemBudget: k.MemBudget, CacheTTL: k.CacheTTL, Grace: k.Grace}
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
