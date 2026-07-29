// Package paging decides WHICH resident bubbles to page out, and in what
// order. It is pure: no clock, no I/O, no kernel — every input (memory,
// context size, idleness) is supplied by the caller, which is what makes the
// policy table-testable in isolation from the runner.
//
// A bubble is a live `claude` process. Paging it out kills the process to
// reclaim RAM; the conversation survives and resumes later via --resume. But
// killing it also throws away its prompt cache, so the next use re-pays full
// uncached input for the bubble's entire context. A 600k-token bubble evicted
// and woken two minutes later can cost more in that one rewarm than the
// reclaimed RAM was ever worth. This package makes that trade explicit.
package paging

import (
	"sort"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

// Candidate is one resident bubble, as seen by the eviction policy.
type Candidate struct {
	Addr          addr.Address
	MemBytes      uint64        // actual resident memory
	ContextTokens int64         // proxy for rewarm cost; 0 = unknown (see neutralTokens)
	IdleFor       time.Duration // how long since its last activity
	AlwaysOn      bool          // always-on receivers are never paged out
}

// Config is the policy's tuning.
type Config struct {
	MemBudget int64         // resident bytes the live set must fit within; <= 0 disables
	CacheTTL  time.Duration // prompt-cache lifetime; below this an eviction throws away a live cache
	// Grace is the recency floor. A candidate idle for less than Grace has just
	// been woken — it has already paid one rewarm — and must not be made to pay
	// another on the very next sweep. It is a preference in ORDER, never an
	// exemption: see tier.
	Grace time.Duration
}

// tier splits candidates into the two eviction bands that make the ordering
// converge. Band 0 (idle for at least Grace) is ordered entirely ahead of band 1
// (idle for less than Grace), so a just-woken bubble is only ever a victim once
// everything that has settled has already been taken.
//
// Without this the waste score alone decides, and waste is dominated by context
// size: a freshly woken SMALL bubble scores ~tokens*1.0, still far below a large
// bubble idle a single minute, so the small one is re-evicted on every sweep and
// pays a rewarm each time while the large one never yields. The tier makes the
// fleet make progress: the expensive bubble is evicted ONCE instead.
//
// Grace == 0 puts every candidate in band 0 (IdleFor >= 0 always), which is
// exactly the single-tier ordering that came before.
func tier(c Candidate, grace time.Duration) int {
	if c.IdleFor >= grace {
		return 0
	}
	return 1
}

// waste is the scoring rule, and the one sentence a future reader needs:
// evicting a bubble wastes its rewarm cost (ContextTokens) in proportion to
// how much of its prompt cache is still alive (1 at zero idleness, decaying
// linearly to 0 at CacheTTL), so the least wasteful bubble is evicted first.
//
// With CacheTTL == 0 every waste is 0, which collapses the ordering to the
// tie-break — plain coldest-first LRU, i.e. exactly today's behaviour.
func waste(c Candidate, tokens int64, cacheTTL time.Duration) float64 {
	if cacheTTL <= 0 || c.IdleFor >= cacheTTL {
		return 0 // cache is dead (or protection disabled): eviction costs nothing
	}
	remaining := 1 - float64(c.IdleFor)/float64(cacheTTL)
	return float64(tokens) * remaining
}

// neutralTokens resolves an unknown ContextTokens (0). An unknown is treated
// as AVERAGE — the mean of the candidates whose context IS known — so a bubble
// the context pump has not measured yet is neither shielded (which would let
// an unmeasured bubble squat forever) nor sacrificed (which would make every
// freshly launched bubble the first thing killed). If nothing is known, all
// candidates score 0 and the ordering falls back to LRU.
func neutralTokens(live []Candidate) int64 {
	var sum, n int64
	for _, c := range live {
		if c.ContextTokens > 0 {
			sum += c.ContextTokens
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / n
}

// evictable drops the bubbles that are never victims: root and always-on
// receivers. This is pre-existing law in both kernel eviction paths.
func evictable(live []Candidate) []Candidate {
	out := make([]Candidate, 0, len(live))
	for _, c := range live {
		if c.Addr.IsRoot() || c.AlwaysOn {
			continue
		}
		out = append(out, c)
	}
	return out
}

// Victims returns, in eviction order, who to page out so that the evictable
// live set fits within c.MemBudget. Ordering is cost-aware (see waste) and
// recency-aware (see tier), but the budget is never blown to protect an
// expensive or a recently woken bubble: those decide WHO goes, not HOW MANY. If
// draining the whole non-grace tier still leaves the fleet over budget, the
// grace tier is drained too rather than exceed the budget.
//
// The resident total is summed HERE, over the very same candidates that can be
// victims, so no caller can pass a total covering a different set. That is not
// tidiness: the drain loop subtracts each victim's MemBytes from an unsigned
// running total, so a total that included never-evictable RAM (root, always-on)
// would drain every candidate and still read as over budget.
func Victims(c Config, live []Candidate) []addr.Address {
	if c.MemBudget <= 0 {
		return nil
	}
	cand := evictable(live)
	var totalMem uint64
	for _, x := range cand {
		totalMem += x.MemBytes
	}
	if int64(totalMem) <= c.MemBudget {
		return nil
	}
	neutral := neutralTokens(cand)
	scores := make(map[addr.Address]float64, len(cand))
	for _, x := range cand {
		tokens := x.ContextTokens
		if tokens <= 0 {
			tokens = neutral
		}
		scores[x.Addr] = waste(x, tokens, c.CacheTTL)
	}
	sort.SliceStable(cand, func(i, j int) bool {
		if ti, tj := tier(cand[i], c.Grace), tier(cand[j], c.Grace); ti != tj {
			return ti < tj // settled bubbles are all ordered ahead of just-woken ones
		}
		si, sj := scores[cand[i].Addr], scores[cand[j].Addr]
		if si != sj {
			return si < sj // least wasteful to evict goes first
		}
		if cand[i].IdleFor != cand[j].IdleFor {
			return cand[i].IdleFor > cand[j].IdleFor // then coldest first (LRU)
		}
		return cand[i].Addr < cand[j].Addr // then stable, for determinism
	})

	total := totalMem
	var victims []addr.Address
	for _, x := range cand {
		if int64(total) <= c.MemBudget {
			break
		}
		victims = append(victims, x.Addr)
		total -= x.MemBytes
	}
	return victims
}

// IdleVictims returns, coldest first, the bubbles idle enough that eviction is
// genuinely free: past idleTimeout AND past c.CacheTTL. A bubble whose prompt
// cache is still warm is spared even when it exceeds idleTimeout — with no
// memory pressure demanding the RAM, killing a live cache for idleness alone is
// pure waste. CacheTTL == 0 disables that protection, restoring the plain
// idle-timeout behaviour.
func IdleVictims(c Config, idleTimeout time.Duration, live []Candidate) []addr.Address {
	if idleTimeout <= 0 {
		return nil
	}
	cand := evictable(live)
	var idle []Candidate
	for _, x := range cand {
		if x.IdleFor < idleTimeout || x.IdleFor < c.CacheTTL {
			continue
		}
		idle = append(idle, x)
	}
	sort.SliceStable(idle, func(i, j int) bool {
		if idle[i].IdleFor != idle[j].IdleFor {
			return idle[i].IdleFor > idle[j].IdleFor
		}
		return idle[i].Addr < idle[j].Addr
	})
	out := make([]addr.Address, 0, len(idle))
	for _, x := range idle {
		out = append(out, x.Addr)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
