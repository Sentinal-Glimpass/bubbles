package mcpstdio

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

type fakeBackend struct {
	sends   [][4]string
	urgent  []bool
	edits   [][5]string // by, addr, name, description, model
	deletes [][2]string // by, addr
	forgets [][2]string // by, addr

	assigns   [][4]string // by, to, brief, checkCmd, checklist
	submits   [][3]string // by, taskID, summary
	verdicts  []verdictCall
	decisions [][2]string // by, text
	cancels   [][2]string // by, taskID
	controls  []string    // callers who asked for a control webhook
	intros      [][3]string // by, a, b
	bcasts      [][3]string // by, subject, body
	compacts    [][2]string // owner, focus
	schedules   [][]string  // by, target, subject, every, daily
	unschedules [][2]string // by, id
	webhooks    []string    // owners who asked for their URL
}

func (f *fakeBackend) Send(from, to, subject, body string, replyTo int, urgent bool) (int, error) {
	f.sends = append(f.sends, [4]string{from, to, subject, body})
	f.urgent = append(f.urgent, urgent)
	return 7, nil
}
func (f *fakeBackend) Contacts(owner string) []string { return []string{"0", "0.2"} }
func (f *fakeBackend) Inbox(owner string) []string    { return nil }
func (f *fakeBackend) Status(owner string) []string   { return nil }
func (f *fakeBackend) Compact(owner, focus string) error {
	f.compacts = append(f.compacts, [2]string{owner, focus})
	return nil
}
func (f *fakeBackend) Schedule(by, target, subject, body, every, daily string, urgent bool) (string, error) {
	f.schedules = append(f.schedules, []string{by, target, subject, every, daily})
	return "s-abcd", nil
}
func (f *fakeBackend) Unschedule(by, id string) error {
	f.unschedules = append(f.unschedules, [2]string{by, id})
	return nil
}
func (f *fakeBackend) Schedules(by string) []string { return []string{"[s-1] -> 0.2"} }
func (f *fakeBackend) Webhook(by, target string) (string, error) {
	f.webhooks = append(f.webhooks, by+"->"+target)
	return "http://127.0.0.1:8899/w/tok-" + target, nil
}
func (f *fakeBackend) WebhookRotate(by, target string) (string, error) {
	return "http://127.0.0.1:8899/w/new-" + target, nil
}
func (f *fakeBackend) Spawn(by, n, desc, d, model string) (string, error) { return "0.1.1", nil }
func (f *fakeBackend) Edit(by, addr, name, desc, model string) error {
	f.edits = append(f.edits, [5]string{by, addr, name, desc, model})
	return nil
}
func (f *fakeBackend) Delete(by, addr string) (int, error) {
	f.deletes = append(f.deletes, [2]string{by, addr})
	return 2, nil
}
func (f *fakeBackend) Forget(by, addr string) error {
	f.forgets = append(f.forgets, [2]string{by, addr})
	return nil
}
func (f *fakeBackend) Introduce(by, a, b string) error {
	f.intros = append(f.intros, [3]string{by, a, b})
	return nil
}
func (f *fakeBackend) Broadcast(by, subject, body string, urgent bool) (int, error) {
	f.bcasts = append(f.bcasts, [3]string{by, subject, body})
	return 3, nil
}

func (f *fakeBackend) ControlWebhook(by string, rotate bool) (string, error) {
	f.controls = append(f.controls, by)
	if rotate {
		return "http://127.0.0.1:8899/c/new-" + by, nil
	}
	return "http://127.0.0.1:8899/c/tok-" + by, nil
}

func (f *fakeBackend) AssignTask(by, to, brief, checklist string, deterministic bool) (string, error) {
	f.assigns = append(f.assigns, [4]string{by, to, brief, checklist})
	return "t1", nil
}

func (f *fakeBackend) SubmitTask(by, taskID, summary string) (string, error) {
	f.submits = append(f.submits, [3]string{by, taskID, summary})
	return "task " + taskID + " check passed — completion delivered", nil
}

func (f *fakeBackend) Verdict(by, taskID string, pass bool, notes string) (string, error) {
	f.verdicts = append(f.verdicts, verdictCall{by, taskID, pass, notes})
	return "task " + taskID + " ruled", nil
}

func (f *fakeBackend) Tasks(by string) []string { return []string{"t1 [open] you=worker"} }

func (f *fakeBackend) CancelTask(by, taskID string) error {
	f.cancels = append(f.cancels, [2]string{by, taskID})
	return nil
}

func (f *fakeBackend) LogDecision(by, text string) error {
	f.decisions = append(f.decisions, [2]string{by, text})
	return nil
}

type verdictCall struct {
	by, task string
	pass     bool
	notes    string
}

func TestServeFlow(t *testing.T) {
	in := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"send","arguments":{"to":"0","subject":"hi","body":"there"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"bogus","arguments":{}}}`,
	}, "\n"))

	fb := &fakeBackend{}
	s := &Server{Self: "0.1", B: fb, Spawnable: false}
	var out bytes.Buffer
	if err := s.Serve(in, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	type resp struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	var resps []resp
	dec := json.NewDecoder(&out)
	for dec.More() {
		var r resp
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("decode resp: %v", err)
		}
		resps = append(resps, r)
	}
	if len(resps) != 4 { // notification produces no response
		t.Fatalf("got %d responses want 4", len(resps))
	}

	// id1: initialize advertises a protocol version.
	var initR struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	json.Unmarshal(resps[0].Result, &initR)
	if initR.ProtocolVersion == "" {
		t.Fatal("initialize missing protocolVersion")
	}

	// id2: tools/list = send, contacts (no spawn when not Spawnable).
	var listR struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	json.Unmarshal(resps[1].Result, &listR)
	var names []string
	for _, tdef := range listR.Tools {
		names = append(names, tdef.Name)
	}
	if strings.Join(names, ",") != "send,contacts,inbox,status,forget,compact,schedule,unschedule,schedules,webhook,webhook_rotate,submit_task,verdict,tasks,cancel_task,log_decision" {
		t.Fatalf("tools = %v", names)
	}

	// id3: send succeeded and recorded identity = Self.
	var callR struct {
		IsError bool `json:"isError"`
	}
	json.Unmarshal(resps[2].Result, &callR)
	if callR.IsError {
		t.Fatal("send returned isError")
	}
	if len(fb.sends) != 1 || fb.sends[0] != [4]string{"0.1", "0", "hi", "there"} {
		t.Fatalf("backend sends = %v", fb.sends)
	}

	// id4: unknown tool = JSON-RPC error.
	if resps[3].Error == nil {
		t.Fatal("bogus tool should return a JSON-RPC error")
	}
}

func TestSpawnGated(t *testing.T) {
	base := &Server{Self: "0.1", B: &fakeBackend{}, Spawnable: false}
	if len(base.tools()) != 16 { // send..webhook_rotate + submit_task, verdict, tasks, cancel_task, log_decision
		t.Fatalf("base server should advertise 16 tools, got %d", len(base.tools()))
	}
	s := &Server{Self: "0.1", B: &fakeBackend{}, Spawnable: true}
	if len(s.tools()) != 23 { // + spawn, edit, delete, introduce, broadcast, control_webhook, assign_task
		t.Fatalf("spawnable server should advertise 23 tools, got %d", len(s.tools()))
	}
}

// TestCompactTool: compact is available to every bubble and relays with the
// caller's own identity + focus.
func TestCompactTool(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"compact","arguments":{"focus":"keep TODOs"}}}`)
	fb := &fakeBackend{}
	s := &Server{Self: "0.3", B: fb, Spawnable: false} // NOT spawnable, still has compact
	var out bytes.Buffer
	if err := s.Serve(in, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if len(fb.compacts) != 1 || fb.compacts[0] != [2]string{"0.3", "keep TODOs"} {
		t.Fatalf("compact relay = %v", fb.compacts)
	}
}

// TestForgetTool: forget is available to every bubble and relays with the
// caller's own identity.
func TestForgetTool(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"forget","arguments":{"addr":"0.2"}}}`)
	fb := &fakeBackend{}
	s := &Server{Self: "0.1", B: fb, Spawnable: false} // NOT spawnable, still has forget
	var out bytes.Buffer
	if err := s.Serve(in, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if len(fb.forgets) != 1 || fb.forgets[0] != [2]string{"0.1", "0.2"} {
		t.Fatalf("forget relay = %v", fb.forgets)
	}
}

// TestEditDeleteTools: a spawnable server relays edit/delete with its own
// identity; a non-spawnable server doesn't offer them at all.
func TestEditDeleteTools(t *testing.T) {
	in := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"edit","arguments":{"addr":"0.1.2","name":"tester","model":"opus"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"delete","arguments":{"addr":"0.1.3"}}}`,
	}, "\n"))
	fb := &fakeBackend{}
	s := &Server{Self: "0.1", B: fb, Spawnable: true}
	var out bytes.Buffer
	if err := s.Serve(in, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if len(fb.edits) != 1 || fb.edits[0] != [5]string{"0.1", "0.1.2", "tester", "", "opus"} {
		t.Fatalf("edit relay = %v", fb.edits)
	}
	if len(fb.deletes) != 1 || fb.deletes[0] != [2]string{"0.1", "0.1.3"} {
		t.Fatalf("delete relay = %v", fb.deletes)
	}
	if !strings.Contains(out.String(), "2 bubble(s) removed") {
		t.Fatalf("delete result should report the removed count, got %s", out.String())
	}

	// not spawnable -> the tools are refused
	in2 := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete","arguments":{"addr":"0.1.3"}}}`)
	fb2 := &fakeBackend{}
	var out2 bytes.Buffer
	s2 := &Server{Self: "0.1", B: fb2, Spawnable: false}
	if err := s2.Serve(in2, &out2); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if len(fb2.deletes) != 0 || !strings.Contains(out2.String(), "not available") {
		t.Fatalf("non-spawnable server must refuse delete: deletes=%v out=%s", fb2.deletes, out2.String())
	}
}

// TestIntroduceBroadcastTools: introduce/broadcast relay with the caller's own
// identity and are gated on the spawn grant.
func TestIntroduceBroadcastTools(t *testing.T) {
	in := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"introduce","arguments":{"a":"0.1.1","b":"0.1.2"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"broadcast","arguments":{"subject":"standup","body":"post status"}}}`,
	}, "\n"))
	fb := &fakeBackend{}
	s := &Server{Self: "0.1", B: fb, Spawnable: true}
	var out bytes.Buffer
	if err := s.Serve(in, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if len(fb.intros) != 1 || fb.intros[0] != [3]string{"0.1", "0.1.1", "0.1.2"} {
		t.Fatalf("introduce relay = %v", fb.intros)
	}
	if len(fb.bcasts) != 1 || fb.bcasts[0] != [3]string{"0.1", "standup", "post status"} {
		t.Fatalf("broadcast relay = %v", fb.bcasts)
	}
	if !strings.Contains(out.String(), "3 bubble(s) in your subtree") {
		t.Fatalf("broadcast should report the reach, got %s", out.String())
	}

	// not spawnable -> refused
	in2 := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"introduce","arguments":{"a":"0.1.1","b":"0.1.2"}}}`)
	fb2 := &fakeBackend{}
	var out2 bytes.Buffer
	if err := (&Server{Self: "0.1", B: fb2, Spawnable: false}).Serve(in2, &out2); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if len(fb2.intros) != 0 || !strings.Contains(out2.String(), "not available") {
		t.Fatalf("non-spawnable server must refuse introduce: %v %s", fb2.intros, out2.String())
	}
}

func TestTaskTools(t *testing.T) {
	in := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"assign_task","arguments":{"to":"0.1.1","brief":"fix adder","checklist":"a\nb"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"submit_task","arguments":{"task_id":"t1","summary":"done"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"verdict","arguments":{"task_id":"t1","pass":true,"notes":"all good"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"tasks","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"log_decision","arguments":{"text":"use sqlite — simpler"}}}`,
	}, "\n"))
	fb := &fakeBackend{}
	s := &Server{Self: "0.1", B: fb, Spawnable: true}
	var out bytes.Buffer
	if err := s.Serve(in, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if len(fb.assigns) != 1 || fb.assigns[0] != [4]string{"0.1", "0.1.1", "fix adder", "a\nb"} {
		t.Fatalf("assigns = %v", fb.assigns)
	}
	if len(fb.submits) != 1 || fb.submits[0] != [3]string{"0.1", "t1", "done"} {
		t.Fatalf("submits = %v", fb.submits)
	}
	if len(fb.verdicts) != 1 || !fb.verdicts[0].pass || fb.verdicts[0].notes != "all good" {
		t.Fatalf("verdicts = %+v", fb.verdicts)
	}
	if len(fb.decisions) != 1 || fb.decisions[0][1] != "use sqlite — simpler" {
		t.Fatalf("decisions = %v", fb.decisions)
	}
}

func TestAssignTaskRequiresSpawnable(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"assign_task","arguments":{"to":"0.2","brief":"x"}}}`)
	fb := &fakeBackend{}
	s := &Server{Self: "0.1", B: fb, Spawnable: false}
	var out bytes.Buffer
	if err := s.Serve(in, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if len(fb.assigns) != 0 {
		t.Fatalf("assign went through for non-spawnable bubble: %v", fb.assigns)
	}
	if !strings.Contains(out.String(), "not available") {
		t.Fatalf("expected not-available error, got %s", out.String())
	}
}
