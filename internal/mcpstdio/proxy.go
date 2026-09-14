package mcpstdio

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// This file turns the bubbles stdio server into a small MCP AGGREGATOR: a
// bubble can add another MCP server at runtime via the add_mcp tool, and its
// tools become usable immediately — no relaunch — because we re-advertise them
// and fire notifications/tools/list_changed (which claude honours live).
//
// The child servers are proxied, never handed to claude directly: claude only
// ever talks to this one server (so --strict-mcp-config stays intact), and this
// server forwards the namespaced calls on.

// serverDef is a persisted child-MCP launch spec (stdio command servers only —
// the headless model bubbles run in cannot drive an interactive/URL auth flow).
type serverDef struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// proxied is one connected child MCP: its live client and the tools it exposed
// at connect time (kept so tools/list needn't round-trip the child every call).
type proxied struct {
	client *mcpClient
	tools  []Tool
	def    serverDef
}

// proxyMu guards the proxied map. Separate from the write mutex: connecting a
// child (which spawns a process and does I/O) must not hold the stdout lock.
var _ sync.Mutex

// proxyToolPrefixSep separates the server name from the tool name in an
// aggregated tool ("github.search"). A dot is illegal in an add_mcp name so the
// split is unambiguous.
const proxyToolPrefixSep = "."

// addMCP connects a child MCP by name, persists it, and makes its tools live.
func (s *Server) addMCP(id json.RawMessage, args map[string]any) rpcResponse {
	name := strings.TrimSpace(str(args["name"]))
	command := strings.TrimSpace(str(args["command"]))
	if name == "" || command == "" {
		return toolErr(id, "add_mcp needs a 'name' and a 'command' (the executable that speaks MCP over stdio)")
	}
	if strings.ContainsAny(name, "./\\ \t") {
		return toolErr(id, "'name' must be a simple identifier — no dots, slashes, or spaces (it prefixes the tools, e.g. "+name+".<tool>)")
	}
	def := serverDef{Command: command, Args: toStringSlice(args["args"]), Env: toStringMap(args["env"])}
	if err := s.connectProxy(name, def); err != nil {
		return toolErr(id, "could not start MCP '"+name+"': "+err.Error())
	}
	s.persistOverlay()
	s.notifyToolsChanged()

	s.pmu.Lock()
	n := len(s.proxied[name].tools)
	names := make([]string, 0, n)
	for _, t := range s.proxied[name].tools {
		names = append(names, name+proxyToolPrefixSep+t.Name)
	}
	s.pmu.Unlock()
	return toolOK(id, fmt.Sprintf("MCP %q is live now — %d tool(s), no restart needed: %s. It persists across relaunches.",
		name, n, strings.Join(names, ", ")))
}

// connectProxy spawns and handshakes a child MCP, replacing any prior server of
// the same name. On any failure nothing is registered (and a half-started child
// is killed), so the map only ever holds working servers.
func (s *Server) connectProxy(name string, def serverDef) error {
	env := append(os.Environ(), "BUBBLE_ADDR="+s.Self)
	for k, v := range def.Env {
		env = append(env, k+"="+v)
	}
	client, err := startMCPClient(def.Command, def.Args, env)
	if err != nil {
		return err
	}
	if err := client.initialize(); err != nil {
		client.Close()
		return err
	}
	tools, err := client.listTools()
	if err != nil {
		client.Close()
		return err
	}
	s.pmu.Lock()
	if s.proxied == nil {
		s.proxied = map[string]*proxied{}
	}
	if old := s.proxied[name]; old != nil && old.client != nil {
		old.client.Close()
	}
	s.proxied[name] = &proxied{client: client, tools: tools, def: def}
	s.pmu.Unlock()
	return nil
}

// proxyTools returns every proxied tool, namespaced "<server>.<tool>" and
// tagged in its description so the caller knows where it came from.
func (s *Server) proxyTools() []Tool {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	var out []Tool
	for name, p := range s.proxied {
		for _, t := range p.tools {
			out = append(out, Tool{
				Name:        name + proxyToolPrefixSep + t.Name,
				Description: "[" + name + " MCP] " + t.Description,
				InputSchema: t.InputSchema,
			})
		}
	}
	return out
}

// tryProxyCall forwards a namespaced call to its child and returns the child's
// result verbatim. ok=false means the name isn't a proxied tool (fall through to
// the built-in switch).
func (s *Server) tryProxyCall(id json.RawMessage, name string, args map[string]any) (rpcResponse, bool) {
	dot := strings.Index(name, proxyToolPrefixSep)
	if dot < 0 {
		return rpcResponse{}, false
	}
	server, tool := name[:dot], name[dot+1:]
	s.pmu.Lock()
	p := s.proxied[server]
	s.pmu.Unlock()
	if p == nil {
		return rpcResponse{}, false
	}
	raw, err := p.client.callTool(tool, args)
	if err != nil {
		return toolErr(id, "MCP '"+server+"' call failed: "+err.Error()), true
	}
	return ok(id, json.RawMessage(raw)), true // pass the child's content blocks through untouched
}

// persistOverlay writes every connected server's def so a relaunch restores
// them (loadOverlay). Atomic temp+rename; best-effort (a backup that can't be
// written must not fail a working add).
func (s *Server) persistOverlay() {
	if s.OverlayPath == "" {
		return
	}
	s.pmu.Lock()
	defs := map[string]serverDef{}
	for name, p := range s.proxied {
		defs[name] = p.def
	}
	s.pmu.Unlock()
	data, _ := json.MarshalIndent(map[string]any{"servers": defs}, "", "  ")
	if os.MkdirAll(filepath.Dir(s.OverlayPath), 0o755) != nil {
		return
	}
	tmp := s.OverlayPath + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, s.OverlayPath)
	}
}

// loadOverlay reconnects the servers a previous session added, so a relaunched
// bubble comes back with the same personal MCPs. A server that no longer starts
// is skipped, never fatal.
func (s *Server) loadOverlay() {
	if s.OverlayPath == "" {
		return
	}
	data, err := os.ReadFile(s.OverlayPath)
	if err != nil {
		return
	}
	var f struct {
		Servers map[string]serverDef `json:"servers"`
	}
	if json.Unmarshal(data, &f) != nil {
		return
	}
	for name, def := range f.Servers {
		_ = s.connectProxy(name, def)
	}
}

// shutdownProxies kills every child when this server exits, so a bubble's death
// doesn't orphan its MCP subprocesses.
func (s *Server) shutdownProxies() {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	for _, p := range s.proxied {
		if p.client != nil {
			p.client.Close()
		}
	}
	s.proxied = map[string]*proxied{}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func toStringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func toStringMap(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]string{}
	for k, e := range m {
		if s, ok := e.(string); ok {
			out[k] = s
		}
	}
	return out
}
