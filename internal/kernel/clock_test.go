package kernel

import (
	"testing"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
)

// The clock has to be genuinely injectable, not merely "indirected so tests
// could stub it" — the crash-loop backoff is a decision about elapsed time and
// asserting on it must never mean sleeping.
func TestSetClockDrivesClockNow(t *testing.T) {
	k := New(runner.NewFake())
	fixed := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	k.SetClock(func() time.Time { return fixed })
	if got := k.clockNow(); !got.Equal(fixed) {
		t.Fatalf("clockNow = %v want %v", got, fixed)
	}
	k.SetClock(nil) // nil restores the wall clock rather than panicking
	if time.Since(k.clockNow()) > time.Minute {
		t.Fatal("clockNow did not fall back to the wall clock")
	}
}

func TestNewDefaultsClockToWallClock(t *testing.T) {
	k := New(runner.NewFake())
	if time.Since(k.clockNow()) > time.Minute {
		t.Fatal("New must default the clock to time.Now")
	}
}
