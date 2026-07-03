package main

import "testing"

func TestParseNgrokTunnels(t *testing.T) {
	body := []byte(`{"tunnels":[
		{"public_url":"http://ab12.ngrok-free.app","proto":"http"},
		{"public_url":"https://ab12.ngrok-free.app","proto":"https"}
	]}`)
	if got := parseNgrokTunnels(body); got != "https://ab12.ngrok-free.app" {
		t.Fatalf("should prefer https, got %q", got)
	}
	if got := parseNgrokTunnels([]byte(`{"tunnels":[]}`)); got != "" {
		t.Fatalf("empty -> %q", got)
	}
	if got := parseNgrokTunnels([]byte(`not json`)); got != "" {
		t.Fatalf("bad json -> %q", got)
	}
	// http-only still returned as a fallback
	if got := parseNgrokTunnels([]byte(`{"tunnels":[{"public_url":"http://x.io"}]}`)); got != "http://x.io" {
		t.Fatalf("http fallback -> %q", got)
	}
}
