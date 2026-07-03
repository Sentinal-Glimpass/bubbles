package main

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/kernel"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
)

// TestWebhookHTTPEndToEnd: a real POST to the bubble's URL lands in its inbox
// and wakes it; bad tokens 404; raw bodies + query params work; the token
// survives a fleet save/restore.
func TestWebhookHTTPEndToEnd(t *testing.T) {
	// pick a free port for the server (avoid colliding with a live fleet's 8899)
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()
	t.Setenv("BUBBLES_WEBHOOK_PORT", strconv.Itoa(port))
	t.Setenv("BUBBLES_WEBHOOK_PUBLIC", "")
	t.Setenv("BUBBLES_WEBHOOK_BASE", "")

	fr := runner.NewFake()
	k := kernel.New(fr)
	k.RelaunchProbe = 0
	a, _ := k.Spawn(addr.Root, "", t.TempDir(), runner.SpawnOpts{Name: "watcher"})

	base := startWebhookServer(k)
	if base == "" {
		t.Fatal("webhook server failed to start")
	}
	k.WebhookBase = base
	url, err := k.WebhookURL(a)
	if err != nil {
		t.Fatal(err)
	}

	post := func(u, body, ctype string) (*http.Response, error) {
		return http.Post(u, ctype, strings.NewReader(body))
	}
	waitUp := time.Now().Add(2 * time.Second)
	for { // wait for the listener goroutine
		if r, err := post(base+"/w/bogus", "x", "text/plain"); err == nil {
			r.Body.Close()
			if r.StatusCode != http.StatusNotFound {
				t.Fatalf("bogus token = %d want 404", r.StatusCode)
			}
			break
		}
		if time.Now().After(waitUp) {
			t.Fatal("server did not come up")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// JSON payload -> message delivered, cold bubble woken
	r, err := post(url, `{"subject":"deploy failed","body":"job 4821 exited 1","from":"ci"}`, "application/json")
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("post: %v (status %v)", err, r)
	}
	r.Body.Close()
	if !k.IsHot(a) {
		t.Fatal("webhook should wake the cold bubble")
	}
	in := k.Inbox(a)
	if len(in) != 1 || !strings.Contains(in[0], "webhook (ci)") || !strings.Contains(in[0], "deploy failed") || !strings.Contains(in[0], "job 4821") {
		t.Fatalf("inbox = %v", in)
	}

	// raw body + query subject (the shell/cron path)
	r, err = post(url+"?subject=ping&from=cron", "build is green", "text/plain")
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("raw post: %v %v", err, r)
	}
	r.Body.Close()
	in = k.Inbox(a)
	if len(in) != 1 || !strings.Contains(in[0], "webhook (cron)") || !strings.Contains(in[0], "build is green") {
		t.Fatalf("raw inbox = %v", in)
	}

	// GET refused
	resp, err := http.Get(url)
	if err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %v %v want 405", err, resp.StatusCode)
	}
	resp.Body.Close()

	// token survives fleet save/restore (stable URL across restarts)
	baseDir := t.TempDir()
	if err := saveFleet(baseDir, k, map[int]addr.Address{}); err != nil {
		t.Fatal(err)
	}
	k2 := kernel.New(runner.NewFake())
	restoreFleet(baseDir, k2)
	tok := strings.TrimPrefix(url, base+"/w/")
	if got, ok := k2.ResolveWebhookToken(tok); !ok || got != a {
		t.Fatalf("webhook token should survive restart, got %v %v", got, ok)
	}

	// rotation kills the old URL over HTTP too
	if _, err := k.RotateWebhook(a); err != nil {
		t.Fatal(err)
	}
	r, err = post(url, "late event", "text/plain")
	if err != nil || r.StatusCode != http.StatusNotFound {
		t.Fatalf("old URL after rotate = %v want 404", r.StatusCode)
	}
	r.Body.Close()
}
