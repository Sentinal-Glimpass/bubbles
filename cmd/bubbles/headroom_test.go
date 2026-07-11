package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// TestStartHeadroomReusesHealthyProxy: when a healthy proxy is already listening
// on the port (another workspace's daemon owns it), startHeadroom must reuse it
// instead of spawning a competitor that can't bind and crash-loops.
func TestStartHeadroomReusesHealthyProxy(t *testing.T) {
	// Stand in for an already-running proxy on 127.0.0.1:<port>.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"status":"healthy","ready":true}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	addr := srv.Listener.Addr().String()
	p := addr[strings.LastIndex(addr, ":")+1:]
	t.Setenv("BUBBLES_HEADROOM", "1")
	t.Setenv("BUBBLES_HEADROOM_PORT", p)

	h := startHeadroom(t.TempDir())
	if h == nil {
		t.Fatal("startHeadroom returned nil with headroom enabled")
	}
	// Reused proxy: healthy, but we don't own the process (nil proc), so stop() is
	// a no-op and never kills the other daemon's proxy.
	if !h.healthy() {
		t.Fatal("reused proxy should report healthy")
	}
	if got, _ := strconv.Atoi(p); h.port != got {
		t.Fatalf("port = %d, want %s", h.port, p)
	}
	if pr := h.proc.Load(); pr != nil {
		t.Fatal("reused proxy must not own a process")
	}
	h.stop() // must not panic and must not kill the external server
	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("external proxy died after stop(): %v", err)
	}
	resp.Body.Close()
}
