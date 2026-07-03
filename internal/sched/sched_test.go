package sched

import (
	"testing"
	"time"
)

func TestParseTrigger(t *testing.T) {
	if tr, err := ParseTrigger("15m", ""); err != nil || tr.Interval != 15*time.Minute {
		t.Fatalf("every: %+v %v", tr, err)
	}
	if tr, err := ParseTrigger("", "08:30"); err != nil || tr.DailyMin != 8*60+30 {
		t.Fatalf("daily: %+v %v", tr, err)
	}
	if _, err := ParseTrigger("15m", "08:00"); err == nil {
		t.Fatal("both set should error")
	}
	if _, err := ParseTrigger("5s", ""); err == nil {
		t.Fatal("interval < 30s should error")
	}
	if _, err := ParseTrigger("", "25:00"); err == nil {
		t.Fatal("bad clock time should error")
	}
	if _, err := ParseTrigger("", ""); err == nil {
		t.Fatal("neither set should error")
	}
}

func TestTriggerNext(t *testing.T) {
	base := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	// interval
	if got := (Trigger{Interval: time.Hour}).Next(base); !got.Equal(base.Add(time.Hour)) {
		t.Fatalf("interval next = %v", got)
	}
	// daily later today
	if got := (Trigger{DailyMin: 14 * 60}).Next(base); got.Hour() != 14 || !got.Equal(time.Date(2026, 7, 3, 14, 0, 0, 0, time.UTC)) {
		t.Fatalf("daily later today = %v", got)
	}
	// daily already passed -> tomorrow
	if got := (Trigger{DailyMin: 8 * 60}).Next(base); got.Day() != 4 || got.Hour() != 8 {
		t.Fatalf("daily passed should roll to tomorrow, got %v", got)
	}
}

func TestDueAndPersist(t *testing.T) {
	now := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	s := New()
	sc := s.Add(now, "0.1", "0.1", "poll", "check feed", Trigger{Interval: 15 * time.Minute}, true)
	// not due yet
	if len(s.Due(now.Add(10*time.Minute))) != 0 {
		t.Fatal("should not be due before the interval")
	}
	// due after the interval; advances to next
	due := s.Due(now.Add(16 * time.Minute))
	if len(due) != 1 || due[0].ID != sc.ID {
		t.Fatalf("should be due, got %v", due)
	}
	got, _ := s.Get(sc.ID)
	if !got.NextFire.After(now.Add(16 * time.Minute)) {
		t.Fatalf("NextFire should advance to the future, got %v", got.NextFire)
	}

	// persist round-trip
	snap := s.Snapshot()
	r := New()
	r.Load(snap)
	if _, ok := r.Get(sc.ID); !ok {
		t.Fatal("schedule lost across snapshot/load")
	}

	// purge
	s.PurgeBubble("0.1")
	if len(s.All()) != 0 {
		t.Fatal("purge should remove schedules for the bubble")
	}
}
