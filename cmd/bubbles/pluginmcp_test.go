package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePluginHome fakes ~/.claude with three enabled plugins (slack: an HTTP
// server in a wrapped .mcp.json; linear: a flat .mcp.json; tool: a stdio server
// inline in plugin.json that uses ${CLAUDE_PLUGIN_ROOT}) and one
// installed-but-disabled plugin.
func writePluginHome(t *testing.T) (home, toolRoot string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	write := func(path string, v any) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(v)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cache := filepath.Join(home, ".claude", "plugins", "cache", "mkt")
	slackRoot := filepath.Join(cache, "slack", "1.4.1")
	toolRoot = filepath.Join(cache, "tool", "abc")
	offRoot := filepath.Join(cache, "off", "1")
	write(filepath.Join(home, ".claude", "settings.json"), map[string]any{
		"enabledPlugins": map[string]bool{"slack@mkt": true, "tool@mkt": true, "linear@mkt": true, "off@mkt": false},
	})
	write(filepath.Join(home, ".claude", "plugins", "installed_plugins.json"), map[string]any{
		"plugins": map[string]any{
			"slack@mkt":  []map[string]string{{"installPath": slackRoot}},
			"tool@mkt":   []map[string]string{{"installPath": toolRoot}},
			"off@mkt":    []map[string]string{{"installPath": offRoot}},
			"linear@mkt": []map[string]string{{"installPath": filepath.Join(cache, "linear", "1")}},
		},
	})
	write(filepath.Join(slackRoot, ".mcp.json"), map[string]any{"mcpServers": map[string]any{
		"slack": map[string]any{"type": "http", "url": "https://mcp.slack.com/mcp"},
	}})
	write(filepath.Join(toolRoot, ".claude-plugin", "plugin.json"), map[string]any{"mcpServers": map[string]any{
		"tool": map[string]any{"command": "${CLAUDE_PLUGIN_ROOT}/bin/server", "args": []string{"--x"}},
	}})
	// linear ships a FLAT .mcp.json (no "mcpServers" wrapper).
	linearRoot := filepath.Join(cache, "linear", "1")
	write(filepath.Join(linearRoot, ".mcp.json"), map[string]any{
		"linear": map[string]any{"type": "http", "url": "https://mcp.linear.app/mcp"},
	})
	write(filepath.Join(offRoot, ".mcp.json"), map[string]any{"mcpServers": map[string]any{
		"off": map[string]any{"command": "off-server"},
	}})
	return home, toolRoot
}

// TestPluginMCPServers is the Slack-plugin bug: plugin servers live inside the
// plugin, so a bubble (under --strict-mcp-config) never got them.
func TestPluginMCPServers(t *testing.T) {
	_, toolRoot := writePluginHome(t)
	got := pluginMCPServers(nil)
	if len(got) != 3 {
		t.Fatalf("got %v, want the three ENABLED plugins' servers", keysOf(got))
	}
	if !strings.Contains(string(got["plugin:linear:linear"]), "mcp.linear.app") {
		t.Fatal("a flat .mcp.json (no mcpServers wrapper) must be read too")
	}
	if !strings.Contains(string(got["plugin:slack:slack"]), "mcp.slack.com") {
		t.Fatalf("slack must keep claude's own name (its OAuth token is stored under it): %v", keysOf(got))
	}
	tool := string(got["plugin:tool:tool"])
	if strings.Contains(tool, "CLAUDE_PLUGIN_ROOT") || !strings.Contains(tool, toolRoot+"/bin/server") {
		t.Fatalf("${CLAUDE_PLUGIN_ROOT} must be expanded to the install path, got %s", tool)
	}
	if only := pluginMCPServers(map[string]bool{"slack": true}); len(only) != 1 || only["plugin:slack:slack"] == nil {
		t.Fatalf("a filter by short name must select just that server, got %v", keysOf(only))
	}
}

func TestInheritedMCPServersModes(t *testing.T) {
	home, _ := writePluginHome(t)
	// The operator hand-configured their own "slack" plus playwright.
	cfg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{
		"playwright": map[string]any{"command": "npx"},
		"slack":      map[string]any{"command": "my-slack"},
	}})
	os.WriteFile(filepath.Join(home, ".claude.json"), cfg, 0o644)

	t.Setenv("BUBBLES_MCP", "") // default: curated + every enabled plugin
	got := inheritedMCPServers()
	if got["playwright"] == nil || got["plugin:tool:tool"] == nil {
		t.Fatalf("default must include curated and plugin servers, got %v", keysOf(got))
	}

	t.Setenv("BUBBLES_MCP", "+slack") // additive: default + the operator's slack
	got = inheritedMCPServers()
	if got["playwright"] == nil || got["slack"] == nil || got["plugin:tool:tool"] == nil {
		t.Fatalf("'+slack' must keep the defaults and add slack, got %v", keysOf(got))
	}
	if got["plugin:slack:slack"] != nil {
		t.Fatal("the plugin's slack duplicates the operator's own slack and must be skipped")
	}

	t.Setenv("BUBBLES_MCP", "tool") // explicit: only what's named
	got = inheritedMCPServers()
	if len(got) != 1 || got["plugin:tool:tool"] == nil {
		t.Fatalf("an explicit list must give only the named servers, got %v", keysOf(got))
	}

	t.Setenv("BUBBLES_MCP", "none")
	if got = inheritedMCPServers(); len(got) != 0 {
		t.Fatalf("'none' must give nothing, got %v", keysOf(got))
	}
}
