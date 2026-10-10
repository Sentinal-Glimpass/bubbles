package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Plugin MCP servers. A Claude Code plugin ships its MCP servers inside the
// plugin (its .mcp.json, or "mcpServers" in .claude-plugin/plugin.json), not in
// ~/.claude.json — so under --strict-mcp-config a bubble never saw them (e.g.
// the Slack plugin's server). These helpers read the operator's ENABLED plugins
// and hand their servers to bubbles.
//
// Each server keeps the name claude itself gives it, "plugin:<plugin>:<server>":
// claude stores a server's OAuth token under that name, so a login done once in
// a normal session (/mcp → authenticate) is the one every bubble then uses.

// pluginServerName is claude's own name for a plugin-provided MCP server.
func pluginServerName(plugin, server string) string {
	return "plugin:" + plugin + ":" + server
}

// pluginMCPServers returns the MCP servers of every plugin enabled in
// ~/.claude/settings.json, keyed by pluginServerName. want filters them: nil
// takes all; otherwise a server is kept if want names its full name, its server
// name ("slack"), or its plugin name. Any unreadable file is skipped.
func pluginMCPServers(want map[string]bool) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	home, err := os.UserHomeDir()
	if err != nil {
		return out
	}
	claudeDir := filepath.Join(home, ".claude")

	var settings struct {
		EnabledPlugins map[string]bool `json:"enabledPlugins"`
	}
	if data, err := os.ReadFile(filepath.Join(claudeDir, "settings.json")); err != nil || json.Unmarshal(data, &settings) != nil {
		return out
	}
	var installed struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
		} `json:"plugins"`
	}
	if data, err := os.ReadFile(filepath.Join(claudeDir, "plugins", "installed_plugins.json")); err != nil || json.Unmarshal(data, &installed) != nil {
		return out
	}

	ids := make([]string, 0, len(settings.EnabledPlugins))
	for id, on := range settings.EnabledPlugins {
		if on {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids { // id is "<plugin>@<marketplace>"
		entries := installed.Plugins[id]
		if len(entries) == 0 || entries[len(entries)-1].InstallPath == "" {
			continue
		}
		root := entries[len(entries)-1].InstallPath
		plugin, _, _ := strings.Cut(id, "@")
		for server, def := range pluginServerDefs(root) {
			full := pluginServerName(plugin, server)
			if want != nil && !want[full] && !want[server] && !want[plugin] {
				continue
			}
			out[full] = expandPluginRoot(def, root)
		}
	}
	return out
}

// pluginServerDefs reads a plugin's MCP servers from its .mcp.json and from an
// inline "mcpServers" object in .claude-plugin/plugin.json. A plugin .mcp.json
// comes in two shapes: wrapped ({"mcpServers": {...}}, e.g. slack) and flat
// ({"linear": {...}}, e.g. linear, serena); both are read.
func pluginServerDefs(root string) map[string]json.RawMessage {
	defs := map[string]json.RawMessage{}
	if data, err := os.ReadFile(filepath.Join(root, ".mcp.json")); err == nil {
		var top map[string]json.RawMessage
		if json.Unmarshal(data, &top) == nil {
			servers := top
			if wrapped, ok := top["mcpServers"]; ok {
				servers = nil
				_ = json.Unmarshal(wrapped, &servers)
			}
			for n, d := range servers {
				if isServerDef(d) {
					defs[n] = d
				}
			}
		}
	}
	var manifest struct {
		MCPServers json.RawMessage `json:"mcpServers"`
	}
	if data, err := os.ReadFile(filepath.Join(root, ".claude-plugin", "plugin.json")); err == nil && json.Unmarshal(data, &manifest) == nil {
		var inline map[string]json.RawMessage
		if json.Unmarshal(manifest.MCPServers, &inline) == nil { // a path string instead is ignored
			for n, d := range inline {
				if _, dup := defs[n]; !dup {
					defs[n] = d
				}
			}
		}
	}
	return defs
}

// isServerDef reports whether d looks like one MCP server definition (a stdio
// command or a URL), so stray top-level keys in a flat .mcp.json are ignored.
func isServerDef(d json.RawMessage) bool {
	var v struct {
		Command string `json:"command"`
		URL     string `json:"url"`
	}
	return json.Unmarshal(d, &v) == nil && (v.Command != "" || v.URL != "")
}

// expandPluginRoot substitutes ${CLAUDE_PLUGIN_ROOT}, which plugins use to
// point at their own files and which claude only expands for plugins it loads
// itself — not for a server handed over in --mcp-config.
func expandPluginRoot(def json.RawMessage, root string) json.RawMessage {
	s := string(def)
	if !strings.Contains(s, "${CLAUDE_PLUGIN_ROOT}") {
		return def
	}
	esc, _ := json.Marshal(root) // JSON-escaped, with quotes
	return json.RawMessage(strings.ReplaceAll(s, "${CLAUDE_PLUGIN_ROOT}", string(esc[1:len(esc)-1])))
}

// inheritedMCPServers is the full set of the operator's servers a bubble gets:
// the curated/named servers from the operator's config, plus plugin servers —
// all enabled plugins' by default (or with the additive "+name" form), only the
// named ones for an explicit list, none for "none". A plugin server whose short
// name the operator already provides (e.g. a hand-configured "github") is
// skipped so a bubble doesn't get the same tools twice. Read at each launch, so
// a newly installed plugin reaches the next bubble launched — no restart.
func inheritedMCPServers() map[string]json.RawMessage {
	allow := mcpAllowList()
	out := resolveMCPServers(allow)
	if allow == nil {
		return out
	}
	var want map[string]bool
	if !mcpAllPlugins() {
		want = map[string]bool{}
		for _, n := range allow {
			want[n] = true
		}
	}
	for full, def := range pluginMCPServers(want) {
		short := full[strings.LastIndex(full, ":")+1:]
		if _, have := out[short]; have {
			continue
		}
		out[full] = def
	}
	return out
}
