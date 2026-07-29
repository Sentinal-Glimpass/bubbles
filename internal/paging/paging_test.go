package paging

import (
	"testing"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

const gb = uint64(1) << 30

func eq(got, want []addr.Address) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestVictims(t *testing.T) {
	ttl := 5 * time.Minute
	tests := []struct {
		name string
		cfg  Config
		live []Candidate
		want []addr.Address
	}{
		{
			name: "under budget evicts nothing",
			cfg:  Config{MemBudget: int64(10 * gb), CacheTTL: ttl},
			live: []Candidate{
				{Addr: "0.1", MemBytes: 2 * gb, ContextTokens: 100_000, IdleFor: time.Minute},
				{Addr: "0.2", MemBytes: 3 * gb, ContextTokens: 500_000, IdleFor: time.Hour},
			},
			want: nil,
		},
		{
			name: "equal idleness evicts the cheapest to rewarm first, not plain LRU",
			cfg:  Config{MemBudget: int64(5 * gb), CacheTTL: ttl},
			// Both warm and equally idle. Plain LRU would be free to pick
			// either; cost-awareness must pick the small-context one.
			live: []Candidate{
				{Addr: "0.1", MemBytes: 3 * gb, ContextTokens: 600_000, IdleFor: time.Minute},
				{Addr: "0.2", MemBytes: 3 * gb, ContextTokens: 20_000, IdleFor: time.Minute},
			},
			want: []addr.Address{"0.2"},
		},
		{
			name: "a large recently-active bubble is protected over an equally large long-idle one",
			cfg:  Config{MemBudget: int64(5 * gb), CacheTTL: ttl},
			live: []Candidate{
				{Addr: "0.1", MemBytes: 3 * gb, ContextTokens: 600_000, IdleFor: 5 * time.Second},
				{Addr: "0.2", MemBytes: 3 * gb, ContextTokens: 600_000, IdleFor: time.Hour},
			},
			want: []addr.Address{"0.2"},
		},
		{
			name: "budget is never blown to protect an expensive bubble",
			cfg:  Config{MemBudget: int64(3 * gb), CacheTTL: ttl},
			// Everything left is huge and warm; memory still wins.
			live: []Candidate{
				{Addr: "0.1", MemBytes: 3 * gb, ContextTokens: 900_000, IdleFor: time.Second},
				{Addr: "0.2", MemBytes: 3 * gb, ContextTokens: 800_000, IdleFor: time.Second},
			},
			want: []addr.Address{"0.2"}, // cheaper of the two expensive ones
		},
		{
			name: "always-on and root are never victims",
			cfg:  Config{MemBudget: int64(1 * gb), CacheTTL: ttl},
			live: []Candidate{
				{Addr: addr.Root, MemBytes: 4 * gb, ContextTokens: 10, IdleFor: time.Hour},
				{Addr: "0.1", MemBytes: 4 * gb, ContextTokens: 10, IdleFor: time.Hour, AlwaysOn: true},
				{Addr: "0.2", MemBytes: 4 * gb, ContextTokens: 900_000, IdleFor: time.Second},
			},
			want: []addr.Address{"0.2"},
		},
		{
			name: "unknown ContextTokens is neutral: neither shielded nor sacrificed",
			cfg:  Config{MemBudget: int64(3 * gb), CacheTTL: ttl},
			// Known contexts are 100k and 500k (mean 300k), so the unknown
			// bubble must rank between them: evicted after the cheap one and
			// before the expensive one.
			live: []Candidate{
				{Addr: "0.1", MemBytes: 3 * gb, ContextTokens: 100_000, IdleFor: time.Minute},
				{Addr: "0.2", MemBytes: 3 * gb, ContextTokens: 0, IdleFor: time.Minute},
				{Addr: "0.3", MemBytes: 3 * gb, ContextTokens: 500_000, IdleFor: time.Minute},
			},
			want: []addr.Address{"0.1", "0.2"},
		},
		{
			name: "CacheTTL zero disables cache protection and restores plain LRU",
			cfg:  Config{MemBudget: int64(5 * gb), CacheTTL: 0},
			live: []Candidate{
				{Addr: "0.1", MemBytes: 3 * gb, ContextTokens: 20_000, IdleFor: time.Hour},
				{Addr: "0.2", MemBytes: 3 * gb, ContextTokens: 900_000, IdleFor: 2 * time.Hour},
			},
			// Coldest first, exactly as today, cost ignored.
			want: []addr.Address{"0.2"},
		},
		{
			name: "an idle bubble is preferred over a warm one even when it is pricier",
			cfg:  Config{MemBudget: int64(5 * gb), CacheTTL: ttl},
			live: []Candidate{
				{Addr: "0.1", MemBytes: 3 * gb, ContextTokens: 50_000, IdleFor: time.Second},
				{Addr: "0.2", MemBytes: 3 * gb, ContextTokens: 900_000, IdleFor: time.Hour},
			},
			want: []addr.Address{"0.2"}, // its cache is already dead: eviction is free
		},
		{
			name: "evicts as many as needed to fit",
			cfg:  Config{MemBudget: int64(3 * gb), CacheTTL: ttl},
			live: []Candidate{
				{Addr: "0.1", MemBytes: 2 * gb, ContextTokens: 900_000, IdleFor: time.Second},
				{Addr: "0.2", MemBytes: 2 * gb, ContextTokens: 10_000, IdleFor: time.Second},
				{Addr: "0.3", MemBytes: 2 * gb, ContextTokens: 20_000, IdleFor: time.Second},
			},
			want: []addr.Address{"0.2", "0.3"},
		},
		{
			name: "no budget configured means no eviction",
			cfg:  Config{MemBudget: 0, CacheTTL: ttl},
			live: []Candidate{
				{Addr: "0.1", MemBytes: 900 * gb, ContextTokens: 1, IdleFor: time.Hour},
			},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Victims(tt.cfg, tt.live)
			if !eq(got, tt.want) {
				t.Fatalf("Victims = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIdleVictims(t *testing.T) {
	ttl := 5 * time.Minute
	timeout := 30 * time.Minute
	tests := []struct {
		name    string
		cfg     Config
		timeout time.Duration
		live    []Candidate
		want    []addr.Address
	}{
		{
			name:    "spares a bubble whose cache is still warm",
			cfg:     Config{CacheTTL: time.Hour},
			timeout: timeout,
			// Past the idle timeout, but inside the cache lifetime: with no
			// memory pressure demanding it, killing this is pure waste.
			live: []Candidate{{Addr: "0.1", ContextTokens: 400_000, IdleFor: 40 * time.Minute}},
			want: nil,
		},
		{
			name:    "evicts beyond both thresholds",
			cfg:     Config{CacheTTL: ttl},
			timeout: timeout,
			live:    []Candidate{{Addr: "0.1", ContextTokens: 400_000, IdleFor: 40 * time.Minute}},
			want:    []addr.Address{"0.1"},
		},
		{
			name:    "does not evict below the idle timeout",
			cfg:     Config{CacheTTL: ttl},
			timeout: timeout,
			live:    []Candidate{{Addr: "0.1", IdleFor: 10 * time.Minute}},
			want:    nil,
		},
		{
			name:    "always-on and root are never victims",
			cfg:     Config{CacheTTL: ttl},
			timeout: timeout,
			live: []Candidate{
				{Addr: addr.Root, IdleFor: time.Hour},
				{Addr: "0.1", IdleFor: time.Hour, AlwaysOn: true},
				{Addr: "0.2", IdleFor: time.Hour},
			},
			want: []addr.Address{"0.2"},
		},
		{
			name:    "CacheTTL zero restores today's behaviour",
			cfg:     Config{CacheTTL: 0},
			timeout: timeout,
			live:    []Candidate{{Addr: "0.1", ContextTokens: 900_000, IdleFor: 31 * time.Minute}},
			want:    []addr.Address{"0.1"},
		},
		{
			name:    "no timeout configured means no eviction",
			cfg:     Config{CacheTTL: ttl},
			timeout: 0,
			live:    []Candidate{{Addr: "0.1", IdleFor: 100 * time.Hour}},
			want:    nil,
		},
		{
			name:    "coldest first",
			cfg:     Config{CacheTTL: ttl},
			timeout: timeout,
			live: []Candidate{
				{Addr: "0.1", IdleFor: 40 * time.Minute},
				{Addr: "0.2", IdleFor: 90 * time.Minute},
			},
			want: []addr.Address{"0.2", "0.1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IdleVictims(tt.cfg, tt.timeout, tt.live)
			if !eq(got, tt.want) {
				t.Fatalf("IdleVictims = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestVictimsGraceTier pins the recency floor: a candidate idle for less than
// Grace has just paid a rewarm and is ordered BEHIND every settled candidate,
// but the floor is an ordering preference and never an exemption from the
// budget.
func TestVictimsGraceTier(t *testing.T) {
	ttl := 30 * time.Minute
	grace := 5 * time.Minute
	tests := []struct {
		name string
		cfg  Config
		live []Candidate
		want []addr.Address
	}{
		{
			// The reviewer's scenario, as one ordering decision: the small
			// just-woken bubble is the cheapest to rewarm and would go first on
			// waste alone, which is what made it thrash. The settled bubble goes
			// instead, however expensive, because it has not just paid.
			name: "a just-woken bubble is ordered behind a settled one, whatever the waste says",
			cfg:  Config{MemBudget: int64(1 * gb), CacheTTL: ttl, Grace: grace},
			live: []Candidate{
				{Addr: "0.1", MemBytes: 1 * gb, ContextTokens: 500_000, IdleFor: 10 * time.Minute},
				{Addr: "0.2", MemBytes: 1 * gb, ContextTokens: 1_000, IdleFor: 0},
			},
			want: []addr.Address{"0.1"},
		},
		{
			// THE invariant: grace reorders WHO, never HOW MANY. Draining every
			// settled candidate is not enough here, so the policy must continue
			// into the grace tier rather than leave the fleet over budget.
			name: "the budget wins: the grace tier is drained too when the settled tier is not enough",
			cfg:  Config{MemBudget: int64(1 * gb), CacheTTL: ttl, Grace: grace},
			live: []Candidate{
				{Addr: "0.1", MemBytes: 1 * gb, ContextTokens: 500_000, IdleFor: time.Hour},
				{Addr: "0.2", MemBytes: 1 * gb, ContextTokens: 900_000, IdleFor: 2 * time.Second},
				{Addr: "0.3", MemBytes: 1 * gb, ContextTokens: 10_000, IdleFor: time.Second},
			},
			// settled tier first (0.1), then inside the grace tier the existing
			// waste ordering still decides: the cheapest to rewarm (0.3).
			want: []addr.Address{"0.1", "0.3"},
		},
		{
			// Everything is inside the grace window and the fleet is still over
			// budget: grace cannot spare anyone, and the ordering inside the tier
			// is exactly the pre-grace waste ordering.
			name: "grace never exempts: all candidates inside the window still page out to fit",
			cfg:  Config{MemBudget: int64(1 * gb), CacheTTL: ttl, Grace: grace},
			live: []Candidate{
				{Addr: "0.1", MemBytes: 1 * gb, ContextTokens: 900_000, IdleFor: time.Second},
				{Addr: "0.2", MemBytes: 1 * gb, ContextTokens: 10_000, IdleFor: time.Second},
				{Addr: "0.3", MemBytes: 1 * gb, ContextTokens: 20_000, IdleFor: time.Second},
			},
			want: []addr.Address{"0.2", "0.3"},
		},
		{
			// Grace == 0 must be today's single-tier behaviour exactly: the same
			// fleet as the first case, where the floor would have changed the
			// answer, resolves on waste alone.
			name: "Grace zero reproduces the single-tier waste ordering exactly",
			cfg:  Config{MemBudget: int64(1 * gb), CacheTTL: ttl, Grace: 0},
			live: []Candidate{
				{Addr: "0.1", MemBytes: 1 * gb, ContextTokens: 500_000, IdleFor: 10 * time.Minute},
				{Addr: "0.2", MemBytes: 1 * gb, ContextTokens: 1_000, IdleFor: 0},
			},
			want: []addr.Address{"0.2"},
		},
		{
			// And with the cache protection off too, Grace == 0 is still plain
			// coldest-first LRU, the pre-Phase-3 behaviour.
			name: "Grace zero and CacheTTL zero is still plain coldest-first LRU",
			cfg:  Config{MemBudget: int64(1 * gb), CacheTTL: 0, Grace: 0},
			live: []Candidate{
				{Addr: "0.1", MemBytes: 1 * gb, ContextTokens: 500_000, IdleFor: 10 * time.Minute},
				{Addr: "0.2", MemBytes: 1 * gb, ContextTokens: 1_000, IdleFor: 0},
			},
			want: []addr.Address{"0.1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Victims(tt.cfg, tt.live)
			if !eq(got, tt.want) {
				t.Fatalf("Victims = %v, want %v", got, tt.want)
			}
			// Whatever the ordering chose, the budget must actually be met.
			var left uint64
			evicted := map[addr.Address]bool{}
			for _, v := range got {
				evicted[v] = true
			}
			for _, c := range tt.live {
				if !c.Addr.IsRoot() && !c.AlwaysOn && !evicted[c.Addr] {
					left += c.MemBytes
				}
			}
			if int64(left) > tt.cfg.MemBudget {
				t.Fatalf("after evicting %v the evictable set is %d bytes, over the %d budget", got, left, tt.cfg.MemBudget)
			}
		})
	}
}

// TestVictimsSumsItsOwnTotal: the resident total is summed inside Victims over
// the candidates it may actually evict, so RAM that is never evictable (root,
// always-on) can no longer be charged against the budget by a caller. Before
// this, a caller passing a total that included that RAM would drain every
// worker and still read as over budget.
func TestVictimsSumsItsOwnTotal(t *testing.T) {
	live := []Candidate{
		{Addr: addr.Root, MemBytes: 40 * gb, ContextTokens: 10, IdleFor: time.Hour},
		{Addr: "0.1", MemBytes: 40 * gb, ContextTokens: 10, IdleFor: time.Hour, AlwaysOn: true},
		{Addr: "0.2", MemBytes: 1 * gb, ContextTokens: 10, IdleFor: time.Hour},
	}
	if got := Victims(Config{MemBudget: int64(2 * gb), CacheTTL: time.Minute}, live); got != nil {
		t.Fatalf("Victims = %v, want none: the workers (1 GB) fit the 2 GB budget; root and always-on RAM is not budgeted against them", got)
	}
}
