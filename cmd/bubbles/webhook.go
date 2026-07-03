package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/kernel"
)

// webhookPort is where the daemon's incoming-webhook server listens.
// Default 8899; override with `bubbles --webhook-port N`.
func webhookPort() int {
	if v := os.Getenv("BUBBLES_WEBHOOK_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 65536 {
			return n
		}
	}
	return 8899
}

// startWebhookServer binds the incoming-webhook listener and serves it. By
// default it binds 127.0.0.1 (only local crons/scripts can hit it); `bubbles
// --webhook-public` binds 0.0.0.0 for internet-facing use (put your firewall or
// tunnel in front — the URL token is the auth). Returns the advertised base URL,
// or "" if the port was unavailable (another fleet may own it), in which case
// the webhook tools report unavailable rather than handing out dead URLs.
func startWebhookServer(k *kernel.Kernel) string {
	bind, public := "127.0.0.1", false
	if os.Getenv("BUBBLES_WEBHOOK_PUBLIC") == "1" {
		bind, public = "0.0.0.0", true
	}
	port := webhookPort()
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", bind, port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "bubbles: webhook server disabled: %v\n", err)
		return ""
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
