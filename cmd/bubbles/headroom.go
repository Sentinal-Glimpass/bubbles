package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// Headroom (github.com/headroomlabs-ai/headroom) is a local compression proxy
// that shrinks what each claude session sends/receives — tool outputs, logs,
// conversation history — before it reaches the model. Bubbles runs ONE proxy for
// the whole fleet and points every session at it via a base-URL env var, which
// each launched claude inherits (cmd inherits os.Environ, same path as the AWS /
// Bedrock vars). Opt-in via --headroom / BUBBLES_HEADROOM=1; OFF by default.
//
// Design guarantees:
//   - Fail-open: the base URL is injected ONLY after /health is ready. If the
//     proxy never comes up (or `headroom` isn't installed), sessions talk to the
//     provider directly — the fleet never hard-depends on the proxy.
//   - Supervised: if the proxy crashes, it's relaunched on the same port so the
//     already-injected base URL keeps routing (bounded restarts).
//   - Backend-aware: subscription / API-key route via ANTHROPIC_BASE_URL; when
//     CLAUDE_CODE_USE_BEDROCK=1 the proxy runs --backend bedrock (it re-signs
//     SigV4) and routing uses ANTHROPIC_BEDROCK_BASE_URL. Both are honored by
//     Claude Code.
type headroomProc struct {
	url      string
	port     int
	stopping atomic.Bool
	proc     atomic.Pointer[os.Process]
}

// headroomPort is the proxy's loopback port (BUBBLES_HEADROOM_PORT, default 8787).
func headroomPort() int {
	if v := os.Getenv("BUBBLES_HEADROOM_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 8787
}

// startHeadroom launches the compression proxy when BUBBLES_HEADROOM=1. Returns
// nil (a no-op) when disabled or when `headroom` isn't on PATH. Non-blocking:
// the proxy warms up in the background; call routeWhenReady before launching
// bubbles to gate env injection on health.
func startHeadroom(baseDir string) *headroomProc {
	if os.Getenv("BUBBLES_HEADROOM") != "1" {
		return nil
	}
	bin, err := exec.LookPath("headroom")
	if err != nil {
		fmt.Fprintln(os.Stderr, "bubbles: --headroom set but `headroom` not found on PATH — "+
			"install it (uv tool install \"headroom-ai[proxy]\") then restart; running WITHOUT compression")
		return nil
	}
	port := headroomPort()
	h := &headroomProc{url: fmt.Sprintf("http://127.0.0.1:%d", port), port: port}

	logPath := filepath.Join(baseDir, ".bubbles", "headroom.log")
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)

	args := []string{"proxy", "--port", strconv.Itoa(port)}
	if os.Getenv("CLAUDE_CODE_USE_BEDROCK") == "1" {
		// The proxy re-signs SigV4, so it can compress and forward direct to AWS.
		args = append(args, "--backend", "bedrock")
	}
	// Optimization mode: "cache" freezes prior turns to maximize provider
	// prefix-cache hits (best net $ for a cache-heavy fleet); "token" rewrites
	// them for max compression (may bust cache). The dashboard's cost-savings %
	// nets both — flip this and watch which wins. Default: headroom's own default.
	if m := os.Getenv("BUBBLES_HEADROOM_MODE"); m != "" {
		args = append(args, "--mode", m)
	}
	if os.Getenv("HEADROOM_OUTPUT_SHAPER") == "" {
		// Trim what the model writes back (verbosity steering + effort routing on
		// tool-result resumes). Output tokens cost ~5x input on Opus-class models.
		_ = os.Setenv("HEADROOM_OUTPUT_SHAPER", "1")
	}

	if !h.launch(bin, args, logPath) {
		return nil
	}
	go h.supervise(bin, args, logPath)
	return h
}

// launch starts one proxy process, redirecting its output to logPath. Records
// the process handle for supervision/shutdown. Returns false on spawn failure.
func (h *headroomProc) launch(bin string, args []string, logPath string) bool {
	cmd := exec.Command(bin, args...)
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
		cmd.Stdout, cmd.Stderr = f, f
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "bubbles: headroom proxy failed to start: %v (running WITHOUT compression)\n", err)
		return false
	}
	h.proc.Store(cmd.Process)
	go func() { _ = cmd.Wait() }() // reap so a crashed proxy doesn't linger as a zombie
	return true
}

// supervise relaunches the proxy if it exits unexpectedly, so the injected base
// URL keeps resolving. Backs off and gives up after too many rapid failures
// (fail-open: sessions then fall back to whatever the env last held).
func (h *headroomProc) supervise(bin string, args []string, logPath string) {
	fails := 0
	for !h.stopping.Load() {
		time.Sleep(2 * time.Second)
		p := h.proc.Load()
		if p == nil {
			return
		}
		// Signal 0 probes liveness without affecting the process.
		if err := p.Signal(syscall.Signal(0)); err == nil {
			fails = 0
			continue // still alive
		}
		if h.stopping.Load() {
			return
		}
		fails++
		if fails > 5 {
			fmt.Fprintln(os.Stderr, "bubbles: headroom proxy keeps crashing — giving up (see .bubbles/headroom.log)")
			return
		}
		fmt.Fprintf(os.Stderr, "bubbles: headroom proxy exited, relaunching (attempt %d)\n", fails)
		h.launch(bin, args, logPath)
	}
}

// routeWhenReady blocks up to timeout for the proxy's /health to report ready,
// then injects the base-URL env so every subsequently-launched claude routes
// through it. Returns false (WITHOUT setting env) if the proxy isn't ready in
// time — the fail-open path. Safe to call once, before bubbles are launched.
func (h *headroomProc) routeWhenReady(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if h.healthy() {
			key := "ANTHROPIC_BASE_URL"
			if os.Getenv("CLAUDE_CODE_USE_BEDROCK") == "1" {
				key = "ANTHROPIC_BEDROCK_BASE_URL"
			}
			_ = os.Setenv(key, h.url)
			// With a custom base URL, Claude Code otherwise inlines every MCP tool
			// schema into context. Keep schema deferral on (bubbles loads many MCP
			// servers — mechmagnet alone is 24 tools).
			_ = os.Setenv("ENABLE_TOOL_SEARCH", "true")
			fmt.Fprintf(os.Stderr, "bubbles: token compression ON — fleet routing through headroom at %s (%s)\n", h.url, key)
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Fprintf(os.Stderr, "bubbles: headroom proxy not ready within %s — running WITHOUT compression (fail-open)\n", timeout)
	return false
}

// healthy reports whether the proxy's /health endpoint is up and ready.
func (h *headroomProc) healthy() bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(h.url + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return strings.Contains(string(body), "\"ready\":true")
}

// stop terminates the proxy and disables the supervisor's restart. Called on
// app shutdown.
func (h *headroomProc) stop() {
	if h == nil {
		return
	}
	h.stopping.Store(true)
	if p := h.proc.Load(); p != nil {
		_ = p.Kill()
	}
}
