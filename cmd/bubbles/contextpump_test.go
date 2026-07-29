package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/kernel"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
	"github.com/Sentinal-Glimpass/bubbles/internal/transcript"
)

// pumpFixture is a one-bubble fleet with a private fake HOME, so convPath
// resolves into a temp dir and a synthesised transcript can be planted there.
type pumpFixture struct {
	m    *HealthManager
	k    *kernel.Kernel
	a    addr.Address
	home string
	dir  string
}

func newPumpFixture(t *testing.T) *pumpFixture {
	t.Helper()
	home, dir := t.TempDir(), t.TempDir()
	k := kernel.New(runner.NewFake())
	k.RelaunchProbe = 0
	a, err := k.Spawn(addr.Root, "w", dir, runner.SpawnOpts{Persona: "w"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	m := NewHealthManager(k)
	m.home = home
	return &pumpFixture{m: m, k: k, a: a, home: home, dir: dir}
}

// hot pages the bubble in and returns its fake session, so writes can be read
// back. EnsureAlive mints the session id, so it must run before writeContext.
func (f *pumpFixture) hot(t *testing.T) *runner.FakeSession {
	t.Helper()
	s := f.k.EnsureAlive(f.a)
	fs, ok := s.(*runner.FakeSession)
	if !ok {
		t.Fatalf("EnsureAlive returned %T, want *runner.FakeSession", s)
	}
	return fs
}

// sessionID assigns a session id without launching anything (cold bubble).
func (f *pumpFixture) sessionID(t *testing.T, id string) {
	t.Helper()
	b, ok := f.k.Reg.Get(f.a)
	if !ok {
		t.Fatal("bubble vanished from registry")
	}
	b.SessionID = id
}

// writeContext plants a transcript whose last usage entry reports n tokens.
func (f *pumpFixture) writeContext(t *testing.T, tokens int64) {
	t.Helper()
	b, _ := f.k.Reg.Get(f.a)
	if b.SessionID == "" {
		t.Fatal("writeContext needs a session id")
	}
	p := convPath(f.home, f.dir, b.SessionID)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"type":"assistant","message":{"usage":{"input_tokens":%d,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`, tokens)
	if err := os.WriteFile(p, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// agePump backdates every throttle window for the bubble, so throttle EXPIRY
// can be exercised without a 30-minute sleep.
func (f *pumpFixture) agePump(t *testing.T, d time.Duration) {
	t.Helper()
	f.m.pumpMu.Lock()
	defer f.m.pumpMu.Unlock()
	w, ok := f.m.lastPump[f.a]
	if !ok {
		t.Fatal("agePump: no throttle window recorded -- nothing was pumped")
	}
	if !w.nudge.IsZero() {
		w.nudge = w.nudge.Add(-d)
	}
	if !w.force.IsZero() {
		w.force = w.force.Add(-d)
	}
	f.m.lastPump[f.a] = w
}

func (f *pumpFixture) contextGauge() int64 {
	return f.k.Cost.Snapshot()[f.a].ContextTokens
}

// TestContextPumpBelowNudgeRecordsGaugeAndDoesNothingElse: the gauge is
// unconditional (the TUI panel and later phases consume it), but a bubble under
// the threshold must cost nothing -- no notice, no compaction.
func TestContextPumpBelowNudgeRecordsGaugeAndDoesNothingElse(t *testing.T) {
	f := newPumpFixture(t)
	s := f.hot(t)
	f.writeContext(t, 100_000)
	before := s.Written()

	f.m.pumpContext()

	if got := f.contextGauge(); got != 100_000 {
		t.Fatalf("FContextTokens = %d, want 100000 -- the gauge is recorded even when nothing is done", got)
	}
	if s.Written() != before {
		t.Fatalf("a bubble below %d tokens must not be written to, got %q", transcript.ContextNudgeTokens, s.Written())
	}
}

// TestContextPumpNudgesOnceAtThreshold: exactly one nudge, metered as a notice.
func TestContextPumpNudgesOnceAtThreshold(t *testing.T) {
	f := newPumpFixture(t)
	s := f.hot(t)
	f.writeContext(t, transcript.ContextNudgeTokens)

	f.m.pumpContext()

	w := s.Written()
	if !strings.Contains(w, "compact()") {
		t.Fatalf("want a nudge asking the bubble to call compact(), got %q", w)
	}
	if strings.Contains(w, "/compact") {
		t.Fatalf("at the nudge threshold compaction must be SUGGESTED, not forced, got %q", w)
	}
	if got := f.contextGauge(); got != transcript.ContextNudgeTokens {
		t.Fatalf("FContextTokens = %d, want %d", got, transcript.ContextNudgeTokens)
	}
	if got := f.k.Cost.Snapshot()[f.a].NoticesWritten; got != 1 {
		t.Fatalf("NoticesWritten = %d, want 1 -- the nudge must go through the notice path and be metered", got)
	}
}

// TestContextPumpThrottlesRepeatNudges is the primary correctness property of
// the pump: Sweep runs every 2 minutes, so a bubble parked above the threshold
// would otherwise be nudged ~30 times an hour -- far more expensive than the
// context leak the nudge exists to fix.
func TestContextPumpThrottlesRepeatNudges(t *testing.T) {
	f := newPumpFixture(t)
	s := f.hot(t)
	f.writeContext(t, transcript.ContextNudgeTokens+10_000)

	f.m.pumpContext()
	f.m.pumpContext() // the very next sweep, well inside the throttle

	if n := strings.Count(s.Written(), "compact()"); n != 1 {
		t.Fatalf("nudges written = %d, want exactly 1 -- the per-bubble throttle must hold across sweeps", n)
	}
	if got := f.k.Cost.Snapshot()[f.a].NoticesWritten; got != 1 {
		t.Fatalf("NoticesWritten = %d, want 1", got)
	}
}

// TestContextPumpForcesCompactionAtCeiling: past the force threshold the pump
// stops asking and types /compact itself.
func TestContextPumpForcesCompactionAtCeiling(t *testing.T) {
	f := newPumpFixture(t)
	s := f.hot(t)
	f.writeContext(t, transcript.ContextForceTokens)

	f.m.pumpContext()

	if !strings.Contains(s.Written(), "/compact") {
		t.Fatalf("want /compact typed at %d tokens, got %q", transcript.ContextForceTokens, s.Written())
	}
	if got := f.contextGauge(); got != transcript.ContextForceTokens {
		t.Fatalf("FContextTokens = %d, want %d", got, transcript.ContextForceTokens)
	}
}

// TestContextPumpThrottlesForcedCompaction: a forced compaction is a full model
// turn; unthrottled it is the worse half of the same flood.
func TestContextPumpThrottlesForcedCompaction(t *testing.T) {
	f := newPumpFixture(t)
	s := f.hot(t)
	f.writeContext(t, transcript.ContextForceTokens+50_000)

	f.m.pumpContext()
	f.m.pumpContext()

	if n := strings.Count(s.Written(), "/compact"); n != 1 {
		t.Fatalf("/compact written %d times, want exactly 1 -- forcing must be rate-limited too", n)
	}
}

// TestContextPumpEscalatesToForceInsideTheNudgeWindow: the 800k tier is the
// hard backstop, and the bubble it exists to catch is precisely the one that
// did NOT act on the polite nudge. If the nudge's throttle window also gated
// forcing, that bubble would be unforceable for up to 30 minutes -- the polite
// tier suppressing the backstop written to cover its failure.
func TestContextPumpEscalatesToForceInsideTheNudgeWindow(t *testing.T) {
	f := newPumpFixture(t)
	s := f.hot(t)

	f.writeContext(t, 600_000)
	f.m.pumpContext()
	if !strings.Contains(s.Written(), "compact()") {
		t.Fatalf("setup: expected a nudge at 600k, got %q", s.Written())
	}

	// Same bubble climbs past the force threshold, well inside the 30m window.
	f.writeContext(t, 900_000)
	f.m.pumpContext()

	if !strings.Contains(s.Written(), "/compact") {
		t.Fatalf("crossing into the force tier must compact immediately, not wait out the nudge window; got %q", s.Written())
	}
	if got := f.contextGauge(); got != 900_000 {
		t.Fatalf("FContextTokens = %d, want 900000", got)
	}
}

// TestContextPumpForceTierStaysRateLimitedAfterEscalation: escalation must not
// turn the force tier into a once-per-sweep /compact, which is the more
// expensive flood.
func TestContextPumpForceTierStaysRateLimitedAfterEscalation(t *testing.T) {
	f := newPumpFixture(t)
	s := f.hot(t)

	f.writeContext(t, 600_000)
	f.m.pumpContext() // nudge
	f.writeContext(t, 900_000)
	f.m.pumpContext() // escalate: force
	f.m.pumpContext() // next sweep, still above force
	f.m.pumpContext()

	if n := strings.Count(s.Written(), "/compact"); n != 1 {
		t.Fatalf("/compact written %d times, want exactly 1 -- the force tier keeps its own window", n)
	}
}

// TestContextPumpNudgesAgainAfterTheThrottleExpires pins the OTHER side of the
// throttle. Without it, a throttle that is permanent -- nudge once, ever --
// passes every other test in this file, since they all pump back to back. The
// window must suppress inside 30 minutes AND allow after it.
func TestContextPumpNudgesAgainAfterTheThrottleExpires(t *testing.T) {
	f := newPumpFixture(t)
	s := f.hot(t)
	f.writeContext(t, transcript.ContextNudgeTokens+10_000)

	f.m.pumpContext()
	if n := strings.Count(s.Written(), "compact()"); n != 1 {
		t.Fatalf("setup: nudges = %d, want 1", n)
	}

	// Age the window past the throttle instead of sleeping 30 minutes.
	f.agePump(t, contextPumpThrottle+time.Minute)
	f.m.pumpContext()

	if n := strings.Count(s.Written(), "compact()"); n != 2 {
		t.Fatalf("nudges = %d, want 2 -- a bubble still oversized after the throttle expires must be nudged again, or the pump asks once and gives up forever", n)
	}
}

// TestContextPumpForcesAgainAfterTheThrottleExpires: same two-sided guarantee
// for the hard tier.
func TestContextPumpForcesAgainAfterTheThrottleExpires(t *testing.T) {
	f := newPumpFixture(t)
	s := f.hot(t)
	f.writeContext(t, transcript.ContextForceTokens+50_000)

	f.m.pumpContext()
	f.agePump(t, contextPumpThrottle+time.Minute)
	f.m.pumpContext()

	if n := strings.Count(s.Written(), "/compact"); n != 2 {
		t.Fatalf("/compact written %d times, want 2 after the force window expired", n)
	}
}

// TestContextPumpDoesNotPageInColdBubble: waking a cold bubble to tell it to
// compact pays the full prompt-cache rewarm this phase exists to avoid.
func TestContextPumpDoesNotPageInColdBubble(t *testing.T) {
	f := newPumpFixture(t)
	f.sessionID(t, "cold-session-id")
	f.writeContext(t, transcript.ContextForceTokens+100_000)

	f.m.pumpContext()

	if f.k.IsHot(f.a) {
		t.Fatal("the pump must never page in a cold bubble -- that costs the rewarm it exists to save")
	}
	if got := f.contextGauge(); got != transcript.ContextForceTokens+100_000 {
		t.Fatalf("FContextTokens = %d -- reading a transcript is read-only and must be measured on cold bubbles too", got)
	}
}

// TestContextPumpSkipsMissingTranscript: a bubble that has never run has no
// transcript. That is normal, not an error.
func TestContextPumpSkipsMissingTranscript(t *testing.T) {
	f := newPumpFixture(t)
	s := f.hot(t) // EnsureAlive mints a session id but plants no transcript
	before := s.Written()

	f.m.pumpContext() // must not panic

	if s.Written() != before {
		t.Fatalf("a bubble with no transcript must be skipped silently, got %q", s.Written())
	}
	if got := f.contextGauge(); got != 0 {
		t.Fatalf("FContextTokens = %d, want 0 -- no transcript means no measurement", got)
	}
}

// TestContextPumpSkipsTranscriptWithNoUsage: ErrNoUsage means the file exists
// but carries no basis for a context size (user turns only, pre-first-reply).
func TestContextPumpSkipsTranscriptWithNoUsage(t *testing.T) {
	f := newPumpFixture(t)
	s := f.hot(t)
	b, _ := f.k.Reg.Get(f.a)
	p := convPath(f.home, f.dir, b.SessionID)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"type":"user","message":{"role":"user"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := s.Written()

	f.m.pumpContext()

	if s.Written() != before {
		t.Fatalf("ErrNoUsage must be skipped silently, got %q", s.Written())
	}
}

// TestContextPumpSkipsBubbleWithoutSessionID: no session id means no
// conversation to resolve, and convPath would name a bogus file.
func TestContextPumpSkipsBubbleWithoutSessionID(t *testing.T) {
	f := newPumpFixture(t)
	// never launched, never assigned an id
	f.m.pumpContext() // must not panic
	if got := f.contextGauge(); got != 0 {
		t.Fatalf("FContextTokens = %d, want 0 for a bubble with no session id", got)
	}
}

// TestSweepRunsTheContextPump: the pump is only a forcing function if it is
// actually wired into the periodic sweep.
func TestSweepRunsTheContextPump(t *testing.T) {
	f := newPumpFixture(t)
	s := f.hot(t)
	f.writeContext(t, transcript.ContextNudgeTokens)

	f.m.Sweep()

	if !strings.Contains(s.Written(), "compact()") {
		t.Fatalf("Sweep must run pumpContext, got %q", s.Written())
	}
}

// forceWindowClaimed reports whether the force tier's 30-minute throttle window
// has been spent on this bubble.
func (f *pumpFixture) forceWindowClaimed() bool {
	f.m.pumpMu.Lock()
	defer f.m.pumpMu.Unlock()
	return !f.m.lastPump[f.a].force.IsZero()
}

// TestContextPumpDoesNotForceCompactIntoTheOperatorsTyping is the input-safety
// property for the FORCED tier. The pump is a background ticker; typing
// /compact plus Enter into the bubble the operator is currently dived into
// would submit their half-written prompt. The nudge tier has always honoured
// this (SystemNotice); the force tier used to call Compact and walk past it.
func TestContextPumpDoesNotForceCompactIntoTheOperatorsTyping(t *testing.T) {
	f := newPumpFixture(t)
	s := f.hot(t)
	f.k.TypingWindow = time.Hour // the operator is continuously typing
	f.k.SetFocus(f.a)
	f.k.NoteKeystroke()
	f.writeContext(t, transcript.ContextForceTokens+50_000)

	f.m.pumpContext()

	if strings.Contains(s.Written(), "/compact") {
		t.Fatalf("forced compaction was typed into a half-written prompt: %q", s.Written())
	}
	if f.forceWindowClaimed() {
		t.Fatal("a compaction that never happened must not claim the 30-minute window")
	}

	// Retried on a later sweep once the operator pauses -- the hold is a delay,
	// not a cancellation, and the bubble is still 800k+.
	f.k.TypingWindow = time.Nanosecond
	f.m.pumpContext()

	if n := strings.Count(s.Written(), "/compact"); n != 1 {
		t.Fatalf("/compact written %d times after the operator paused, want 1", n)
	}
	if !f.forceWindowClaimed() {
		t.Fatal("a compaction that DID land must claim the window")
	}
}

// TestContextPumpDoesNotBurnTheWindowOnANotReadySession: a session still
// booting or parked on the resume menu swallows /compact unsubmitted. Claiming
// the throttle window there would buy 30 minutes of silence for an action that
// never happened -- an 800k bubble left uncompacted for half an hour.
func TestContextPumpDoesNotBurnTheWindowOnANotReadySession(t *testing.T) {
	f := newPumpFixture(t)
	s := f.hot(t)
	s.SetInputReady(false)
	f.writeContext(t, transcript.ContextForceTokens+50_000)

	f.m.pumpContext()

	if strings.Contains(s.Written(), "/compact") {
		t.Fatalf("/compact must not be typed into a session that would swallow it: %q", s.Written())
	}
	if f.forceWindowClaimed() {
		t.Fatal("the force window was claimed for a compaction the session never received")
	}

	// The next sweep finds it ready and compacts -- no 30-minute wait.
	s.SetInputReady(true)
	f.m.pumpContext()

	if n := strings.Count(s.Written(), "/compact"); n != 1 {
		t.Fatalf("/compact written %d times once the session was ready, want 1", n)
	}
	if !f.forceWindowClaimed() {
		t.Fatal("the delivered compaction must claim the window")
	}
}
