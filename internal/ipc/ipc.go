// Package ipc is a tiny newline-delimited JSON request/reply protocol over a
// unix socket. The main bubbles process serves it; each per-bubble MCP helper
// dials it to relay tool calls back to the kernel.
package ipc

import (
	"encoding/json"
	"net"
	"sync"
)

// Request is a tool action relayed from a bubble's MCP helper.
type Request struct {
	Op      string `json:"op"` // "send" | "contacts" | "spawn" | "inbox" | "status" | "edit" | "delete"
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Addr    string `json:"addr,omitempty"` // target bubble for edit/delete
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body,omitempty"`
	ReplyTo int    `json:"replyTo,omitempty"`
	Urgent  bool   `json:"urgent,omitempty"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Persona     string `json:"persona,omitempty"`
	Dir     string `json:"dir,omitempty"`
	Model   string `json:"model,omitempty"`
	Every   string `json:"every,omitempty"` // schedule interval ("15m")
	Daily   string `json:"daily,omitempty"` // schedule daily time ("08:00")

	Task      string `json:"task,omitempty"`      // task id (submit_task / verdict)
	Cmd       string `json:"cmd,omitempty"`       // assign_task check command
	Checklist string `json:"checklist,omitempty"` // assign_task checklist (newline-separated)
	Pass      bool   `json:"pass,omitempty"`      // verdict ruling
	Rotate    bool   `json:"rotate,omitempty"`    // control_webhook: mint a fresh URL
}

// Reply is the result of handling a Request.
type Reply struct {
	OK       bool     `json:"ok"`
	Err      string   `json:"err,omitempty"`
	Contacts []string `json:"contacts,omitempty"`
	Messages []string `json:"messages,omitempty"`
	ID       int      `json:"id,omitempty"`
	Addr     string   `json:"addr,omitempty"`
}

// Serve listens on sock and calls handle for each decoded Request. The returned
// closer stops the listener.
func Serve(sock string, handle func(Request) Reply) (*Listener, error) {
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	l := &Listener{ln: ln}
	go l.accept(handle)
	return l, nil
}

// Listener wraps the accept loop so callers can Close it.
type Listener struct {
	ln net.Listener
}

func (l *Listener) accept(handle func(Request) Reply) {
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			return // listener closed
		}
		go serveConn(conn, handle)
	}
}

func serveConn(conn net.Conn, handle func(Request) Reply) {
	defer conn.Close()
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	for {
		var req Request
		if err := dec.Decode(&req); err != nil {
			return // EOF or bad frame
		}
		if err := enc.Encode(handle(req)); err != nil {
			return
		}
	}
}

// Close stops accepting connections.
func (l *Listener) Close() error { return l.ln.Close() }

// Client dials a served socket and issues Requests.
type Client struct {
	mu   sync.Mutex
	sock string // kept so Do can reconnect after a daemon restart
	conn net.Conn
	dec  *json.Decoder
	enc  *json.Encoder
}

// Dial connects to a served socket.
func Dial(sock string) (*Client, error) {
	c := &Client{sock: sock}
	if err := c.redial(); err != nil {
		return nil, err
	}
	return c, nil
}

// redial (re)establishes the connection to the socket. Callers hold c.mu (Do)
// or are single-threaded (Dial).
func (c *Client) redial() error {
	conn, err := net.Dial("unix", c.sock)
	if err != nil {
		return err
	}
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.conn = conn
	c.dec = json.NewDecoder(conn)
	c.enc = json.NewEncoder(conn)
	return nil
}

// Do sends a Request and returns the Reply. Safe for concurrent use.
func (c *Client) Do(req Request) (Reply, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rep, err := c.exchange(req)
	if err == nil {
		return rep, nil
	}
	// The daemon may have restarted (new process, same STABLE socket path). A
	// persistent connection to the old daemon is now dead — reconnect once and
	// retry so a live session heals instead of failing every tool call until it
	// is relaunched. This is the fix for post-restart "loud send-failures".
	if rerr := c.redial(); rerr != nil {
		return Reply{}, err // daemon still unreachable: surface the original error
	}
	return c.exchange(req)
}

func (c *Client) exchange(req Request) (Reply, error) {
	if err := c.enc.Encode(req); err != nil {
		return Reply{}, err
	}
	var rep Reply
	if err := c.dec.Decode(&rep); err != nil {
		return Reply{}, err
	}
	return rep, nil
}

// Close closes the client connection.
func (c *Client) Close() error { return c.conn.Close() }
