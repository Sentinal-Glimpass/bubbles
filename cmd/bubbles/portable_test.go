package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/kernel"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
)

func TestClaudeSlug(t *testing.T) {
	if got := claudeSlug("/home/rishi/open_source"); got != "-home-rishi-open-source" {
		t.Fatalf("slug = %q", got)
	}
	if got := claudeSlug("/home/x/proj/.bubbles/ceo"); got != "-home-x-proj--bubbles-ceo" {
		t.Fatalf("dotdir slug = %q", got)
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	// isolate claude's session store in a fake HOME
	home := t.TempDir()
	t.Setenv("HOME", home)

	src := t.TempDir()
	// build a fleet with one launched bubble (so it has a session id + a conversation)
	k := kernel.New(runner.NewFake())
	k.RelaunchProbe = 0
	scoutDir := filepath.Join(src, "scout")
	a, _ := k.Spawn(addr.Root, "scout", scoutDir, runner.SpawnOpts{Name: "scout", Goal: "find bugs"})
	k.EnsureAlive(a)
	b, _ := k.Reg.Get(a)
	sid := b.SessionID
	k.Store.Append(inboxMsg(addr.Root, a, "hi", "world", 0))
	if err := saveFleet(src, k, map[int]addr.Address{}); err != nil {
		t.Fatal(err)
	}
	if err := saveInbox(src, k); err != nil {
		t.Fatal(err)
	}
	// plant a fake claude transcript for scout at its source slug
	proj := filepath.Join(home, ".claude", "projects", claudeSlug(scoutDir))
	os.MkdirAll(proj, 0o755)
	os.WriteFile(filepath.Join(proj, sid+".jsonl"), []byte(`{"type":"user","text":"transcript"}`), 0o644)
	// plant working files in scout's dir: one text, one media (dropped in text mode)
	os.MkdirAll(scoutDir, 0o755)
	os.WriteFile(filepath.Join(scoutDir, "notes.md"), []byte("# notes"), 0o644)
	os.WriteFile(filepath.Join(scoutDir, "logo.png"), []byte("PNGDATA"), 0o644)

	// export with the text scope: notes.md travels, logo.png does not
	blob := filepath.Join(t.TempDir(), "fleet.tgz")
	n, files, _, err := exportFleet(src, blob, "text", false)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 conversation bundled, got %d", n)
	}
	if files != 1 {
		t.Fatalf("text scope should bundle only notes.md, got %d files", files)
	}

	// import onto a DIFFERENT base (simulating another machine, same fake HOME)
	dst := t.TempDir()
	if _, err := importFleet(blob, dst, false); err != nil {
		t.Fatalf("import: %v", err)
	}

	// fleet.json restored with the dir rebased under dst
	fm, ok := loadFleet(dst)
	if !ok {
		t.Fatal("no fleet after import")
	}
	var newDir string
	for _, br := range fm.Bubbles {
		if br.Addr == a.String() {
			newDir = br.Dir
		}
	}
	wantDir := filepath.Join(dst, "scout")
	if newDir != wantDir {
		t.Fatalf("scout dir = %q want rebased %q", newDir, wantDir)
	}
	// the conversation was placed at the NEW slug so --resume will find it
	placed := filepath.Join(home, ".claude", "projects", claudeSlug(newDir), sid+".jsonl")
	if data, err := os.ReadFile(placed); err != nil || !strings.Contains(string(data), "transcript") {
		t.Fatalf("conversation not relocated to %q: %v", placed, err)
	}
	// the text working file was restored under the rebased dir; the media file was not
	if data, err := os.ReadFile(filepath.Join(newDir, "notes.md")); err != nil || string(data) != "# notes" {
		t.Fatalf("notes.md not restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(newDir, "logo.png")); !os.IsNotExist(err) {
		t.Fatal("logo.png should NOT be in a text-scope export")
	}
	// inbox came across
	k2 := kernel.New(runner.NewFake())
	if _, ok := loadInbox(dst, k2); !ok {
		t.Fatal("inbox not imported")
	}
	if k2.Store.UnreadCount(a) != 1 {
		t.Fatalf("unread mail lost across export/import: %d", k2.Store.UnreadCount(a))
	}

	// safety: a second import without --force must refuse
	if _, err := importFleet(blob, dst, false); err == nil {
		t.Fatal("import over an existing fleet should require --force")
	}
	if _, err := importFleet(blob, dst, true); err != nil {
		t.Fatalf("forced re-import should succeed: %v", err)
	}
}
