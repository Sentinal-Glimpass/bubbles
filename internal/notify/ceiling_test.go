package notify

import "testing"
import "time"

func TestCeilingCapsBurst(t *testing.T) {
	c := NewCeiling(DefaultCeilingPerMinute, DefaultCeilingBurst)
	now := time.Unix(0, 0)
	allowed := 0
	for i := 0; i < 178; i++ { // the observed 632fe95 flood size
		if c.Allow("0.1", now) {
			allowed++
		}
	}
	if allowed != DefaultCeilingBurst {
		t.Fatalf("allowed = %d, want %d -- INV-1 violated", allowed, DefaultCeilingBurst)
	}
}

func TestCeilingRefillsOverTime(t *testing.T) {
	c := NewCeiling(6, 6)
	now := time.Unix(0, 0)
	for i := 0; i < 6; i++ {
		c.Allow("0.1", now)
	}
	if c.Allow("0.1", now) {
		t.Fatal("bucket should be empty")
	}
	if !c.Allow("0.1", now.Add(10*time.Second)) { // 6/min = 1 per 10s
		t.Fatal("bucket should have refilled one token after 10s")
	}
}

func TestCeilingIsPerBubble(t *testing.T) {
	c := NewCeiling(6, 6)
	now := time.Unix(0, 0)
	for i := 0; i < 6; i++ {
		c.Allow("0.1", now)
	}
	if !c.Allow("0.2", now) {
		t.Fatal("0.2 must have its own bucket")
	}
}
