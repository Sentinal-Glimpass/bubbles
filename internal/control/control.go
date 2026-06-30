// Package control is the RPC between the bubbles daemon (which owns the kernel
// and every claude PTY) and a TUI client. The client renders from a Snapshot and
// issues actions; dive-in uses a separate byte-relay (added later).
package control

import (
	"encoding/json"
	"net"
	"sync"
)

// BubbleInfo is one bubble's state in a Snapshot.
type BubbleInfo struct {
	Addr    string `json:"addr"`
	Persona string `json:"persona"`
	Parent  string `json:"parent"`
	Status  string `json:"status"`
	Unread  int    `json:"unread"`
}

// GroupInfo is one group's state in a Snapshot.
type GroupInfo struct {
	Name    string   `json:"name"`
	Members []string `json:"members"`
	Session string   `json:"session,omitempty"`
}

// Snapshot is everything the TUI needs to render the fleet.
type Snapshot struct {
	Bubbles []BubbleInfo      `json:"bubbles"`
	Groups  []GroupInfo       `json:"groups"`
	Marks   map[string]string `json:"marks"` // slot -> address
}

// Request is one control call to the daemon.
type Request struct {
	Op           string   `json:"op"` // snapshot|spawn|startRoot|introduce|createGroup|attachGroupSession|deleteGroup|setMark|clearMark|stop
	By           string   `json:"by,omitempty"`
	Parent       string   `json:"parent,omitempty"`
	Persona      string   `json:"persona,omitempty"`
	Dir          string   `json:"dir,omitempty"`
	Name         string   `json:"name,omitempty"`
	Members      []string `json:"members,omitempty"`
	IntroduceAll bool     `json:"introduceAll,omitempty"`
	A            string   `json:"a,omitempty"`
	B            string   `json:"b,omitempty"`
	Slot         int      `json:"slot,omitempty"`
}

// Reply is the daemon's response.
type Reply struct {
	OK       bool      `json:"ok"`
	Err      string    `json:"err,omitempty"`
	Addr     string    `json:"addr,omitempty"`
	Snapshot *Snapshot `json:"snapshot,omitempty"`
}

// Serve listens on sock and calls handle for each Request. Closing the returned
// Listener stops it.
func Serve(sock string, handle func(Request) Reply) (*Listener, error) {
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	l := &Listener{ln: ln}
	go l.accept(handle)
	return l, nil
}

// Listener wraps the control accept loop.
type Listener struct{ ln net.Listener }

func (l *Listener) accept(handle func(Request) Reply) {
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			return
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
			return
		}
		if err := enc.Encode(handle(req)); err != nil {
			return
		}
	}
}

func (l *Listener) Close() error { return l.ln.Close() }

// Client dials a control socket and issues Requests.
type Client struct {
	mu   sync.Mutex
	conn net.Conn
	dec  *json.Decoder
	enc  *json.Encoder
}

func Dial(sock string) (*Client, error) {
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn, dec: json.NewDecoder(conn), enc: json.NewEncoder(conn)}, nil
}

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

func (c *Client) Close() error { return c.conn.Close() }
