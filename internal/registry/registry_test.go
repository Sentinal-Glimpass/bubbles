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
