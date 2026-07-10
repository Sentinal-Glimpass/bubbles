package kernel

import (
	"errors"
	"strings"
	"testing"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
	"github.com/Sentinal-Glimpass/bubbles/internal/tasks"
)

func taskKernel(t *testing.T) (*Kernel, addr.Address, addr.Address) {
	t.Helper()
	k := New(runner.NewFake())
	k.RelaunchProbe = 0
	boss, err := k.Spawn(addr.Root, "boss", "/tmp/boss", runner.SpawnOpts{Name: "boss", GrantSpawn: true})
	if err != nil {
		t.Fatal(err)
	}
	k.Caps.GrantSpawnDepth(boss, 2)
	worker, err := k.SpawnUnder(boss, boss, "worker", "/tmp/worker", runner.SpawnOpts{Name: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	return k, boss, worker
}

func lastTo(k *Kernel, to addr.Address) (subject, body string) {
	all := k.Store.All(to)
	if len(all) == 0 {
		return "", ""
	}
	m := all[len(all)-1]
	return m.Subject, m.Body
}

func TestAssignAuthorityFollowsTree(t *testing.T) {
	k, boss, worker := taskKernel(t)
	// A worker cannot assign upward or to itself.
	if _, err := k.AssignTask(worker, boss, "x", "true", nil); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("worker->boss assign: %v", err)
	}
	// The boss can assign into its subtree; root can assign to anyone.
	if _, err := k.AssignTask(boss, worker, "do x", "true", nil); err != nil {
		t.Fatalf("boss->worker: %v", err)
	}
	if _, err := k.AssignTask(addr.Root, worker, "do y", "true", nil); err != nil {
		t.Fatalf("root->worker: %v", err)
	}
	// Empty contract is rejected: an ungated task is just a message.
	if _, err := k.AssignTask(boss, worker, "no contract", "", nil); err == nil {
		t.Fatal("empty contract accepted")
	}
	// Assignment lands in the worker's inbox with the submit instructions.
	if _, body := lastTo(k, worker); !strings.Contains(body, "submit_task") {
		t.Fatalf("assignment body missing instructions: %q", body)
	}
}

func TestDeterministicRoute(t *testing.T) {
	k, boss, worker := taskKernel(t)
	fail := true
	k.RunCheck = func(dir, cmd string) (bool, string) {
		if fail {
			return false, "TestFoo FAILED: expected 2 got 3"
		}
		return true, "ok"
	}
	id, err := k.AssignTask(boss, worker, "fix the adder", "go test ./...", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Wrong caller can't submit.
	if _, err := k.SubmitTask(boss, id, "done"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("boss submit: %v", err)
	}

	// Failing check bounces to the worker WITH the output; assigner sees nothing.
	if _, err := k.SubmitTask(worker, id, "fixed it"); err == nil || !strings.Contains(err.Error(), "TestFoo FAILED") {
		t.Fatalf("want check failure with output, got %v", err)
	}
	tk, _ := k.Tasks.Get(id)
	if tk.State != tasks.Open || tk.Rounds != 1 {
		t.Fatalf("after fail: %+v", tk)
	}
	if s, _ := lastTo(k, boss); strings.Contains(s, "✅") {
		t.Fatalf("assigner saw a completion for a failed check: %q", s)
	}

	// Passing check delivers the kernel-composed verified notice to the assigner.
	fail = false
	out, err := k.SubmitTask(worker, id, "fixed it properly")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "closed") {
		t.Fatalf("submit result: %q", out)
	}
	s, b := lastTo(k, boss)
	if !strings.Contains(s, "✅ task "+id+" verified") || !strings.Contains(b, "fixed it properly") {
		t.Fatalf("completion notice: %q / %q", s, b)
	}
	tk, _ = k.Tasks.Get(id)
	if tk.State != tasks.Done {
		t.Fatalf("state = %s", tk.State)
	}
	// A closed task can't be resubmitted.
	if _, err := k.SubmitTask(worker, id, "again"); err == nil {
		t.Fatal("resubmit of a done task accepted")
	}
}

func TestChecklistRouteThroughVerifier(t *testing.T) {
	k, boss, worker := taskKernel(t)
	k.RunCheck = func(dir, cmd string) (bool, string) { return true, "ok" }
	var reaped []addr.Address
	k.VerifierReap = func(a addr.Address) { reaped = append(reaped, a); k.DeleteBubble(a) }

	id, err := k.AssignTask(boss, worker, "write the docs", "true", []string{"docs cover every tool", "examples compile"})
	if err != nil {
		t.Fatal(err)
	}
	tk, _ := k.Tasks.Get(id)
	if tk.Verifier == "" {
		t.Fatal("no verifier spawned for a checklist task")
	}
	vb, ok := k.Reg.Get(tk.Verifier)
	if !ok || vb.Label() != "verify:"+id || !strings.Contains(vb.Goal, "INDEPENDENT VERIFIER") {
		t.Fatalf("verifier bubble: %+v", vb)
	}
	if act := k.Tasks.Active(); len(act) != 1 || act[0].ID != id {
		t.Fatalf("active tasks = %+v", act)
	}

	// Submission goes to the VERIFIER, not the assigner.
	if _, err := k.SubmitTask(worker, id, "docs written"); err != nil {
		t.Fatal(err)
	}
	if s, _ := lastTo(k, tk.Verifier); !strings.Contains(s, "task "+id+" submission") {
		t.Fatalf("verifier inbox: %q", s)
	}

	// Worker can't rule on its own task.
	if _, err := k.TaskVerdict(worker, id, true, "lgtm"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("worker verdict: %v", err)
	}

	// Reject: feedback reaches the worker, task reopens, verifier survives.
	if _, err := k.TaskVerdict(tk.Verifier, id, false, "examples do not compile: fix snippet 2"); err != nil {
		t.Fatal(err)
	}
	if s, b := lastTo(k, worker); !strings.Contains(s, "❌ task "+id) || !strings.Contains(b, "snippet 2") {
		t.Fatalf("reject feedback: %q / %q", s, b)
	}
	tk2, _ := k.Tasks.Get(id)
	if tk2.State != tasks.Open || tk2.Rounds != 1 {
		t.Fatalf("after reject: %+v", tk2)
	}

	// Resubmit → approve: assigner gets the verified notice; verifier reaped.
	if _, err := k.SubmitTask(worker, id, "fixed snippet 2"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.TaskVerdict(tk.Verifier, id, true, "all items pass"); err != nil {
		t.Fatal(err)
	}
	if s, _ := lastTo(k, boss); !strings.Contains(s, "✅ task "+id+" verified") {
		t.Fatalf("assigner notice: %q", s)
	}
	if len(reaped) != 1 || reaped[0] != tk.Verifier {
		t.Fatalf("verifier not reaped: %v", reaped)
	}
	if _, ok := k.Reg.Get(tk.Verifier); ok {
		t.Fatal("verifier bubble still registered")
	}
	if act := k.Tasks.Active(); len(act) != 0 {
		t.Fatalf("task still active after completion: %+v", act)
	}
}

func TestSendAnnotatedWhileTaskOpen(t *testing.T) {
	k, boss, worker := taskKernel(t)
	id, err := k.AssignTask(boss, worker, "do x", "true", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Worker→assigner while open: subject is marked unverified.
	if _, err := k.Send(worker, boss, "DONE! all finished", "trust me", 0, false); err != nil {
		t.Fatal(err)
	}
	if s, _ := lastTo(k, boss); !strings.Contains(s, "[task "+id+" open — unverified]") {
		t.Fatalf("unannotated claim got through: %q", s)
	}
	// Other directions are untouched.
	if _, err := k.Send(boss, worker, "how is it going", "", 0, false); err != nil {
		t.Fatal(err)
	}
	if s, _ := lastTo(k, worker); strings.Contains(s, "unverified") {
		t.Fatalf("assigner->worker wrongly annotated: %q", s)
	}
	// After completion the annotation stops.
	k.RunCheck = func(dir, cmd string) (bool, string) { return true, "" }
	if _, err := k.SubmitTask(worker, id, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Send(worker, boss, "follow-up", "", 0, false); err != nil {
		t.Fatal(err)
	}
	if s, _ := lastTo(k, boss); strings.Contains(s, "unverified") {
		t.Fatalf("annotation persisted past completion: %q", s)
	}
}

func TestCancelTask(t *testing.T) {
	k, boss, worker := taskKernel(t)
	id, _ := k.AssignTask(boss, worker, "do x", "", []string{"item"})
	tk, _ := k.Tasks.Get(id)
	if err := k.CancelTask(worker, id); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("worker cancel: %v", err)
	}
	if err := k.CancelTask(boss, id); err != nil {
		t.Fatal(err)
	}
	tk2, _ := k.Tasks.Get(id)
	if tk2.State != tasks.Cancelled {
		t.Fatalf("state = %s", tk2.State)
	}
	if _, ok := k.Reg.Get(tk.Verifier); ok {
		t.Fatal("verifier survived cancel")
	}
	if s, _ := lastTo(k, worker); !strings.Contains(s, "cancelled") {
		t.Fatalf("worker not told: %q", s)
	}
}

func TestDeleteWorkerCancelsItsTasks(t *testing.T) {
	k, boss, worker := taskKernel(t)
	id, _ := k.AssignTask(boss, worker, "do x", "true", nil)
	k.DeleteBubble(worker)
	tk, _ := k.Tasks.Get(id)
	if tk.State != tasks.Cancelled {
		t.Fatalf("state after worker delete = %s", tk.State)
	}
}

func TestTasksFor(t *testing.T) {
	k, boss, worker := taskKernel(t)
	id, _ := k.AssignTask(boss, worker, "brief line one\nmore", "true", nil)
	ls := k.TasksFor(worker)
	if len(ls) != 1 || !strings.Contains(ls[0], id) || !strings.Contains(ls[0], "you=worker") || !strings.Contains(ls[0], "brief line one") {
		t.Fatalf("tasks for worker: %v", ls)
	}
	if ls := k.TasksFor(boss); len(ls) != 1 || !strings.Contains(ls[0], "you=assigner") {
		t.Fatalf("tasks for boss: %v", ls)
	}
}

func TestControlWebhookRequiresSpawnAndBase(t *testing.T) {
	k := New(runner.NewFake())
	k.RelaunchProbe = 0
	// No webhook base configured → unavailable even for root.
	if _, err := k.ControlWebhookURL(addr.Root); err == nil {
		t.Fatal("expected error when webhook base is unset")
	}
	k.WebhookBase = "http://127.0.0.1:8899"

	boss, _ := k.Spawn(addr.Root, "boss", "/tmp/boss", runner.SpawnOpts{Name: "boss", GrantSpawn: true})
	worker, _ := k.Spawn(addr.Root, "worker", "/tmp/worker", runner.SpawnOpts{Name: "worker"}) // no spawn grant

	// Spawn-capable bubble gets a stable /c/ URL; a fresh call returns the same.
	u1, err := k.ControlWebhookURL(boss)
	if err != nil || !strings.Contains(u1, "/c/") {
		t.Fatalf("boss control url: %q err=%v", u1, err)
	}
	if u2, _ := k.ControlWebhookURL(boss); u2 != u1 {
		t.Fatalf("control url not stable: %q vs %q", u1, u2)
	}
	// Resolves back to the owning bubble.
	if got, ok := k.ResolveControlToken(strings.TrimPrefix(u1, "http://127.0.0.1:8899/c/")); !ok || got != boss {
		t.Fatalf("resolve = %s ok=%v", got, ok)
	}
	// A non-spawn bubble is denied a control surface entirely.
	if _, err := k.ControlWebhookURL(worker); err != ErrNotAllowed {
		t.Fatalf("worker control url err = %v, want ErrNotAllowed", err)
	}
	// Rotation revokes the old token.
	u3, err := k.RotateControlWebhook(boss)
	if err != nil || u3 == u1 {
		t.Fatalf("rotate: %q err=%v", u3, err)
	}
	if _, ok := k.ResolveControlToken(strings.TrimPrefix(u1, "http://127.0.0.1:8899/c/")); ok {
		t.Fatal("old control token still resolves after rotate")
	}
}
