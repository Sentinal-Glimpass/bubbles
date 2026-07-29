package registry

import (
	"testing"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/notify"
)

func TestRootSeeded(t *testing.T) {
	r := New()
	b, ok := r.Get(addr.Root)
	if !ok || b.Persona != "root" {
		t.Fatalf("root not seeded: %+v ok=%v", b, ok)
	}
}

func TestAddAssignsAddresses(t *testing.T) {
	r := New()
	a1 := r.Add(addr.Root, "scout", "/tmp/scout")
	a2 := r.Add(addr.Root, "docs", "/tmp/docs")
	if a1.Addr != "0.1" || a2.Addr != "0.2" {
		t.Fatalf("got %q,%q want 0.1,0.2", a1.Addr, a2.Addr)
	}
	nested := r.Add(a1.Addr, "helper", "/tmp/h")
	if nested.Addr != "0.1.1" {
		t.Fatalf("nested = %q want 0.1.1", nested.Addr)
	}
}

func TestRestoreContinuesNumbering(t *testing.T) {
	r := New()
	r.Restore(Bubble{Addr: "0.1", Persona: "a", Parent: "0", Status: Idle})
	r.Restore(Bubble{Addr: "0.2", Persona: "b", Parent: "0", Status: Idle})
	if b, ok := r.Get("0.1"); !ok || b.Persona != "a" {
		t.Fatalf("0.1 not restored: %+v ok=%v", b, ok)
	}
	if nb := r.Add(addr.Root, "c", ""); nb.Addr != "0.3" {
		t.Fatalf("next Add after restore = %q want 0.3", nb.Addr)
	}
}

func TestStatusAndChildren(t *testing.T) {
	r := New()
	a1 := r.Add(addr.Root, "scout", "")
	r.Add(addr.Root, "docs", "")
	r.SetStatus(a1.Addr, Done)
	if b, _ := r.Get(a1.Addr); b.Status != Done {
		t.Fatalf("status = %q want done", b.Status)
	}
	if got := len(r.Children(addr.Root)); got != 2 {
		t.Fatalf("root children = %d want 2", got)
	}
	if _, ok := r.Get("0.9"); ok {
		t.Fatal("Get unknown should be false")
	}
}

func TestMuteRulesRoundTrip(t *testing.T) {
	r := New()
	a := r.Add(addr.Root, "w", "/tmp")
	r.SetMuteRules(a.Addr, []notify.Rule{{ID: "r1", Source: "pump", Window: time.Hour}})
	got := r.MuteRules(a.Addr)
	if len(got) != 1 || got[0].ID != "r1" {
		t.Fatalf("MuteRules = %+v, want one rule r1", got)
	}
}

func TestSetMuteRulesBumpsVersion(t *testing.T) {
	r := New()
	a := r.Add(addr.Root, "w", "/tmp")
	v0 := r.Version()
	r.SetMuteRules(a.Addr, []notify.Rule{{ID: "r1", Source: "pump"}})
	if r.Version() == v0 {
		t.Fatal("SetMuteRules must bump version so fleet.json is re-saved")
	}
}

func TestMuteRulesReturnsCopy(t *testing.T) {
	r := New()
	a := r.Add(addr.Root, "w", "/tmp")
	r.SetMuteRules(a.Addr, []notify.Rule{{ID: "r1", Source: "pump"}})
	got := r.MuteRules(a.Addr)
	got[0].ID = "mutated"
	got2 := r.MuteRules(a.Addr)
	if got2[0].ID != "r1" {
		t.Fatalf("MuteRules must return a copy, got mutated state: %+v", got2)
	}
}

// TestReapExpiredMuteRules covers the persisted half of mute-rule reaping:
// expired rules leave the registry, live and TTL-less ones stay, the version
// moves only when something actually changed (so the saver isn't woken for a
// no-op), and a second pass is a no-op.
func TestReapExpiredMuteRules(t *testing.T) {
	r := New()
	a := r.Add(addr.Root, "", "").Addr
	b := r.Add(addr.Root, "", "").Addr

	created := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	r.SetMuteRules(a, []notify.Rule{
		{ID: "keep", Source: "x", Created: created},
		{ID: "gone", Source: "y", TTL: time.Hour, Created: created},
		{ID: "live", Source: "z", TTL: 10 * time.Hour, Created: created},
	})
	r.SetMuteRules(b, []notify.Rule{{ID: "b-live", Source: "q", TTL: 10 * time.Hour, Created: created}})

	now := created.Add(2 * time.Hour)
	if n := r.ReapExpiredMuteRules(now); n != 1 {
		t.Fatalf("reaped %d rules fleet-wide, want 1", n)
	}
	got := r.MuteRules(a)
	if len(got) != 2 || got[0].ID != "keep" || got[1].ID != "live" {
		t.Fatalf("survivors = %+v, want [keep live]", got)
	}
	if len(r.MuteRules(b)) != 1 {
		t.Fatal("another bubble's live rule was reaped")
	}

	v := r.Version()
	if n := r.ReapExpiredMuteRules(now); n != 0 {
		t.Fatalf("second reap removed %d, want 0", n)
	}
	if r.Version() != v {
		t.Fatal("a no-op reap bumped the version, which would make every saver tick write")
	}
}

// TestReapExpiredMuteRulesFreesQuota: the reason the reap exists. A bubble that
// spent its whole MaxRules quota on rules that have since expired must be able
// to store rules again.
func TestReapExpiredMuteRulesFreesQuota(t *testing.T) {
	r := New()
	a := r.Add(addr.Root, "", "").Addr
	created := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	var rules []notify.Rule
	for i := 0; i < notify.MaxRules; i++ {
		rules = append(rules, notify.Rule{ID: string(rune('a' + i)), Source: "s", TTL: time.Hour, Created: created})
	}
	r.SetMuteRules(a, rules)

	// The MaxRules cap lives in RuleSet.Add, which is what kernel.MuteBy runs
	// the stored rules through. Before the reap it is full.
	full := notify.NewRuleSet()
	for _, x := range r.MuteRules(a) {
		_ = full.Add(x)
	}
	if err := full.Add(notify.Rule{ID: "new", Source: "s"}); err != notify.ErrTooManyRules {
		t.Fatalf("expected a full rule set, got %v", err)
	}

	if n := r.ReapExpiredMuteRules(created.Add(2 * time.Hour)); n != notify.MaxRules {
		t.Fatalf("reaped %d, want %d", n, notify.MaxRules)
	}
	after := notify.NewRuleSet()
	for _, x := range r.MuteRules(a) {
		_ = after.Add(x)
	}
	if err := after.Add(notify.Rule{ID: "new", Source: "s"}); err != nil {
		t.Fatalf("after reaping an all-expired rule set, a new rule was still rejected: %v", err)
	}
}
