package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

// Every launch writes up to two small JSON files into the system temp dir: the
// --mcp-config for the bubble's MCP server, and the --settings file wiring the
// session-id hook. Until now nothing ever removed either of them.
//
// Both names embed THIS process's pid, and that is load-bearing. Another
// bubbles daemon may be running on the same host against a different workspace,
// with live sessions whose claude processes were launched from its config
// files. A sweep that matched `bubbles-*` — or that reaped anything merely
// old-looking by mtime — would delete a live daemon's configs and cause an
// outage in a fleet it does not own. Pid ownership is the only safe key, and
// even within our own pid a file is only removed once no session holds that
// address.
const (
	mcpConfigStem = "bubbles-mcp-"
	settingsStem  = "bubbles-settings-"
	tempConfigExt = ".json"
)

// mcpConfigPath is where a bubble's --mcp-config lives.
func mcpConfigPath(dir string, pid int, a addr.Address) string {
	return filepath.Join(dir, fmt.Sprintf("%s%d-%s%s", mcpConfigStem, pid, a, tempConfigExt))
}

// settingsFilePath is where a bubble's --settings file lives.
func settingsFilePath(dir string, pid int, a addr.Address) string {
	return filepath.Join(dir, fmt.Sprintf("%s%d-%s%s", settingsStem, pid, a, tempConfigExt))
}

// ownsTempConfig reports whether name is a temp config written by pid.
//
// The trailing "-" after the pid is what makes this exact rather than a prefix
// match: without it, pid 4 would claim every file written by pid 42.
func ownsTempConfig(name string, pid int) bool {
	if !strings.HasSuffix(name, tempConfigExt) {
		return false
	}
	for _, stem := range []string{mcpConfigStem, settingsStem} {
		if strings.HasPrefix(name, fmt.Sprintf("%s%d-", stem, pid)) {
			return true
		}
	}
	return false
}

// sweepTempConfigs removes temp configs in dir that belong to pid and are not
// in keep (a set of base filenames still held by a live session). It returns
// the number of files removed.
//
// It is deliberately total: a missing directory, an unreadable entry or a file
// that vanished under it are all "nothing to do", never an error, because this
// runs forever on a timer and a housekeeping failure must not surface as fleet
// breakage.
func sweepTempConfigs(dir string, pid int, keep map[string]bool) int {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !ownsTempConfig(name, pid) || keep[name] {
			continue
		}
		if os.Remove(filepath.Join(dir, name)) == nil {
			removed++
		}
	}
	return removed
}

// liveTempConfigs returns the base filenames of the temp configs belonging to
// sessions this runner currently holds, PLUS those of any launch in flight.
// Anything in this set is off-limits to the sweep: the claude process launched
// from it is still around, or is about to be.
//
// The in-flight half is not optional. Launch writes its config files before
// pty.Start and registers the session only after it, so an address in that
// window is absent from r.sessions; a sweep that snapshotted only the map would
// delete a live launch's config and boot the bubble with no MCP server.
func (r *LocalRunner) liveTempConfigs() map[string]bool {
	pid := os.Getpid()
	dir := os.TempDir()
	keep := map[string]bool{}
	r.mu.Lock()
	addrs := make([]addr.Address, 0, len(r.sessions)+len(r.launching))
	for a := range r.sessions {
		addrs = append(addrs, a)
	}
	for a := range r.launching {
		addrs = append(addrs, a)
	}
	r.mu.Unlock()
	for _, a := range addrs {
		keep[filepath.Base(mcpConfigPath(dir, pid, a))] = true
		keep[filepath.Base(settingsFilePath(dir, pid, a))] = true
	}
	return keep
}

// removeTempConfigs deletes the temp configs for a. Called from Kill with r.mu
// held, after the address has been dropped from the session map and only when
// no launch of a is in flight — so neither a live session nor a launch that is
// mid-flight can be holding them.
func (r *LocalRunner) removeTempConfigs(a addr.Address) {
	pid, dir := os.Getpid(), os.TempDir()
	_ = os.Remove(mcpConfigPath(dir, pid, a))
	_ = os.Remove(settingsFilePath(dir, pid, a))
}

// SweepTempConfigs removes temp configs THIS process orphaned — a session that
// crashed, or a launch that failed before Kill could run. Files written by any
// other pid are left strictly alone.
//
// It takes no lock across any I/O: the live set is snapshotted under r.mu and
// the removals happen after it is released.
func (r *LocalRunner) SweepTempConfigs() int {
	return sweepTempConfigs(os.TempDir(), os.Getpid(), r.liveTempConfigs())
}
