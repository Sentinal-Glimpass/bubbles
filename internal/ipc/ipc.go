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
	Op      string `json:"op"` // "send" | "contacts" | "spawn" | "inbox"
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body,omitempty"`
	Persona string `json:"persona,omitempty"`
	Dir     string `json:"dir,omitempty"`
}

// Reply is the result of handling a Request.
type Reply struct {
	OK       bool     `json:"ok"`
	Err      string   `json:"err,omitempty"`
	Contacts []string `json:"contacts,omitempty"`
	Messages []string `json:"messages,omitempty"`
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
	conn net.Conn
	dec  *json.Decoder
	enc  *json.Encoder
}

// Dial connects to a served socket.
func Dial(sock string) (*Client, error) {
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn, dec: json.NewDecoder(conn), enc: json.NewEncoder(conn)}, nil
}

// Do sends a Request and returns the Reply. Safe for concurrent use.
func (c *Client) Do(req Request) (Reply, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
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
