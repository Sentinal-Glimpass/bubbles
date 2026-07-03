package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPAllowList(t *testing.T) {
	t.Setenv("BUBBLES_MCP", "")
	if got := mcpAllowList(); len(got) == 0 {
		t.Fatal("default allow list should be non-empty")
	}
	t.Setenv("BUBBLES_MCP", "none")
	if got := mcpAllowList(); got != nil {
		t.Fatalf("'none' should disable inheritance, got %v", got)
	}
	t.Setenv("BUBBLES_MCP", "playwright, github ,")
	got := mcpAllowList()
	if len(got) != 2 || got[0] != "playwright" || got[1] != "github" {
		t.Fatalf("parsed allow list = %v", got)
	}
}

func TestResolveMCPServersAndConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// a claude config: playwright global, firecrawl in a project, plus an unwanted one
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"playwright": map[string]any{"type": "stdio", "command": "npx", "args": []string{"playwright-mcp"}},
			"docusign":   map[string]any{"type": "http", "url": "https://x"},
		},
		"projects": map[string]any{
			"/home/rishi": map[string]any{"mcpServers": map[string]any{
				"firecrawl": map[string]any{"type": "stdio", "command": "npx", "args": []string{"firecrawl"}},
			}},
		},
	}
	data, _ := json.Marshal(cfg)
	os.WriteFile(filepath.Join(home, ".claude.json"), data, 0o644)

	got := resolveMCPServers([]string{"playwright", "firecrawl", "missing"})
	if len(got) != 2 {
		t.Fatalf("resolved = %v want playwright+firecrawl", keysOf(got))
	}
	if _, ok := got["playwright"]; !ok {
		t.Fatal("playwright (global) should resolve")
	}
	if _, ok := got["firecrawl"]; !ok {
		t.Fatal("firecrawl (project) should resolve")
	}
	if _, ok := got["docusign"]; ok {
		t.Fatal("unrequested server must not be included")
	}

	// the generated config includes bubbles + the curated servers, and playwright's command survives
	js := mcpConfigJSON("/bin/bubbles", "/tmp/sock", "0.1", true, got)
	for _, want := range []string{`"bubbles"`, `"playwright"`, `"firecrawl"`, `playwright-mcp`, `"BUBBLE_ADDR":"0.1"`} {
		if !strings.Contains(js, want) {
			t.Fatalf("mcp config missing %q:\n%s", want, js)
		}
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	var k []string
	for n := range m {
		k = append(k, n)
	}
	return k
}
