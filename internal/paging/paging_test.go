package paging

import (
	"testing"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

const gb = uint64(1) << 30

func totalOf(cs []Candidate) uint64 {
	var t uint64
	for _, c := range cs {
		t += c.MemBytes
	}
	return t
}

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
			got := Victims(tt.cfg, tt.live, totalOf(tt.live))
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
