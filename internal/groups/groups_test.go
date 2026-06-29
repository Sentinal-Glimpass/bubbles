package groups

import (
	"sort"
	"testing"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

func TestGroupLifecycle(t *testing.T) {
	s := New()
	s.Create("api", []addr.Address{"0.1", "0.2"})

	if g, ok := s.Get("api"); !ok || len(g.Members) != 2 {
		t.Fatalf("group not created: %+v ok=%v", g, ok)
	}
	if tags := s.Tags("0.1"); len(tags) != 1 || tags[0] != "api" {
		t.Fatalf("0.1 tags = %v want [api]", tags)
	}
	if tags := s.Tags("0.9"); len(tags) != 0 {
		t.Fatalf("0.9 should have no tags, got %v", tags)
	}

	s.SetSession("api", "0.5")
	if g, _ := s.Get("api"); g.Session != "0.5" {
		t.Fatalf("session = %q want 0.5", g.Session)
	}
	if tags := s.Tags("0.5"); len(tags) != 1 || tags[0] != "api" {
		t.Fatalf("session 0.5 should be tagged: %v", tags)
	}

	s.Delete("api")
	if _, ok := s.Get("api"); ok {
		t.Fatal("group should be deleted")
	}
}

func TestGroupsAllOrdered(t *testing.T) {
	s := New()
	s.Create("a", nil)
	s.Create("b", nil)
	var names []string
	for _, g := range s.All() {
		names = append(names, g.Name)
	}
	sort.Strings(names)
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Fatalf("All = %v", names)
	}
}
