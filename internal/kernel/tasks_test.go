package kernel

import (
	"errors"
	"strings"
	"testing"
	"time"

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
	// A worker cannot assign upward (to its parent).
	if _, err := k.AssignTask(worker, boss, "x", []string{"item"}, false); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("worker->boss assign: %v", err)
	}
	// A bubble CAN assign a task to itself (self-imposed, still verified).
	if _, err := k.AssignTask(worker, worker, "self task", []string{"done"}, false); err != nil {
		t.Fatalf("worker->self assign should be allowed: %v", err)
	}
	// The boss can assign into its subtree; root can assign to anyone.
	if _, err := k.AssignTask(boss, worker, "do x", []string{"tests pass"}, false); err != nil {
		t.Fatalf("boss->worker: %v", err)
	}
	if _, err := k.AssignTask(addr.Root, worker, "do y", []string{"tests pass"}, false); err != nil {
		t.Fatalf("root->worker: %v", err)
	}
	// Empty contract is rejected: an ungated task is just a message.
	if _, err := k.AssignTask(boss, worker, "no contract", nil, false); err == nil {
		t.Fatal("empty contract accepted")
	}
	// Assignment lands in the worker's inbox with the submit instructions.
	if _, body := lastTo(k, worker); !strings.Contains(body, "submit_task") {
		t.Fatalf("assignment body missing instructions: %q", body)
	}
}

func TestDeterministicFlagReachesVerifier(t *testing.T) {
	k, boss, worker := taskKernel(t)
	k.VerifierReap = func(a addr.Address) { k.DeleteBubble(a) }

	id, err := k.AssignTask(boss, worker, "fix the adder", []string{"adder works correctly"}, true)
	if err != nil {
		t.Fatal(err)
	}
	tk, _ := k.Tasks.Get(id)
	if !tk.Deterministic {
		t.Fatal("task should be deterministic")
	}
	// No verifier exists yet — it's spawned lazily on the first submission.
	if tk.Verifier != "" {
		t.Fatalf("verifier should not exist before submission, got %s", tk.Verifier)
	}

	// Wrong caller can't submit.
	if _, err := k.SubmitTask(boss, id, "done"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("boss submit: %v", err)
	}

	// Worker submits → verifier is spawned now, with the submission in its charter.
	out, err := k.SubmitTask(worker, id, "fixed it")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "verifier") {
		t.Fatalf("submit should route to verifier: %q", out)
	}
	tk, _ = k.Tasks.Get(id)
	if tk.State != tasks.Checking {
		t.Fatalf("state = %s want checking", tk.State)
	}
	if tk.Verifier == "" {
		t.Fatal("verifier should be spawned after first submission")
	}
	vb, _ := k.Reg.Get(tk.Verifier)
	if !strings.Contains(vb.Goal, "DETERMINISTIC TEST CASES") {
		t.Fatalf("verifier charter missing deterministic instruction")
	}
	if !strings.Contains(vb.Goal, "fixed it") {
		t.Fatalf("verifier charter should contain the submission: %q", vb.Goal)
	}
	// Verifier approves.
	if _, err := k.TaskVerdict(tk.Verifier, id, true, "test passes"); err != nil {
		t.Fatal(err)
	}
	s, b := lastTo(k, boss)
	if !strings.Contains(s, "\xe2\x9c\x85 task "+id+" verified") || !strings.Contains(b, "fixed it") {
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
	var reaped []addr.Address
	k.VerifierReap = func(a addr.Address) { reaped = append(reaped, a); k.DeleteBubble(a) }

	id, err := k.AssignTask(boss, worker, "write the docs", []string{"docs cover every tool", "examples compile"}, false)
	if err != nil {
		t.Fatal(err)
	}
	// Verifier is spawned lazily — not until the first submission.
	tk, _ := k.Tasks.Get(id)
	if tk.Verifier != "" {
		t.Fatal("verifier should not exist before submission")
	}

	// First submission spawns the verifier with the submission in its charter.
	if _, err := k.SubmitTask(worker, id, "docs written"); err != nil {
		t.Fatal(err)
	}
	tk, _ = k.Tasks.Get(id)
	if tk.Verifier == "" {
		t.Fatal("verifier not spawned after first submission")
	}
	vb, ok := k.Reg.Get(tk.Verifier)
	if !ok || vb.Label() != "verify:"+id || !strings.Contains(vb.Goal, "INDEPENDENT VERIFIER") || !strings.Contains(vb.Goal, "docs written") {
		t.Fatalf("verifier bubble: %+v", vb)
	}
	if act := k.Tasks.Active(); len(act) != 1 || act[0].ID != id {
		t.Fatalf("active tasks = %+v", act)
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
	id, err := k.AssignTask(boss, worker, "do x", []string{"tests pass"}, false)
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
	// After completion (submit + verifier approves) the annotation stops.
	if _, err := k.SubmitTask(worker, id, "done"); err != nil {
		t.Fatal(err)
	}
	tk, _ := k.Tasks.Get(id)
	if _, err := k.TaskVerdict(tk.Verifier, id, true, "ok"); err != nil {
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
	id, _ := k.AssignTask(boss, worker, "do x", []string{"item"}, false)
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
	id, _ := k.AssignTask(boss, worker, "do x", []string{"tests pass"}, false)
	k.DeleteBubble(worker)
	tk, _ := k.Tasks.Get(id)
	if tk.State != tasks.Cancelled {
		t.Fatalf("state after worker delete = %s", tk.State)
	}
}

func TestTasksFor(t *testing.T) {
	k, boss, worker := taskKernel(t)
	id, _ := k.AssignTask(boss, worker, "brief line one\nmore", []string{"done"}, false)
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

func TestAlwaysOnReceiver(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	k.IdleTimeout = time.Millisecond // aggressive idle so we can prove exemption
	a, _ := k.Spawn(addr.Root, "recv", "/tmp/recv", runner.SpawnOpts{Name: "recv"})
	k.EnsureAlive(a)

	// Not always-on: an idle sweep pages it out.
	time.Sleep(2 * time.Millisecond)
	k.EvictIdle()
	if k.IsHot(a) {
		t.Fatal("a normal idle bubble should be evicted")
	}

	// Mark always-on + keep-alive → it comes back hot and stays hot across sweeps.
	k.Reg.SetAlwaysOn(a, true)
	k.KeepAlive()
	if !k.IsHot(a) {
		t.Fatal("KeepAlive should launch an always-on receiver")
	}
	time.Sleep(2 * time.Millisecond)
	k.EvictIdle()
	if !k.IsHot(a) {
		t.Fatal("always-on receiver must be exempt from idle eviction")
	}
}

// TestReapOrphanVerifiersIsPeriodicSafe pins the properties that let this run
// on the supervisor's sweep goroutine every few minutes instead of only once at
// boot. The orphan here is made the way a LIVE run makes one, not the way a
// restart does: deleting a task's worker cancels the task via
// Tasks.PurgeParticipant, which leaves the verifier bubble resident forever.
func TestReapOrphanVerifiersIsPeriodicSafe(t *testing.T) {
	k, boss, worker := taskKernel(t)
	k.VerifierReap = func(a addr.Address) { k.DeleteBubble(a) }

	id, err := k.AssignTask(boss, worker, "do x", []string{"item"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.SubmitTask(worker, id, "done"); err != nil {
		t.Fatal(err)
	}
	tk, _ := k.Tasks.Get(id)
	verifier := tk.Verifier
	if verifier == "" {
		t.Fatal("submission did not spawn a verifier")
	}

	// A reap while the task is still Checking must not touch the verifier: it
	// is mid-verdict, the exact bubble a careless sweep would delete.
	if n := k.ReapOrphanVerifiers(); n != 0 {
		t.Fatalf("reaped %d verifiers of a live (checking) task, want 0", n)
	}
	if _, ok := k.Reg.Get(verifier); !ok {
		t.Fatal("the verifier of a task awaiting a verdict was deleted")
	}
	// ...and it is still able to rule, i.e. nothing was disturbed.
	if _, err := k.TaskVerdict(verifier, id, false, "not yet"); err != nil {
		t.Fatalf("verifier could not rule after a reap pass: %v", err)
	}

	// Now orphan it the live way: delete the worker. PurgeParticipant cancels
	// the task and leaves the verifier behind.
	k.DeleteBubble(worker)
	tk, _ = k.Tasks.Get(id)
	if tk.State != tasks.Cancelled {
		t.Fatalf("task state = %s, want cancelled", tk.State)
	}
	if _, ok := k.Reg.Get(verifier); !ok {
		t.Fatal("test premise broken: the verifier was already gone")
	}

	if n := k.ReapOrphanVerifiers(); n != 1 {
		t.Fatalf("reaped %d, want 1 (the orphaned verifier)", n)
	}
	if _, ok := k.Reg.Get(verifier); ok {
		t.Fatal("orphaned verifier survived the reap")
	}
	// Idempotent: every later tick over the same state is a no-op.
	for i := 0; i < 3; i++ {
		if n := k.ReapOrphanVerifiers(); n != 0 {
			t.Fatalf("repeat reap %d removed %d, want 0", i, n)
		}
	}
	// The sweep must not have woken anything. The fake runner records launches.
	if _, ok := k.Reg.Get(boss); !ok {
		t.Fatal("the reap deleted an unrelated bubble")
	}
}

// TestReapOrphanVerifiersConcurrentWithCompletion drives the reap alongside
// live task completion under -race. The reap must never delete a verifier that
// is at that instant being handed a verdict, and must never race the task store.
func TestReapOrphanVerifiersConcurrentWithCompletion(t *testing.T) {
	k, boss, worker := taskKernel(t)
	k.VerifierReap = func(a addr.Address) { k.DeleteBubble(a) }

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				k.ReapOrphanVerifiers()
			}
		}
	}()

	for i := 0; i < 40; i++ {
		id, err := k.AssignTask(boss, worker, "do x", []string{"item"}, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := k.SubmitTask(worker, id, "done"); err != nil {
			t.Fatal(err)
		}
		tk, _ := k.Tasks.Get(id)
		// The verdict must land even with the reaper running flat out: a task
		// in Checking is never a reap target, so its verifier is still there.
		if _, err := k.TaskVerdict(tk.Verifier, id, true, "ok"); err != nil {
			t.Fatalf("round %d: verdict lost to a concurrent reap: %v", i, err)
		}
	}
	close(stop)
	<-done
}

// TestReapExpiredMutesUsesKernelClock proves the sweep's entry point reaps via
// the kernel's injectable clock and leaves live rules alone.
func TestReapExpiredMutesUsesKernelClock(t *testing.T) {
	k, boss, _ := taskKernel(t)
	created := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	k.now = func() time.Time { return created }

	if _, err := k.MuteBy(boss, "pump", "", "", "1m", "1h"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.MuteBy(boss, "other", "", "", "1m", ""); err != nil {
		t.Fatal(err)
	}
	if n := k.ReapExpiredMutes(); n != 0 {
		t.Fatalf("reaped %d rules before any TTL elapsed, want 0", n)
	}

	k.now = func() time.Time { return created.Add(2 * time.Hour) }
	if n := k.ReapExpiredMutes(); n != 1 {
		t.Fatalf("reaped %d, want 1", n)
	}
	rules := k.Reg.MuteRules(boss)
	if len(rules) != 1 || rules[0].Source != "other" {
		t.Fatalf("survivors = %+v, want the TTL-less rule only", rules)
	}
	// Quota is genuinely freed: MuteBy rebuilds from the stored set.
	if _, err := k.MuteBy(boss, "third", "", "", "1m", ""); err != nil {
		t.Fatalf("adding a rule after a reap failed: %v", err)
	}
}
