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

func TestRateLimiter(t *testing.T) {
	rl := newRateLimiter(1, 3) // burst 3, 1/sec
	now := time.Unix(1000, 0)
	// burst: first 3 allowed
	for i := 0; i < 3; i++ {
		if !rl.allow("tok", now) {
			t.Fatalf("burst request %d should be allowed", i)
		}
	}
	// 4th within the same instant: denied
	if rl.allow("tok", now) {
		t.Fatal("over-burst request should be rate limited")
	}
	// after ~1.1s, one token refilled -> allowed again
	if !rl.allow("tok", now.Add(1100*time.Millisecond)) {
		t.Fatal("should allow after refill")
	}
	// a different token has its own bucket
	if !rl.allow("other", now) {
		t.Fatal("a separate token should not share the bucket")
	}
}

// TestControlWebhookHTTP: POST /c/<token> spawns and deletes bubbles as the
// owning bubble, gated by its spawn authority; a non-spawn bubble gets no URL.
func TestControlWebhookHTTP(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()
	t.Setenv("BUBBLES_WEBHOOK_PORT", strconv.Itoa(port))
	t.Setenv("BUBBLES_WEBHOOK_PUBLIC", "")
	t.Setenv("BUBBLES_WEBHOOK_BASE", "")
	t.Chdir(t.TempDir()) // default spawn dir is cwd; keep it out of the repo

	k := kernel.New(runner.NewFake())
	k.RelaunchProbe = 0
	boss, _ := k.Spawn(addr.Root, "", t.TempDir(), runner.SpawnOpts{Name: "boss", GrantSpawn: true})
	plain, _ := k.Spawn(addr.Root, "", t.TempDir(), runner.SpawnOpts{Name: "plain"})

	base := startWebhookServer(k)
	if base == "" {
		t.Fatal("webhook server failed to start")
	}
	k.WebhookBase = base

	// A non-spawn bubble is refused a control surface entirely.
	if _, err := k.ControlWebhookURL(plain); err == nil {
		t.Fatal("plain bubble should not get a control webhook")
	}
	ctl, err := k.ControlWebhookURL(boss)
	if err != nil {
		t.Fatal(err)
	}

	post := func(body string) (int, string) {
		r, err := http.Post(ctl, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		return r.StatusCode, string(buf[:n])
	}
	// wait for listener
	waitUp := time.Now().Add(2 * time.Second)
	for {
		if r, err := http.Post(base+"/c/bogus", "application/json", strings.NewReader("{}")); err == nil {
			r.Body.Close()
			break
		}
		if time.Now().After(waitUp) {
			t.Fatal("server did not come up")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// spawn → a child appears under boss and its address is returned
	code, body := post(`{"action":"spawn","name":"minion","description":"do the thing","model":"opus"}`)
	if code != 200 || !strings.Contains(body, `"ok":true`) || !strings.Contains(body, `"addr":"0.1.1"`) {
		t.Fatalf("spawn resp %d: %s", code, body)
	}
	if _, ok := k.Reg.Get("0.1.1"); !ok {
		t.Fatal("spawned child not in registry")
	}

	// list → shows the child
	code, body = post(`{"action":"list"}`)
	if code != 200 || !strings.Contains(body, "0.1.1") || !strings.Contains(body, "minion") {
		t.Fatalf("list resp %d: %s", code, body)
	}

	// delete → removes it
	code, body = post(`{"action":"delete","target":"0.1.1"}`)
	if code != 200 || !strings.Contains(body, `"removed":1`) {
		t.Fatalf("delete resp %d: %s", code, body)
	}
	if _, ok := k.Reg.Get("0.1.1"); ok {
		t.Fatal("child not removed")
	}

	// boss cannot delete something outside its subtree (plain is a sibling)
	code, body = post(`{"action":"delete","target":"` + plain.String() + `"}`)
	if code != http.StatusForbidden {
		t.Fatalf("cross-subtree delete should be forbidden, got %d: %s", code, body)
	}
	if _, ok := k.Reg.Get(plain); !ok {
		t.Fatal("sibling wrongly deleted")
	}

	// unknown action → 400
	if code, _ := post(`{"action":"frobnicate"}`); code != http.StatusBadRequest {
		t.Fatalf("unknown action got %d", code)
	}
}

func TestWebhookPortFixedAndOverridable(t *testing.T) {
	t.Setenv("BUBBLES_WEBHOOK_PORT", "") // no override -> the fixed default
	if p := webhookPort(); p != defaultWebhookPort {
		t.Fatalf("default webhook port = %d, want %d", p, defaultWebhookPort)
	}
	t.Setenv("BUBBLES_WEBHOOK_PORT", "9999") // explicit override wins
	if webhookPort() != 9999 {
		t.Fatal("explicit BUBBLES_WEBHOOK_PORT should override the default")
	}
}
