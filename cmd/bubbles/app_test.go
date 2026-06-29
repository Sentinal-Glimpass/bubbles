package main

import (
	"encoding/json"
	"testing"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/bus"
	"github.com/Sentinal-Glimpass/bubbles/internal/ipc"
	"github.com/Sentinal-Glimpass/bubbles/internal/kernel"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
)

func TestHandleIPC(t *testing.T) {
	k := kernel.New(runner.NewFake()) // FakeRunner: no real claude
	// root inbox must exist for a send to root to route
	k.Bus.Subscribe(addr.Root, func(bus.Message) {})

	// spawn by root creates 0.1
	rep := handleIPC(k, ipc.Request{Op: "spawn", From: "0", Persona: "scout", Dir: t.TempDir()})
	if !rep.OK || rep.Addr != "0.1" {
		t.Fatalf("spawn reply = %+v want addr 0.1", rep)
	}

	// 0.1 -> root send is allowed (fresh bubble knows root)
	if rep := handleIPC(k, ipc.Request{Op: "send", From: "0.1", To: "0", Subject: "hi"}); !rep.OK {
		t.Fatalf("send to root = %+v want ok", rep)
	}

	// 0.1 -> some stranger is denied (not a contact)
	if rep := handleIPC(k, ipc.Request{Op: "send", From: "0.1", To: "0.9", Subject: "x"}); rep.OK {
		t.Fatalf("send to non-contact unexpectedly ok")
	}

	// contacts of 0.1 = [root], labeled with its persona
	rep = handleIPC(k, ipc.Request{Op: "contacts", From: "0.1"})
	if !rep.OK || len(rep.Contacts) != 1 || rep.Contacts[0] != "0 (root)" {
		t.Fatalf("contacts = %+v want [\"0 (root)\"]", rep)
	}

	// unknown op
	if rep := handleIPC(k, ipc.Request{Op: "frobnicate"}); rep.OK {
		t.Fatalf("unknown op unexpectedly ok")
	}
}

func TestMCPConfigJSON(t *testing.T) {
	js := mcpConfigJSON("/usr/local/bin/bubbles", "/tmp/b.sock", addr.Address("0.2"), true)
	var cfg struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(js), &cfg); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, js)
	}
	s, ok := cfg.MCPServers["bubbles"]
	if !ok {
		t.Fatal("no 'bubbles' server in config")
	}
	if s.Type != "stdio" || s.Command != "/usr/local/bin/bubbles" || len(s.Args) != 1 || s.Args[0] != "mcp-stdio" {
		t.Fatalf("server wiring wrong: %+v", s)
	}
	if s.Env["BUBBLE_ADDR"] != "0.2" || s.Env["BUBBLE_SOCK"] != "/tmp/b.sock" || s.Env["BUBBLE_SPAWNABLE"] != "1" {
		t.Fatalf("env wrong: %+v", s.Env)
	}
}
