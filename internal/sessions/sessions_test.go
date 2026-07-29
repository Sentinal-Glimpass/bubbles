package sessions

import (
	"sync"
	"testing"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
)

// fakeSession is a runner.Session whose liveness the test controls.
type fakeSession struct {
	alive bool
	mem   uint64
}

func (f *fakeSession) Write(p []byte) (int, error) { return len(p), nil }
func (f *fakeSession) Close() error                { return nil }
func (f *fakeSession) Alive() bool                 { return f.alive }
func (f *fakeSession) MemBytes() uint64            { return f.mem }
func (f *fakeSession) CPUTime() time.Duration      { return 0 }
func (f *fakeSession) LastActivity() time.Time     { return time.Time{} }
func (f *fakeSession) RecentOutput() string        { return "" }
func (f *fakeSession) InputReady() bool            { return true }

var _ runner.Session = (*fakeSession)(nil)

func TestGetSetDelete(t *testing.T) {
	tab := New()
	a := addr.Address("1")
	if got := tab.Get(a); got != nil {
		t.Fatalf("empty table returned %v", got)
	}
	s := &fakeSession{alive: true}
	tab.Set(a, s)
	if got := tab.Get(a); got != runner.Session(s) {
		t.Fatalf("Get returned %v, want the stored session", got)
	}
	tab.Delete(a)
	if got := tab.Get(a); got != nil {
		t.Fatalf("Get after Delete returned %v, want nil", got)
	}
}

func TestSetReplaces(t *testing.T) {
	tab := New()
	a := addr.Address("1")
	first, second := &fakeSession{alive: true}, &fakeSession{alive: true}
	tab.Set(a, first)
	tab.Set(a, second)
	if got := tab.Get(a); got != runner.Session(second) {
		t.Fatal("Set did not replace the previous session")
	}
}

// Take hands the session back so the caller can Close it OUTSIDE the lock —
// that is the whole reason it exists rather than a Get+Delete pair.
func TestTakeReturnsAndRemoves(t *testing.T) {
	tab := New()
	a := addr.Address("1")
	s := &fakeSession{alive: true}
	tab.Set(a, s)
	if got := tab.Take(a); got != runner.Session(s) {
		t.Fatalf("Take returned %v, want the stored session", got)
	}
	if got := tab.Get(a); got != nil {
		t.Fatal("Take did not remove the entry")
	}
	if got := tab.Take(a); got != nil {
		t.Fatalf("Take on a missing address returned %v, want nil", got)
	}
}

func TestDeleteAll(t *testing.T) {
	tab := New()
	for _, a := range []addr.Address{"1", "2", "3"} {
		tab.Set(a, &fakeSession{alive: true})
	}
	tab.DeleteAll(nil) // no-op, must not panic
	tab.DeleteAll([]addr.Address{"1", "3", "9"})
	if tab.Get("1") != nil || tab.Get("3") != nil {
		t.Fatal("DeleteAll left a victim behind")
	}
	if tab.Get("2") == nil {
		t.Fatal("DeleteAll removed an address it was not given")
	}
}

func TestIsHot(t *testing.T) {
	tab := New()
	if tab.IsHot("1") {
		t.Fatal("absent address reported hot")
	}
	tab.Set("1", &fakeSession{alive: false})
	if tab.IsHot("1") {
		t.Fatal("dead session reported hot")
	}
	tab.Set("2", &fakeSession{alive: true})
	if !tab.IsHot("2") {
		t.Fatal("live session reported cold")
	}
	var nilSess runner.Session
	tab.Set("3", nilSess)
	if tab.IsHot("3") {
		t.Fatal("nil session reported hot")
	}
}

// Live is the paging path's input: live, non-root, non-nil only.
func TestLiveFilters(t *testing.T) {
	tab := New()
	tab.Set(addr.Root, &fakeSession{alive: true})
	tab.Set("1", &fakeSession{alive: true})
	tab.Set("2", &fakeSession{alive: false})
	var nilSess runner.Session
	tab.Set("3", nilSess)

	live := tab.Live()
	if len(live) != 1 {
		t.Fatalf("Live returned %d entries, want 1: %+v", len(live), live)
	}
	if live[0].Addr != addr.Address("1") {
		t.Fatalf("Live returned %s, want 1", live[0].Addr)
	}
	if live[0].Session == nil {
		t.Fatal("Live entry has no session")
	}
}

func TestLiveEmpty(t *testing.T) {
	if got := New().Live(); len(got) != 0 {
		t.Fatalf("empty table returned %d live sessions", len(got))
	}
}

// The table is touched from the delivery path, the sweep, and the paging path
// at once; -race must stay clean.
func TestConcurrentAccess(t *testing.T) {
	tab := New()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a := addr.Address(string(rune('1' + i)))
			for n := 0; n < 200; n++ {
				tab.Set(a, &fakeSession{alive: true})
				tab.Get(a)
				tab.IsHot(a)
				tab.Live()
				tab.Take(a)
				tab.DeleteAll([]addr.Address{a})
			}
		}(i)
	}
	wg.Wait()
}
