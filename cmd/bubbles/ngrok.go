package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// startNgrok launches an ngrok tunnel to the local webhook port and returns its
// public https URL. The daemon still binds loopback; ngrok forwards
// public -> 127.0.0.1:<port>, so nothing binds a public port directly. Requires
// the ngrok CLI on PATH with an authtoken configured (`ngrok config
// add-authtoken <t>` or NGROK_AUTHTOKEN). `domain` (optional) pins a reserved
// ngrok domain so the URL is STABLE across restarts (ngrok free gives one static
// domain); without it the URL is ephemeral and changes each start.
func startNgrok(port int, domain string) (publicURL string, stop func(), err error) {
	bin, err := exec.LookPath("ngrok")
	if err != nil {
		return "", nil, fmt.Errorf("ngrok not on PATH — install it and run `ngrok config add-authtoken <token>`")
	}
	args := []string{"http", strconv.Itoa(port), "--log=stdout"} // --log=stdout disables the interactive TUI (we're headless)
	if domain = strings.TrimSpace(domain); domain != "" {
		if !strings.HasPrefix(domain, "http") {
			domain = "https://" + domain
		}
		args = append(args, "--url="+domain)
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr                    // ngrok's logs/errors land in the daemon log
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL} // ngrok dies with the app — no strays
	if err := cmd.Start(); err != nil {
		return "", nil, err
	}
	stop = func() { _ = cmd.Process.Kill() }

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if u := ngrokPublicURL(); u != "" {
			return u, stop, nil
		}
		time.Sleep(400 * time.Millisecond)
	}
	stop()
	return "", nil, fmt.Errorf("ngrok gave no public URL in 15s — is the authtoken set, and is another ngrok already running? (free tier allows one active tunnel; see the daemon log)")
}

// ngrokPublicURL queries the ngrok agent's local API for its tunnel URL.
func ngrokPublicURL() string {
	resp, err := http.Get("http://127.0.0.1:4040/api/tunnels")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body := make([]byte, 64<<10)
	n, _ := resp.Body.Read(body)
	return parseNgrokTunnels(body[:n])
}

// parseNgrokTunnels extracts the public https URL from the agent API response.
func parseNgrokTunnels(data []byte) string {
	var out struct {
		Tunnels []struct {
			PublicURL string `json:"public_url"`
		} `json:"tunnels"`
	}
	if json.Unmarshal(data, &out) != nil {
		return ""
	}
	for _, t := range out.Tunnels { // prefer https
		if strings.HasPrefix(t.PublicURL, "https://") {
			return t.PublicURL
		}
	}
	for _, t := range out.Tunnels {
		if t.PublicURL != "" {
			return t.PublicURL
		}
	}
	return ""
}
