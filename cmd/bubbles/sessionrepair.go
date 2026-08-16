package main

import (
	"os"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/kernel"
)

// reconcileSessionIDs clears every stored SessionID that names a transcript
// which is not on disk, and returns how many it cleared.
//
// WHY THIS EXISTS. Until the launch path started marking the fleet dirty (see
// registry.RecordSessionID), a new session id reached fleet.json only if some
// unrelated change happened to bump the version while the bubble was still hot.
// Fleets saved before that fix therefore hold ids that were already superseded
// when they were written, some of which name a conversation file that never
// existed at all. Resuming one of those either fails ("No conversation found",
// after paying for the attempt) or — worse, if the id was recycled — resumes
// the WRONG conversation. Both leave the bubble's real work orphaned.
//
// WHAT IT DELIBERATELY DOES NOT DO. It never guesses a replacement id from the
// other transcripts in the project folder: resuming somebody else's
// conversation is strictly worse than starting clean, and the operator has
// backups from which a specific id can be restored deliberately. It never
// deletes, rewrites, moves or truncates a transcript — the only mutation is to
// registry state, and only ever to the empty string. A bubble whose transcript
// IS present is left completely untouched.
//
// A clear is a genuine change to durable fleet state, so it goes through
// RecordSessionID (dirty) rather than SetSessionIDForSave. It
// runs ONCE at startup, not on a sweep, so there is no autosave feedback here.
//
// It must run BEFORE any listener that can deliver a message is bound (see the
// ordering block in app.go): a delivery racing this either resumes a phantom id
// or has a freshly minted, not-yet-flushed id cleared under it.
//
// No registry lock is held across the os.Stat: the addresses are snapshotted
// through locked accessors first, and all file I/O happens afterwards. Nothing
// here wakes a bubble (no EnsureAlive) — a cold bubble stays cold and simply
// starts a fresh session the next time it is genuinely needed.
func reconcileSessionIDs(k *kernel.Kernel, home string, logf func(string, ...any)) int {
	if k == nil || home == "" {
		return 0
	}
	type entry struct {
		a        addr.Address
		dir, sid string
	}
	var snap []entry
	for _, b := range k.Reg.All() {
		a := b.Addr // the map key, and the only field read off the pointer
		sid, _ := k.Reg.SessionID(a)
		dir, _ := k.Reg.Dir(a)
		if sid == "" || dir == "" { // never launched, or nowhere to resolve a path from
			continue
		}
		snap = append(snap, entry{a: a, dir: dir, sid: sid})
	}

	cleared := 0
	for _, e := range snap {
		path := convPath(home, e.dir, e.sid)
		_, err := os.Stat(path)
		if err == nil {
			continue // the conversation is there: leave it entirely alone
		}
		if !os.IsNotExist(err) {
			// Unreadable is NOT the same as absent (a permission problem, a
			// mount not up yet, a path component that is not a directory).
			// Absence must be PROVEN. Clearing on a stat we could not complete
			// would throw away a working pointer to a live conversation, so the
			// id is kept and the operator gets the line.
			logf("bubbles: session reconcile: addr=%s session=%s path=%s stat failed: %v — id KEPT (an unreadable path is not evidence the conversation is gone)\n",
				e.a, e.sid, path, err)
			continue
		}
		// Logged BEFORE the clear, with everything needed to go find the real
		// conversation in a backup: which bubble, which id, which path.
		//
		// The path is announced as dir-derived because it is: convPath slugs the
		// bubble's CURRENT Dir, so a bubble whose directory moved since its
		// transcript was written is looked up under the new slug and reported
		// missing even though the file is sitting under the old one. The id is
		// still cleared — the bubble genuinely cannot resume from where it now
		// lives — but an operator hunting a backup must know to search by id
		// across project folders rather than trusting this path.
		logf("bubbles: session reconcile: addr=%s session=%s path=%s (dir-derived from dir=%s; stale if this bubble's directory moved) does not exist — clearing stored id; this bubble will start a FRESH session (no transcript was touched)\n",
			e.a, e.sid, path, e.dir)
		if k.Reg.RecordSessionID(e.a, "") {
			cleared++
		}
	}
	return cleared
}
