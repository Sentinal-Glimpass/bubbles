package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// modulePath is bubbles' Go module path — the thing `go install …@main`
// resolves against. Kept next to the updater because that is its only use.
const modulePath = "github.com/Sentinal-Glimpass/bubbles"

// installerURL is the one-command installer, used as the fallback when the Go
// toolchain is not present to rebuild from source.
const installerURL = "https://raw.githubusercontent.com/Sentinal-Glimpass/bubbles/main/install.sh"

// goInstallArgs is the argument vector that rebuilds the latest bubbles with the
// Go toolchain. Pure, so a test can assert it without invoking go.
//
// The version suffix is @main, NOT @latest, and that difference is the whole
// point: `go install …@latest` resolves to the highest semver TAG, not the tip
// of the branch. bubbles ships from main and tags only occasionally, so @latest
// silently pinned everyone to a months-old tag (v0.1.1) that predated half the
// codebase. @main always builds the newest commit on the default branch, which
// is what "update" has to mean here.
func goInstallArgs() []string {
	return []string{"install", modulePath + "/cmd/bubbles@main"}
}

// updateStrategy names how `--update` will rebuild, given what's on PATH. Go is
// preferred (rebuilds just bubbles, in place); the installer is the fallback
// because it also bootstraps a missing Go toolchain. Pure and total so the
// decision is unit-tested rather than discovered at runtime.
type updateStrategy int

const (
	updateNoTool updateStrategy = iota // neither go nor curl: cannot self-update
	updateViaGo                        // `go install …@main`
	updateViaInstaller                 // `curl … | bash`
)

func chooseUpdateStrategy(hasGo, hasCurl bool) updateStrategy {
	switch {
	case hasGo:
		return updateViaGo
	case hasCurl:
		return updateViaInstaller
	default:
		return updateNoTool
	}
}

// updateInstallDir is where the rebuilt binary must land: the directory of the
// CURRENTLY RUNNING executable, so `--update` replaces the bubbles you invoked
// wherever it lives (~/.local/bin, $(go env GOPATH)/bin, …) rather than some
// other copy earlier on PATH. Symlinks are resolved so GOBIN is the real dir.
func updateInstallDir() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, rerr := filepath.EvalSymlinks(self); rerr == nil {
		self = resolved
	}
	return filepath.Dir(self), nil
}

// runUpdate rebuilds bubbles to the latest published commit, in place. It is the
// answer to "how do I get the newest bubbles without remembering the install
// incantation": `bubbles --update`.
//
// It deliberately does NOT restart the daemon. A rebuild replaces the on-disk
// binary but the running daemon keeps executing the old inode, so the update is
// inert until a restart — and restarting is disruptive enough (it interrupts
// every in-flight bubble turn) that it must stay the operator's explicit choice.
// So the command ends by printing exactly how to apply it.
func runUpdate() {
	dir, err := updateInstallDir()
	if err != nil {
		fatal(fmt.Errorf("cannot locate the running binary to update it: %w", err))
	}

	goBin, goErr := exec.LookPath("go")
	curlBin, curlErr := exec.LookPath("curl")
	switch chooseUpdateStrategy(goErr == nil, curlErr == nil) {
	case updateViaGo:
		fmt.Fprintf(os.Stdout, "bubbles: rebuilding the latest version with %s → %s\n", goBin, dir)
		cmd := exec.Command(goBin, goInstallArgs()...)
		// GOBIN sends the built binary to the running binary's own directory;
		// go install writes to a temp file and renames into place, which Linux
		// permits even over the busy executable running this very command.
		cmd.Env = append(os.Environ(), "GOBIN="+dir)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if runErr := cmd.Run(); runErr != nil {
			fatal(fmt.Errorf("go install failed (need Go 1.25+ and network access): %w", runErr))
		}
	case updateViaInstaller:
		fmt.Fprintln(os.Stdout, "bubbles: Go not found — running the one-command installer instead")
		cmd := exec.Command("bash", "-c", curlBin+" -fsSL "+installerURL+" | bash")
		cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
		if runErr := cmd.Run(); runErr != nil {
			fatal(fmt.Errorf("installer failed: %w", runErr))
		}
	default:
		fatal(errors.New("cannot self-update: neither `go` (1.25+) nor `curl` is on PATH — install one and retry, or see the README install options"))
	}

	fmt.Fprintf(os.Stdout, "bubbles: updated → %s\n", filepath.Join(dir, "bubbles"))
	fmt.Fprintln(os.Stdout, updateRestartHint(defaultWorkspace()))
}

// updateRestartHint tells the operator what, if anything, to do so the freshly
// built binary actually runs. Pure given the workspace: it reports whether a
// live daemon here is still on the old binary.
func updateRestartHint(baseDir string) string {
	if daemonAlive(baseDir) {
		return "A fleet is running here on the OLD binary — restart it to apply the update:\n    bubbles stop && bubbles"
	}
	return "No fleet is running here; the next `bubbles` you start will use the update."
}

// daemonAlive reports whether the workspace's recorded daemon pid is a live
// process. A stale pidfile (crash, reboot) reads as not-alive, the same test
// runStop makes.
func daemonAlive(baseDir string) bool {
	data, err := os.ReadFile(pidFile(baseDir))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
