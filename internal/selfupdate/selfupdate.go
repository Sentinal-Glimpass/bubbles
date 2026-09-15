// Package selfupdate keeps the installed bubbles binary current from GitHub,
// automatically and safely. Policy: in the background, fetch and BUILD the tip
// of main and install it over the on-disk binary — but NEVER restart a running
// fleet to apply it. A restart interrupts every in-flight agent turn, so
// applying stays tied to the operator's next natural restart (surfaced as a
// nudge). Everything network/process fails open: any error leaves the current
// binary untouched.
//
// Comparison is by COMMIT, not semver, because bubbles ships from main: a
// `go install …@main` build carries a pseudo-version whose sha we compare to
// main's HEAD on GitHub. The pure helpers here are unit-tested.
package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
)

const (
	ModulePath = "github.com/Sentinal-Glimpass/bubbles"
	CommitsAPI = "https://api.github.com/repos/Sentinal-Glimpass/bubbles/commits/main"
)

// CurrentRevision is the commit this binary was built from, or "" if unknown.
// vcs.revision is set for local `go build`/`make`; a `go install …@main` build
// has no vcs setting but encodes the sha in its pseudo-version's last segment.
func CurrentRevision() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return pseudoSHA(bi.Main.Version)
}

// pseudoSHA pulls the 12-char commit sha out of a Go pseudo-version like
// v0.3.2-0.20260915120000-abcdef123456. Returns "" for a clean tag or "".
func pseudoSHA(v string) string {
	i := strings.LastIndex(v, "-")
	if i < 0 || i+1 >= len(v) {
		return ""
	}
	if cand := v[i+1:]; isHex(cand) && len(cand) >= 7 {
		return cand
	}
	return ""
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// SameCommit reports whether two revisions denote the same commit, tolerating a
// short sha vs a full one (prefix match, case-insensitive). Under 7 hex chars is
// treated as unknown → never "same".
func SameCommit(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	if len(a) < 7 || len(b) < 7 {
		return false
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	return strings.HasPrefix(b, a)
}

// ExtractSHA finds the longest hex run (>=7) in a `bubbles version` line, so the
// installed binary's revision can be read back from its own output.
func ExtractSHA(s string) string {
	best := ""
	cur := strings.Builder{}
	flush := func() {
		if cur.Len() >= 7 && cur.Len() > len(best) {
			best = cur.String()
		}
		cur.Reset()
	}
	for _, r := range s {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return best
}

// DueForCheck rate-limits network checks: true if never checked or the interval
// has elapsed.
func DueForCheck(last, now time.Time, interval time.Duration) bool {
	return last.IsZero() || now.Sub(last) >= interval
}

// FetchLatestCommit returns main's HEAD sha on GitHub. Fail-open.
func FetchLatestCommit(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, CommitsAPI, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github commits: status %d", resp.StatusCode)
	}
	var c struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		return "", err
	}
	if len(c.SHA) < 7 {
		return "", fmt.Errorf("github returned no sha")
	}
	return c.SHA, nil
}

// RevisionOf runs a bubbles binary's `version` and reads back the commit it was
// built from — used to check the ON-DISK binary, which may be newer than the
// running daemon after a prior auto-update.
func RevisionOf(binary string) string {
	out, err := exec.Command(binary, "version").CombinedOutput()
	if err != nil {
		return ""
	}
	return ExtractSHA(string(out))
}

// Apply builds main's tip and atomically installs it over installDir/bubbles,
// but only after VERIFYING the freshly built binary runs and reports wantSHA —
// a broken or wrong build never replaces a working install. The temp dir is
// inside installDir so the final rename is atomic (one filesystem).
func Apply(installDir, wantSHA string) error {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return fmt.Errorf("go toolchain not found: %w", err)
	}
	tmp, err := os.MkdirTemp(installDir, ".bubbles-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	cmd := exec.Command(goBin, "install", ModulePath+"/cmd/bubbles@main")
	cmd.Env = append(os.Environ(), "GOBIN="+tmp)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go install @main failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	built := filepath.Join(tmp, "bubbles")
	got := RevisionOf(built)
	if got == "" {
		return fmt.Errorf("built binary did not run / report a version")
	}
	if !SameCommit(got, wantSHA) {
		return fmt.Errorf("built binary is %s, expected %s", got, wantSHA)
	}
	return os.Rename(built, filepath.Join(installDir, "bubbles"))
}
