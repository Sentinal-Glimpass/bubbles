package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/costmeter"
	"github.com/Sentinal-Glimpass/bubbles/internal/kernel"
	"github.com/Sentinal-Glimpass/bubbles/internal/transcript"
)

// HealthManager is the fleet-health daemon: background upkeep that keeps the
// fleet healthy over time, separate from the kernel's request path. Its checks
// run on a periodic Sweep. This is the seam we grow as the product matures —
// today it trims conversation transcripts; later it can reap orphans, cap disk,
// flag stale daemons, etc.
type HealthManager struct {
	k    *kernel.Kernel
	home string

	// Context-pump throttle state (see contextpump.go). It lives on the manager
	// rather than in the kernel because it is a property of THIS sweep's
	// cadence, not of the bubble: the kernel's own ceiling bounds what a bubble
	// can receive, while this bounds how often the pump asks.
	pumpMu   sync.Mutex
	lastPump map[addr.Address]pumpWindows

	// Throttle state for the never-compacted-oversized-transcript stderr
	// warning (see reportOversizedTranscript). Separate from pumpMu/lastPump:
	// this is a report, not an action, and shares no correctness property with
	// the pump's escalation tiers.
	reportMu            sync.Mutex
	lastOversizedReport map[addr.Address]time.Time

	// Throttle state for the STEADY-STATE trim outcomes only (see recordTrim).
	// Keyed by bubble AND outcome so a condition changing is always announced
	// immediately, and only an unchanged one is quieted.
	trimLogMu   sync.Mutex
	lastTrimLog map[trimLogKey]time.Time

	// logf is where trim decisions are announced. It is a field rather than a
	// direct os.Stderr write so a test can prove a decision was announced at
	// all -- the absence of any such proof is why this incident needed forensic
	// archaeology. The daemon's stderr lands in .bubbles/daemon.log.
	logf func(format string, a ...any)

	// Decoded-transcript memo, keyed by conversation path, live only for the
	// duration of one Sweep. See transcriptStats: it is what stops a sweep from
	// JSON-decoding every bubble's whole transcript two or three times over.
	statMu    sync.Mutex
	sweeping  bool
	statCache map[string]cachedStats
}

// cachedStats is one transcript's decoded Stats, exactly as transcript.Read
// returned them.
//
// There is deliberately no size/mtime stamp and no cross-sweep lifetime. A
// size+mtime key looks like the obvious invalidation rule and is not safe here:
// two writes of the same length can land on the same mtime on a
// coarse-granularity filesystem, and a cache that outlived the sweep would then
// serve a stale context size for as long as the file held still — the pump
// silently measuring a number that had already moved. Scoping the memo to one
// sweep needs no invalidation rule at all (see transcriptStats).
type cachedStats struct {
	st  transcript.Stats
	err error
}

// transcriptStats is the ONLY way this package reads a transcript. It returns
// exactly what transcript.Read would return, decoding at most once per distinct
// file state.
//
// It exists because the sweep had three full reads per cold bubble every 2
// minutes: pumpContext read it, trimTranscripts read it again for the oversized
// check, and trimTranscript read the raw bytes a third time. transcript.Read
// json.Unmarshals every line, so at the 4 MiB oversized ceiling that is ~12 MiB
// of I/O plus two full JSON decodes per parked bubble per sweep — the pump
// leaking cost in a different currency than the one it was written to save.
//
// The memo is scoped to ONE sweep (see beginSweep/endSweep), which is what
// makes it behaviour-preserving rather than merely cheaper. Within a sweep the
// only bubbles read twice are COLD ones — trimTranscripts skips hot bubbles
// outright, so the pump is the first and only reader of a live transcript —
// and a cold bubble is by definition not appending, so nothing can move under
// the memo. The single in-sweep mutation is trimTranscript's own rewrite, which
// drops a prefix and never touches the last usage-bearing entry, so the
// ContextTokens the pump reads afterwards is the same number either way.
//
// Outside a sweep every call reads afresh: no cheap file stamp is strong enough
// to trust across the 2-minute gap (see cachedStats).
func (m *HealthManager) transcriptStats(path string) (transcript.Stats, error) {
	m.statMu.Lock()
	e, ok := m.statCache[path]
	live := m.sweeping
	m.statMu.Unlock()
	if live && ok {
		return e.st, e.err
	}

	st, err := transcript.Read(path)

	m.statMu.Lock()
	if m.sweeping {
		m.statCache[path] = cachedStats{st: st, err: err}
	}
	m.statMu.Unlock()
	return st, err
}

// beginSweep/endSweep bound the memo's lifetime. Outside a sweep every read is
// a fresh read: nothing else in the process shares this manager's cadence, and
// a memo that outlives the sweep would be a cache with no invalidation event
// strong enough to trust (see cachedStats).
func (m *HealthManager) beginSweep() {
	m.statMu.Lock()
	m.sweeping = true
	m.statCache = map[string]cachedStats{}
	m.statMu.Unlock()
}

func (m *HealthManager) endSweep() {
	m.statMu.Lock()
	m.sweeping = false
	m.statCache = map[string]cachedStats{} // release the decoded Stats between sweeps
	m.statMu.Unlock()
}

// prune drops per-bubble state for bubbles that no longer exist. Without it
// lastPump, lastOversizedReport and lastTrimLog grow for the life of the
// process: a fleet
// that spawns and deletes workers all day would keep one entry per address ever
// seen, and the throttle state of a deleted bubble is meaningless anyway (a
// re-spawn gets a fresh address). The stats memo needs no pruning — it is
// emptied at the end of every sweep.
func (m *HealthManager) prune() {
	live := map[addr.Address]bool{}
	for _, b := range m.k.Reg.All() {
		live[b.Addr] = true
	}

	m.pumpMu.Lock()
	for a := range m.lastPump {
		if !live[a] {
			delete(m.lastPump, a)
		}
	}
	m.pumpMu.Unlock()

	m.reportMu.Lock()
	for a := range m.lastOversizedReport {
		if !live[a] {
			delete(m.lastOversizedReport, a)
		}
	}
	m.reportMu.Unlock()

	m.trimLogMu.Lock()
	for k := range m.lastTrimLog {
		if !live[k.addr] {
			delete(m.lastTrimLog, k)
		}
	}
	m.trimLogMu.Unlock()
}

// NewHealthManager builds the manager over a kernel.
func NewHealthManager(k *kernel.Kernel) *HealthManager {
	home, _ := os.UserHomeDir()
	return &HealthManager{
		k:                   k,
		home:                home,
		lastPump:            map[addr.Address]pumpWindows{},
		lastOversizedReport: map[addr.Address]time.Time{},
		lastTrimLog:         map[trimLogKey]time.Time{},
		statCache:           map[string]cachedStats{},
		logf:                func(format string, a ...any) { fmt.Fprintf(os.Stderr, format, a...) },
	}
}

// Sweep runs every health check once. Add checks here as they land.
func (m *HealthManager) Sweep() {
	m.beginSweep()
	m.trimTranscripts()
	m.pumpContext()
	m.endSweep()
	// Last: both checks above have just enumerated the registry, so anything
	// deleted since the previous sweep is dropped with this sweep's own view.
	m.prune()
}

// transcriptKeepBeforeCompact is how many lines to keep BEFORE the latest
// compaction boundary — a small safety buffer. Everything before that is dead
// history the model already summarized away.
const transcriptKeepBeforeCompact = 10

// transcriptOversizedBytes is the byte ceiling past which a COLD, NEVER
// compacted transcript is reported (see reportOversizedTranscript) instead of
// silently ignored. 4 MiB is chosen well above the size a transcript can reach
// before Task 3's pump (contextpump.go) forces /compact at ContextForceTokens
// (800k tokens, roughly a few MiB of JSONL for typical entry shapes) — so a
// file crossing this ceiling with no compaction marker anywhere in it means
// the pump has not yet had a chance to act (the bubble just went cold, or the
// force throttle window hasn't closed), not that trimming was skipped by
// mistake.
const transcriptOversizedBytes = 4 * 1024 * 1024

// oversizedReportThrottle bounds how often the stderr warning fires for the
// same bubble. Sweep runs every 2 minutes; without a throttle a bubble parked
// above the ceiling (which it stays until the pump forces a compaction that
// creates a boundary — see below) would log ~30 lines an hour for a condition
// that has not changed, the same flood contextPumpThrottle exists to prevent
// on the action side.
const oversizedReportThrottle = 30 * time.Minute

// trimTranscripts clears stale context from every COLD bubble's conversation
// transcript, keeping the file (and session id) intact so the bubble keeps
// writing to it on resume. It runs only on cold bubbles: claude appends to the
// .jsonl through a held fd, and rewriting a file it has open would lose appends
// or corrupt it. Hot bubbles are trimmed the moment they next page out.
func (m *HealthManager) trimTranscripts() {
	if m.home == "" {
		return
	}
	for _, b := range m.k.Reg.All() {
		if b.Addr.IsRoot() || b.SessionID == "" || b.Dir == "" {
			continue
		}
		path := convPath(m.home, b.Dir, b.SessionID)
		if m.k.IsHot(b.Addr) {
			// Never rewrite a transcript claude currently holds open. Recorded
			// like every other outcome, through the steady-state throttle: it is
			// the commonest outcome in a healthy fleet, and the sweep runs every
			// 2 minutes. The session id here is the REGISTRY's, since nothing
			// read the file — the log line says so by construction.
			m.recordTrim(b.Addr, trimResult{Outcome: trimRefusedHot, Path: path, Session: b.SessionID})
			continue
		}

		// Read-only check BEFORE the trim attempt, reusing transcript.Read so
		// there is one definition of "has a compaction boundary" (the same one
		// trimTranscript's own scan and the pump in contextpump.go both answer
		// to). transcript.Read still returns valid Stats alongside
		// ErrNoUsage (a transcript with no usage-bearing entry yet, e.g. a huge
		// backlog of tool-only turns, is exactly a case worth reporting) — only
		// a hard I/O error (missing file, unreadable line) yields no usable
		// Stats and is skipped here; trimTranscript below performs its own read
		// and handles os.IsNotExist the normal way.
		st, err := m.transcriptStats(path)
		measured := err == nil || err == transcript.ErrNoUsage
		if measured && !st.HasCompaction && st.Bytes > transcriptOversizedBytes {
			m.reportOversizedTranscript(b.Addr, st.Bytes)
		}
		if measured && !st.HasCompaction {
			// A file with no compaction marker anywhere in it has no boundary
			// to cut before, so trimTranscript is a guaranteed no-op — and its
			// os.ReadFile would pull the whole (possibly multi-MiB) file in to
			// prove it. HasCompaction answers the marker question from the same
			// transcript.CompactMarker scan trimTranscript itself performs, so
			// skipping here decides nothing differently; it just declines to
			// re-read to learn what was already read. Recorded through the
			// steady-state throttle: a never-compacted bubble is in this state
			// on every sweep, unchanged.
			m.recordTrim(b.Addr, trimResult{Outcome: trimNoBoundary, Path: path, Session: b.SessionID, BytesBefore: st.Bytes, BytesAfter: st.Bytes})
			continue
		}

		m.recordTrim(b.Addr, gatedTrim(path, b.SessionID, transcriptKeepBeforeCompact))
	}
}

// reportOversizedTranscript records that a's transcript has grown past
// transcriptOversizedBytes with no compaction boundary anywhere in it.
//
// DELIBERATELY NOT A TRUNCATION SITE. trimTranscript only ever cuts BEFORE the
// latest transcript.CompactMarker because everything after that marker is a
// self-contained conversation tree rooted at a parentUuid:null entry — the cut
// can never break a parentUuid chain a surviving entry depends on. A
// never-compacted file has no such boundary anywhere in it, so there is no
// byte or line offset that is safe to cut at: every surviving entry's
// parentUuid chain would run back through the discarded prefix, and severing
// it risks corrupting the conversation on --resume. Trading a token cost for a
// data-loss bug is strictly worse than the cost itself, so this function only
// ever counts and warns.
//
// The actual remedy already exists and runs elsewhere: Task 3's pump
// (contextpump.go, pumpContext) forces /compact once a HOT bubble's context
// crosses transcript.ContextForceTokens, which is what CREATES the compaction
// boundary this function is waiting for. Once that boundary exists, the next
// sweep's ordinary trimTranscript call reclaims the space safely. A bubble
// that stays cold and oversized indefinitely (no HOT window in which the pump
// could act) is exactly the case this function exists to surface, not silently
// swallow.
// The COUNTER is throttled together with the stderr line, deliberately. It
// counts REPORTS, not sweeps. Incremented per sweep it counted nothing but the
// sweep cadence: one parked bubble accrued ~30/hour for a single unchanged
// condition, and the TUI panel renders it beside genuine incident counters, so
// "OversizedTranscripts: 214" read as 214 events when it meant one file. The
// alternative — making it a gauge (Set 0/1) — was rejected because a gauge has
// to be cleared as well as set: every non-oversized bubble would need a Set(0)
// on every sweep to keep it truthful, widening the meter write surface from
// "the rare bad case" to "the whole fleet, always", and the field name is
// plural and additive like every other F* counter beside it.
//
// The parameter is `size`, not `bytes`: this file uses the bytes PACKAGE
// heavily (trimTranscript), and a parameter shadowing it here is one edit away
// from a confusing compile error in a function that must stay easy to read.
func (m *HealthManager) reportOversizedTranscript(a addr.Address, size int64) {
	m.reportMu.Lock()
	last, seen := m.lastOversizedReport[a]
	due := !seen || time.Since(last) >= oversizedReportThrottle
	if due {
		m.lastOversizedReport[a] = time.Now()
	}
	m.reportMu.Unlock()

	if due {
		m.k.Cost.Add(a, costmeter.FOversizedTranscripts, 1)
		fmt.Fprintf(os.Stderr, "bubbles: %s transcript is %d bytes with no compaction boundary — cannot safely trim (see reportOversizedTranscript); waiting for Task 3's pump to force /compact\n", a, size)
	}
}

// transcriptArchiveSuffix names the sidecar file that holds everything trimming
// has ever cut from a transcript: <transcript>.jsonl.archive. It is APPEND
// ONLY. Nothing in this repo removes bytes from it, prunes it, or ages it out —
// disk grows where it used to shrink, and that is the deliberate trade. An
// automatic pruner would reintroduce exactly the class of bug the archive
// exists to remove.
//
// The suffix keeps the ".jsonl" in the middle rather than replacing it so the
// archive sits next to its transcript in a directory listing, and cannot be
// resolved by convPath: session ids never end in ".jsonl.archive", so no scan
// in this package can mistake an archive for a session's conversation.
const transcriptArchiveSuffix = ".archive"

// Everything after transcript.CompactMarker is a self-contained conversation
// tree rooted at a parentUuid:null entry, so cutting before it never breaks
// the active thread. See internal/transcript for the single definition of
// what a compaction boundary looks like.

// trimLogKey throttles a trim record by bubble AND outcome, so a bubble whose
// outcome CHANGES is always announced immediately and only an unchanged
// steady-state condition is quieted.
type trimLogKey struct {
	addr    addr.Address
	outcome trimOutcome
}

// steadyStateTrimOutcomes are the outcomes a healthy fleet produces on every
// sweep forever: the bubble is in use, or its transcript has nothing to cut.
// Nothing was at stake in either, so they are recorded through a throttle,
// exactly as reportOversizedTranscript throttles its counter together with its
// warning: the sweep runs every 2 minutes, and a counter that climbs with the
// sweep cadence reads as an incident count when it means one unchanged fact
// ("OversizedTranscripts: 214" meant one file).
//
// Every OTHER outcome — a rewrite, a refusal on the identity or recency gates,
// an I/O failure, a registry pointing at a file that does not exist — is
// recorded EVERY time, unthrottled. Those are the ones this incident needed and
// did not have.
func steadyStateTrimOutcome(o trimOutcome) bool {
	return o == trimRefusedHot || o == trimNoBoundary
}

// recordTrim is the single place a trim attempt becomes visible: one log line
// carrying the path, the bubble, the session id it resolved, the size before
// and after, the bytes archived and the outcome — plus the matching counters.
//
// The line is deliberately reconstructible: given it alone, an operator can say
// what was on disk, what is on disk now, and where the difference went. The
// absence of exactly this line is why recovering the lost conversation took
// file-history archaeology.
func (m *HealthManager) recordTrim(a addr.Address, res trimResult) {
	if steadyStateTrimOutcome(res.Outcome) && !m.trimLogDue(a, res.Outcome) {
		return
	}

	errPart := ""
	if res.Err != nil {
		errPart = fmt.Sprintf(" err=%v", res.Err)
	}
	m.logf("bubbles: transcript trim outcome=%s addr=%s path=%s session=%s before=%d after=%d archived=%d%s\n",
		res.Outcome, a, res.Path, res.Session, res.BytesBefore, res.BytesAfter, res.BytesArchived, errPart)

	if res.Outcome == trimTrimmed {
		m.k.Cost.Add(a, costmeter.FTranscriptsTrimmed, 1)
		m.k.Cost.Add(a, costmeter.FTranscriptBytesArchived, res.BytesArchived)
		return
	}
	// Everything that is not a rewrite is a trim that did not happen. Counting
	// them together is deliberate: the question the counter answers is "how
	// often did this path decline to act", and the log line beside it says why.
	m.k.Cost.Add(a, costmeter.FTrimsRefused, 1)
}

// trimLogDue reports whether a steady-state outcome for a is due to be recorded
// again, claiming the window if so. Never called for the outcomes that matter.
func (m *HealthManager) trimLogDue(a addr.Address, o trimOutcome) bool {
	k := trimLogKey{addr: a, outcome: o}
	m.trimLogMu.Lock()
	defer m.trimLogMu.Unlock()
	last, seen := m.lastTrimLog[k]
	if seen && time.Since(last) < oversizedReportThrottle {
		return false
	}
	m.lastTrimLog[k] = time.Now()
	return true
}

// trimQuietPeriod is how long a transcript must have been untouched before it
// may be rewritten. A file being appended to must never be rewritten under its
// writer, WHATEVER THE KERNEL BELIEVES about hotness: IsHot is registry state,
// and registry state is exactly what proved unreliable (SetSessionID never
// bumps the fleet version, so a bubble can point at a session that moved on).
// The mtime is the file's own testimony and cannot go stale.
//
// 5 minutes is far longer than any gap between claude's appends within a turn,
// and far shorter than the idle period of a genuinely parked bubble, so the
// gate costs nothing real: a transcript refused for recency is simply trimmed
// on a later sweep.
const trimQuietPeriod = 5 * time.Minute

// trimIdentityTailBytes bounds the identity read. The gate needs the LAST
// sessionId in the file — the identity of whoever wrote most recently — so it
// reads the tail rather than the whole (possibly multi-MiB) transcript, and
// runs before any full read or any write. A transcript whose final 256 KiB
// carries no sessionId at all is refused, like any other file that cannot say
// whose it is.
const trimIdentityTailBytes = 256 << 10

// sessionIDKey is the field claude records the owning session under, on
// essentially every transcript entry.
var sessionIDKey = []byte(`"sessionId"`)

// gatedTrim is the ONLY path from the sweep to the trimming mechanism. It
// applies the gates that depend on the FILE rather than on registry state, and
// only then rewrites anything:
//
//	Gate A (recency)  — refuse a transcript modified within trimQuietPeriod.
//	Gate B (identity) — refuse unless the sessionId recorded INSIDE the
//	                    transcript is the session this bubble claims. No
//	                    sessionId found is also a refusal: unknown identity is
//	                    not permission.
//
// These ADD to the caller's IsHot check, they do not replace it. Both are cheap
// (a stat and a bounded tail read) and both run before trimTranscriptFile's
// os.ReadFile of the whole file, so a refusal costs almost nothing.
//
// wantSession is the registry's SessionID — the value that is NOT trustworthy.
// It is used only as the thing the file's own recorded identity must agree
// with; on disagreement the file wins and nothing is written.
func gatedTrim(path, wantSession string, keepBefore int) trimResult {
	// Session starts as the registry's claim and is replaced by the file's own
	// once one is found, so the log line always names a session and names the
	// surprising one whenever the two disagree.
	res := trimResult{Path: path, Session: wantSession}

	info, err := os.Stat(path)
	if err != nil {
		// A registry entry naming a file that does not exist is not an I/O
		// failure, it is the staleness this whole plan is about — four bubbles
		// are in that state today. It gets its own outcome so it can be seen
		// rather than swallowed as "trim error".
		res.Outcome = trimNoTranscript
		if !os.IsNotExist(err) {
			res.Outcome = trimFailed
			res.Err = err
		}
		return res
	}
	res.BytesBefore = info.Size()
	res.BytesAfter = info.Size()
	if time.Since(info.ModTime()) < trimQuietPeriod {
		res.Outcome = trimRefusedRecent
		return res
	}

	sid, err := transcriptSessionID(path)
	if err != nil {
		res.Outcome = trimFailed
		res.Err = err
		return res
	}
	if sid != "" {
		res.Session = sid
	}
	if sid == "" || sid != wantSession {
		res.Outcome = trimRefusedIdentity
		return res
	}

	out := trimTranscriptFile(path, keepBefore)
	out.Session = sid
	return out
}

// transcriptSessionID returns the last sessionId recorded in path's final
// trimIdentityTailBytes, or "" if the tail carries none. Byte scanning, not
// JSON decoding: the answer must not depend on the rest of an entry's shape
// staying parseable, and this runs on every trim attempt.
//
// The tail is read rather than the head because the question is "who is writing
// to this file NOW" — a transcript carried across a resume keeps its older
// entries, and it is the most recent writer whose file this is.
func transcriptSessionID(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := info.Size()
	if size == 0 {
		return "", nil
	}
	off := int64(0)
	if size > trimIdentityTailBytes {
		off = size - trimIdentityTailBytes
	}
	buf := make([]byte, size-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return "", err
	}
	return lastSessionID(buf), nil
}

// lastSessionID returns the value of the last `"sessionId": "..."` in b, or ""
// if there is none. A key whose value is not a plain string is skipped rather
// than guessed at.
func lastSessionID(b []byte) string {
	for i := bytes.LastIndex(b, sessionIDKey); i >= 0; i = bytes.LastIndex(b[:i], sessionIDKey) {
		rest := b[i+len(sessionIDKey):]
		j := 0
		for j < len(rest) && (rest[j] == ' ' || rest[j] == '\t') {
			j++
		}
		if j >= len(rest) || rest[j] != ':' {
			continue
		}
		j++
		for j < len(rest) && (rest[j] == ' ' || rest[j] == '\t') {
			j++
		}
		if j >= len(rest) || rest[j] != '"' {
			continue
		}
		j++
		end := bytes.IndexByte(rest[j:], '"')
		if end <= 0 {
			continue
		}
		return string(rest[j : j+end])
	}
	return ""
}

// trimOutcome names what one trim attempt actually did. Every attempt ends in
// exactly one of these, and every one of them is recorded: this incident cost a
// day of work precisely because trimming decided things silently.
type trimOutcome string

const (
	trimTrimmed         trimOutcome = "trimmed"          // the cut portion was archived and the live file replaced
	trimNoBoundary      trimOutcome = "no-boundary"      // no compaction marker, or nothing before the keep buffer
	trimRefusedRecent   trimOutcome = "refused-recent"   // modified within trimQuietPeriod (Gate A)
	trimRefusedIdentity trimOutcome = "refused-identity" // the file's own sessionId is not this bubble's (Gate B)
	trimRefusedHot      trimOutcome = "refused-hot"      // claude holds the file open
	trimNoTranscript    trimOutcome = "no-transcript"    // the registry names a file that does not exist
	trimFailed          trimOutcome = "error"            // an I/O error; see trimResult.Err
)

// trimResult is one attempt's full record: enough to reconstruct, from a log
// line alone, what was on disk before, what is on disk now, and where the
// difference went.
type trimResult struct {
	Outcome       trimOutcome
	Path          string
	Session       string // session id as resolved for/from the file ("" = none found)
	BytesBefore   int64
	BytesAfter    int64
	BytesArchived int64
	Err           error
}

// trimTranscript is the compatibility spelling of the mechanism: it cuts and
// archives, and reports only whether that failed. Callers that need to log or
// meter the outcome use trimTranscriptFile directly.
func trimTranscript(path string, keepBefore int) error {
	return trimTranscriptFile(path, keepBefore).Err
}

// trimTranscriptFile rewrites path, moving everything before
// (latestCompaction - keepBefore) lines into <path>.archive. No-op if there's
// no compaction yet or nothing meaningful to remove. Byte-exact in both
// directions: cut and kept lines are copied verbatim (we never re-serialize
// claude's JSON), so archive+live always reconstructs the original.
//
// ARCHIVE FIRST, REPLACE SECOND, NEVER THE OTHER ORDER. The cut portion is
// durably appended to the archive and only then is the live file replaced. If
// the archive write fails the live transcript is left completely untouched: a
// trim that half-succeeds is the bug this function was rewritten to remove.
// The reverse failure is survivable by construction — an archive append that
// lands while the replace fails leaves the same bytes in two places, which
// costs disk and loses nothing, and a retry next sweep re-appends rather than
// re-deletes.
//
// This function is the MECHANISM only. It assumes the caller has already
// established that rewriting this file is safe — see trimTranscripts, which
// holds the identity, recency and hotness gates. It MUST only run on a file no
// process holds open.
func trimTranscriptFile(path string, keepBefore int) trimResult {
	res := trimResult{Outcome: trimNoBoundary, Path: path}

	// An archive is append-only history that accumulates compaction markers by
	// construction, so anything that mistook one for a transcript would cut it
	// down — destroying exactly what it exists to hold. Nothing in this package
	// can name an archive (convPath builds from session ids), so this is a
	// backstop against a future caller, and it refuses loudly rather than
	// silently no-opping.
	if strings.HasSuffix(path, transcriptArchiveSuffix) {
		res.Outcome = trimFailed
		res.Err = fmt.Errorf("refusing to trim an archive: %s", path)
		return res
	}

	data, err := os.ReadFile(path)
	if err != nil {
		res.Outcome = trimFailed
		res.Err = err
		return res
	}
	res.BytesBefore = int64(len(data))
	res.BytesAfter = res.BytesBefore

	lines := bytes.SplitAfter(data, []byte{'\n'}) // keeps the trailing \n on each line
	latest := -1
	for i, l := range lines {
		if bytes.Contains(l, transcript.CompactMarker) {
			latest = i
		}
	}
	if latest < 0 {
		return res // no compaction boundary yet — nothing summarized away to clear
	}
	cut := latest - keepBefore
	if cut <= 0 {
		return res // the whole file is at/after the buffer — nothing to remove
	}

	var drop, keep bytes.Buffer
	drop.Grow(len(data))
	keep.Grow(len(data))
	for _, l := range lines[:cut] {
		drop.Write(l)
	}
	for _, l := range lines[cut:] {
		keep.Write(l)
	}

	if err := appendArchive(path+transcriptArchiveSuffix, drop.Bytes()); err != nil {
		// The live transcript has not been touched and must not be: the history
		// about to be cut has nowhere durable to go.
		res.Outcome = trimFailed
		res.Err = err
		return res
	}

	tmp := path + ".htrim"
	if err := os.WriteFile(tmp, keep.Bytes(), 0o644); err != nil {
		_ = os.Remove(tmp)
		res.Outcome = trimFailed
		res.Err = err
		return res
	}
	// Same final path/name → same session id → the bubble keeps writing to it.
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		res.Outcome = trimFailed
		res.Err = err
		return res
	}

	res.Outcome = trimTrimmed
	res.BytesArchived = int64(drop.Len())
	res.BytesAfter = int64(keep.Len())
	return res
}

// appendArchive durably appends b to the archive at path, creating it on first
// use. It is the only writer of archives in this repo, and it only ever grows
// them.
//
// The fsync is not decoration: the whole point of the archive is that it
// survives the crash that interrupts the trim, and an append still sitting in
// the page cache when the live file is replaced would not. A write that fails
// part-way is truncated back to the length it had on entry, so an archive never
// keeps a half line — a torn record would corrupt the very reconstruction the
// archive exists to make possible.
func appendArchive(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	start := info.Size()
	_, werr := f.Write(b)
	if werr == nil {
		werr = f.Sync()
	}
	if werr != nil {
		_ = f.Truncate(start) // best effort: never leave a torn record behind
		f.Close()
		return werr
	}
	return f.Close()
}
