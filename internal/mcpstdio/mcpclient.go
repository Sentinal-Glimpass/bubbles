package mcpstdio

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// mcpClient is a minimal JSON-RPC 2.0 client over a child MCP server's stdio.
//
// It is what lets the bubbles server PROXY another MCP server: spawn it,
// discover its tools, and forward tools/call to it. That proxying — rather than
// injecting the child into claude's own --mcp-config — is what makes a new MCP
// usable LIVE, with no relaunch: the bubbles server re-advertises the child's
// tools and fires notifications/tools/list_changed, which claude honours by
// re-fetching tools/list mid-session.
type mcpClient struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader

	wmu sync.Mutex // serialize writes to the child's stdin

	mu      sync.Mutex // guards nextID, pending, closed
	nextID  int
	pending map[int]chan clientResponse
	closed  bool
}

// clientResponse decodes a child's reply with the result left raw, so a proxied
// tools/call result (content blocks, isError, …) passes back to claude verbatim.
type clientResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// startMCPClient spawns command with args and env and begins reading its
// stdout. env is the full environment for the child (caller merges as needed).
func startMCPClient(command string, args, env []string) (*mcpClient, error) {
	cmd := exec.Command(command, args...)
	cmd.Env = env
	cmd.Stderr = io.Discard // the child's logs must never land on our JSON stdout
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &mcpClient{cmd: cmd, in: stdin, out: bufio.NewReader(stdout), pending: map[int]chan clientResponse{}}
	go c.readLoop()
	return c, nil
}

// readLoop dispatches each child reply to the waiting caller by id. Child
// notifications (no id) are ignored — we only proxy request/response.
func (c *mcpClient) readLoop() {
	dec := json.NewDecoder(c.out)
	for {
		var resp clientResponse
		if err := dec.Decode(&resp); err != nil {
			c.failAll()
			return
		}
		if len(resp.ID) == 0 {
			continue
		}
		var id int
		if json.Unmarshal(resp.ID, &id) != nil {
			continue
		}
		c.mu.Lock()
		ch := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if ch != nil {
			ch <- resp
		}
	}
}

// failAll unblocks every pending call when the child dies, so a proxied call
// returns an error instead of hanging until timeout.
func (c *mcpClient) failAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
}

// rpc sends one request and waits for its reply (or timeout). A closed channel
// (child died) reports that rather than blocking.
func (c *mcpClient) rpc(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("MCP server is not running")
	}
	c.nextID++
	id := c.nextID
	ch := make(chan clientResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	idRaw, _ := json.Marshal(id)
	pRaw, _ := json.Marshal(params)
	req := rpcMessage{JSONRPC: "2.0", ID: idRaw, Method: method, Params: pRaw}
	line, _ := json.Marshal(req)
	c.wmu.Lock()
	_, werr := c.in.Write(append(line, '\n'))
	c.wmu.Unlock()
	if werr != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, werr
	}

	select {
	case resp, alive := <-ch:
		if !alive {
			return nil, fmt.Errorf("MCP server exited before replying")
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("%s", resp.Error.Message)
		}
		return resp.Result, nil
	case <-time.After(timeout):
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("MCP server timed out after %s", timeout)
	}
}

// notify sends a fire-and-forget JSON-RPC notification (no id, no reply).
func (c *mcpClient) notify(method string) {
	req := map[string]any{"jsonrpc": "2.0", "method": method}
	line, _ := json.Marshal(req)
	c.wmu.Lock()
	_, _ = c.in.Write(append(line, '\n'))
	c.wmu.Unlock()
}

// initialize runs the MCP handshake: initialize request, then the
// notifications/initialized the spec requires before other calls.
func (c *mcpClient) initialize() error {
	_, err := c.rpc("initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "bubbles-proxy", "version": "0.1.0"},
	}, 20*time.Second)
	if err != nil {
		return err
	}
	c.notify("notifications/initialized")
	return nil
}

// listTools fetches the child's advertised tools.
func (c *mcpClient) listTools() ([]Tool, error) {
	raw, err := c.rpc("tools/list", map[string]any{}, 20*time.Second)
	if err != nil {
		return nil, err
	}
	var r struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	return r.Tools, nil
}

// callTool forwards a tool call and returns the child's raw result object.
func (c *mcpClient) callTool(name string, args map[string]any) (json.RawMessage, error) {
	if args == nil {
		args = map[string]any{}
	}
	return c.rpc("tools/call", map[string]any{"name": name, "arguments": args}, 120*time.Second)
}

// Close kills the child process and unblocks any pending calls.
func (c *mcpClient) Close() {
	c.mu.Lock()
	already := c.closed
	c.closed = true
	c.mu.Unlock()
	_ = c.in.Close()
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	if !already {
		c.mu.Lock()
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.mu.Unlock()
	}
}
