package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/kernel"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
)

// webhookPort is where the daemon's incoming-webhook server listens. It is
// STABLE per-workspace (derived from the workspace dir) so issued webhook URLs —
// which embed the port — survive daemon restarts, and two workspaces on one host
// don't collide on a shared default. Override explicitly with BUBBLES_WEBHOOK_PORT.
func webhookPort(baseDir string) int {
	if v := os.Getenv("BUBBLES_WEBHOOK_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 65536 {
			return n
		}
	}
	abs, err := filepath.Abs(baseDir)
	if err != nil {
		abs = baseDir
	}
	sum := sha256.Sum256([]byte(abs))
	// Deterministic port in 20000–39999: the same workspace always lands here, so
	// its /c/<token> URLs keep resolving across restarts.
	return 20000 + int(binary.BigEndian.Uint16(sum[:2]))%20000
}

// startWebhookServer binds the incoming-webhook listener and serves it. By
// default it binds 127.0.0.1 (only local crons/scripts can hit it); `bubbles
// --webhook-public` binds 0.0.0.0 for internet-facing use (put your firewall or
// tunnel in front — the URL token is the auth). Returns the advertised base URL,
// or "" if the port was unavailable (another fleet may own it), in which case
// the webhook tools report unavailable rather than handing out dead URLs.
func startWebhookServer(k *kernel.Kernel, baseDir string) string {
	bind, public := "127.0.0.1", false
	if os.Getenv("BUBBLES_WEBHOOK_PUBLIC") == "1" {
		bind, public = "0.0.0.0", true
	}
	port := webhookPort(baseDir)
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", bind, port))
	if err != nil {
		// Stable port busy (e.g. a stale daemon still holds it) — bind :0 so this
		// fleet still gets a working webhook server rather than disabling webhooks
		// entirely. NOTE: a :0 port is NOT stable across restarts, so issued URLs
		// will go stale until the stable port is free again — warn loudly.
		ln, err = net.Listen("tcp", fmt.Sprintf("%s:0", bind))
		if err != nil {
			fmt.Fprintf(os.Stderr, "bubbles: webhook server disabled: %v\n", err)
			return ""
		}
		port = ln.Addr().(*net.TCPAddr).Port
		fmt.Fprintf(os.Stderr, "bubbles: WARNING stable webhook port %d busy — using %d (issued URLs will change on restart; free the stable port to keep them durable)\n", webhookPort(baseDir), port)
	}
	base := os.Getenv("BUBBLES_WEBHOOK_BASE") // e.g. https://ops.example.com/hooks (a reverse proxy to this port)
	if base == "" {
		host := "127.0.0.1"
		if public {
			if h, err := os.Hostname(); err == nil && h != "" {
				host = h
			}
		}
		base = fmt.Sprintf("http://%s:%d", host, port)
	}
	base = strings.TrimRight(base, "/")

	limiter := newRateLimiter(1, 20) // ~1 req/s sustained, burst 20, per token
	mux := http.NewServeMux()
	mux.HandleFunc("/w/", func(w http.ResponseWriter, r *http.Request) { handleWebhook(k, limiter, w, r) })
	mux.HandleFunc("/c/", func(w http.ResponseWriter, r *http.Request) { handleControl(k, limiter, w, r) })
	srv := &http.Server{
		Handler:           mux,
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	return base
}

// webhookPayload is the optional JSON body of a webhook POST. A non-JSON body is
// treated as the raw message body instead.
type webhookPayload struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
	From    string `json:"from"`   // display label for the sender, e.g. "stripe", "ci"
	Urgent  *bool  `json:"urgent"` // default true: a programmatic poke should wake the bubble
}

// handleWebhook turns POST /w/<token> into a message delivered to the owning
// bubble (waking it if cold). The unguessable token IS the auth. Responses never
// disclose fleet topology (no bubble address, on success or error).
func handleWebhook(k *kernel.Kernel, limiter *rateLimiter, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"ok":false,"err":"POST only"}`, http.StatusMethodNotAllowed)
		return
	}
	token := strings.TrimPrefix(r.URL.Path, "/w/")
	target, ok := k.ResolveWebhookToken(token)
	if !ok {
		http.Error(w, `{"ok":false,"err":"unknown webhook"}`, http.StatusNotFound)
		return
	}
	if !limiter.allow(token, time.Now()) {
		http.Error(w, `{"ok":false,"err":"rate limited"}`, http.StatusTooManyRequests)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 256<<10)) // bound hostile payloads
	if err != nil {
		http.Error(w, `{"ok":false,"err":"read body"}`, http.StatusBadRequest)
		return
	}

	// query params work for bare `curl -d "text" url?subject=ci`; a JSON body
	// with subject/body/from/urgent overrides them.
	q := r.URL.Query()
	subject, body, source := q.Get("subject"), string(raw), q.Get("from")
	urgent := q.Get("urgent") != "false"
	var p webhookPayload
	if json.Unmarshal(raw, &p) == nil && (p.Subject != "" || p.Body != "") {
		if p.Subject != "" {
			subject = p.Subject
		}
		body = p.Body
		if p.From != "" {
			source = p.From
		}
		if p.Urgent != nil {
			urgent = *p.Urgent
		}
	}
	if subject == "" {
		subject = "webhook"
	}
	if source == "" {
		source = "external"
	}

	id, err := k.WebhookDeliver(target, source, subject, body, urgent)
	if err != nil {
		http.Error(w, `{"ok":false,"err":"target gone"}`, http.StatusGone)
		return
	}
	// success carries the inbox message id only — never the bubble address, so a
	// caller with the token can't map the token to fleet topology.
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"id":%d}`+"\n", id)
}

// controlPayload is the JSON body of a POST /c/<token> fleet action.
type controlPayload struct {
	Action      string `json:"action"`      // "spawn" | "delete" | "list"
	Name        string `json:"name"`        // spawn: child label
	Description string `json:"description"` // spawn: child charter/first prompt
	Dir         string `json:"dir"`         // spawn: working dir (abs, or relative to the workspace)
	Model       string `json:"model"`       // spawn: "sonnet" | "opus" | "fable"
	Target      string `json:"target"`      // delete: address to remove (must be in the caller's subtree)
}

// handleControl turns POST /c/<token> into a fleet action executed AS the
// token's owning bubble, with that bubble's spawn/manage authority. Unlike the
// message webhook, control responses DO return addresses: the token is a
// spawn-capable secret held by the operator/script, so it's an authenticated
// management API, not an anonymous notification sink.
func handleControl(k *kernel.Kernel, limiter *rateLimiter, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"ok":false,"err":"POST only"}`, http.StatusMethodNotAllowed)
		return
	}
	token := strings.TrimPrefix(r.URL.Path, "/c/")
	by, ok := k.ResolveControlToken(token)
	if !ok {
		http.Error(w, `{"ok":false,"err":"unknown control webhook"}`, http.StatusNotFound)
		return
	}
	if !limiter.allow(token, time.Now()) {
		http.Error(w, `{"ok":false,"err":"rate limited"}`, http.StatusTooManyRequests)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		http.Error(w, `{"ok":false,"err":"read body"}`, http.StatusBadRequest)
		return
	}
	var p controlPayload
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			http.Error(w, `{"ok":false,"err":"invalid JSON body"}`, http.StatusBadRequest)
			return
		}
	}
	if p.Action == "" {
		p.Action = r.URL.Query().Get("action") // allow ?action=list for a bare GET-style poke via POST
	}

	w.Header().Set("Content-Type", "application/json")
	switch p.Action {
	case "spawn":
		if p.Name == "" {
			http.Error(w, `{"ok":false,"err":"spawn needs a name"}`, http.StatusBadRequest)
			return
		}
		if len(p.Name) > maxBubbleName {
			http.Error(w, `{"ok":false,"err":"name too long"}`, http.StatusBadRequest)
			return
		}
		dir := p.Dir
		if dir == "" {
			dir = filepath.Join(defaultWorkspace(), p.Name)
		} else if !filepath.IsAbs(dir) {
			dir = filepath.Join(defaultWorkspace(), dir)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			http.Error(w, `{"ok":false,"err":"cannot create dir"}`, http.StatusInternalServerError)
			return
		}
		a, err := k.Spawn(by, "", dir, runner.SpawnOpts{Name: p.Name, Goal: p.Description, Model: p.Model})
		if err != nil {
			writeControlErr(w, err)
			return
		}
		// Hand back the child's address + its own message webhook, so a script can
		// immediately target or wire up what it just created.
		hook, _ := k.WebhookURLBy(by, a)
		fmt.Fprintf(w, `{"ok":true,"addr":%q,"webhook":%q}`+"\n", a.String(), hook)
	case "delete":
		if p.Target == "" {
			http.Error(w, `{"ok":false,"err":"delete needs a target address"}`, http.StatusBadRequest)
			return
		}
		victims, err := k.DeleteBy(by, addr.Address(p.Target))
		if err != nil {
			writeControlErr(w, err)
			return
		}
		fmt.Fprintf(w, `{"ok":true,"removed":%d}`+"\n", len(victims))
	case "list":
		var b strings.Builder
		b.WriteString(`{"ok":true,"children":[`)
		for i, c := range k.Reg.Children(by) {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"addr":%q,"name":%q,"disabled":%t}`, c.Addr.String(), c.Label(), c.Disabled)
		}
		b.WriteString("]}\n")
		_, _ = io.WriteString(w, b.String())
	default:
		http.Error(w, `{"ok":false,"err":"action must be spawn, delete, or list"}`, http.StatusBadRequest)
	}
}

// writeControlErr maps a kernel error to a control response without leaking
// internals: a permission/topology error is a clean 403.
func writeControlErr(w http.ResponseWriter, err error) {
	if err == kernel.ErrNotAllowed || strings.Contains(err.Error(), "not permitted") || strings.Contains(err.Error(), "budget") {
		http.Error(w, `{"ok":false,"err":"not permitted"}`, http.StatusForbidden)
		return
	}
	http.Error(w, fmt.Sprintf(`{"ok":false,"err":%q}`, err.Error()), http.StatusBadRequest)
}

// rateLimiter is a per-token token-bucket, bounding webhook floods (per bubble).
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens refilled per second
	burst   float64 // bucket capacity
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(ratePerSec, burst float64) *rateLimiter {
	return &rateLimiter{buckets: map[string]*bucket{}, rate: ratePerSec, burst: burst}
}

// allow consumes one token for key, refilling by elapsed time. Buckets are
// created only for already-validated tokens, so the map is bounded by live
// webhooks (a bad token 404s before reaching here).
func (rl *rateLimiter) allow(key string, now time.Time) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b := rl.buckets[key]
	if b == nil {
		rl.buckets[key] = &bucket{tokens: rl.burst - 1, last: now}
		return true
	}
	b.tokens += now.Sub(b.last).Seconds() * rl.rate
	if b.tokens > rl.burst {
		b.tokens = rl.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
