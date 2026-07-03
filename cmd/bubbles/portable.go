package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// portManifest is the export blob's top-level descriptor. The fleet structure
// itself travels as a verbatim copy of fleet.json/inbox.json inside the blob;
// this records what's needed to REBASE it onto a different machine (the original
// base dir, so per-bubble dirs can be relocated under the import target).
type portManifest struct {
	Version    int    `json:"version"`
	Created    string `json:"created"`
	SourceBase string `json:"sourceBase"`      // absolute base dir on the exporting machine
	MsgPoll    int    `json:"msgPollMinutes"`  // the message-poll setting, for reference on import
	Convos     int    `json:"conversations"`   // how many claude transcripts are bundled
}

// claudeSlug reproduces Claude Code's per-project session-store key: the absolute
// working dir with every non-alphanumeric character replaced by '-'
// (e.g. /home/rishi/open_source -> -home-rishi-open-source). Conversations live
// at ~/.claude/projects/<slug>/<session-id>.jsonl.
func claudeSlug(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	var b strings.Builder
	for _, r := range abs {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// convPath is where a bubble's claude transcript lives for a given working dir.
func convPath(home, dir, sessionID string) string {
	return filepath.Join(home, ".claude", "projects", claudeSlug(dir), sessionID+".jsonl")
}

// rebaseDir relocates a source-machine dir under the import target: if it was
// inside the source base it keeps its relative position; otherwise it's nested
// under the target by basename (best effort).
func rebaseDir(dir, srcBase, dstBase string) string {
	if rel, err := filepath.Rel(srcBase, dir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return filepath.Clean(filepath.Join(dstBase, rel))
	}
	return filepath.Join(dstBase, filepath.Base(dir))
}

// runExport bundles the workspace's fleet, inbox, and every bubble's claude
// conversation into a single .tgz blob that `bubbles import` can rehydrate on
// another machine. Usage: bubbles export [outfile]
func runExport(args []string) {
	base := defaultWorkspace()
	out := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			out = a
		}
	}
	if out == "" {
		out = fmt.Sprintf("bubbles-export-%s.tgz", time.Now().Format("20060102-150405"))
	}
	n, err := exportFleet(base, out)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("exported fleet + inbox + %d conversation(s) -> %s\n", n, out)
	fmt.Println("(reflects the last saved state; detach the client first for a fully current export)")
	fmt.Printf("import it elsewhere with:  bubbles import %s\n", filepath.Base(out))
}

// exportFleet writes the blob and returns how many conversations were bundled.
func exportFleet(base, outPath string) (int, error) {
	fm, ok := loadFleet(base)
	if !ok {
		return 0, fmt.Errorf("no fleet found in %s (start bubbles there first)", base)
	}
	absBase, _ := filepath.Abs(base)
	home, _ := os.UserHomeDir()

	f, err := os.Create(outPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	convos := 0
	// count conversations first so the manifest is accurate
	for _, b := range fm.Bubbles {
		if b.SessionID != "" && b.Dir != "" {
			if _, e := os.Stat(convPath(home, b.Dir, b.SessionID)); e == nil {
				convos++
			}
		}
	}
	man := portManifest{Version: 1, Created: time.Now().UTC().Format(time.RFC3339), SourceBase: absBase, MsgPoll: messagePollMinutes(), Convos: convos}
	manData, _ := json.MarshalIndent(man, "", "  ")
	if err := tarWrite(tw, "manifest.json", manData); err != nil {
		return 0, err
	}
	if err := tarCopy(tw, "fleet.json", fleetPath(base)); err != nil {
		return 0, err
	}
	_ = tarCopy(tw, "inbox.json", inboxPath(base)) // optional (no mail yet is fine)

	for _, b := range fm.Bubbles {
		if b.SessionID == "" || b.Dir == "" {
			continue
		}
		if data, e := os.ReadFile(convPath(home, b.Dir, b.SessionID)); e == nil {
			if err := tarWrite(tw, "conversations/"+b.SessionID+".jsonl", data); err != nil {
				return 0, err
			}
		}
	}
	return convos, nil
}

// runImport rehydrates a blob into a target workspace (default: cwd), relocating
// bubble dirs and placing each conversation in claude's store so `--resume` finds
// it. Usage: bubbles import <file> [--dir target] [--force]
func runImport(args []string) {
	blob, target, force := "", defaultWorkspace(), false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dir":
			if i+1 < len(args) {
				target = args[i+1]
				i++
			}
		case "--force":
			force = true
		default:
			if !strings.HasPrefix(args[i], "-") {
				blob = args[i]
			}
		}
	}
	if blob == "" {
		fatal(fmt.Errorf("usage: bubbles import <file.tgz> [--dir target] [--force]"))
	}
	summary, err := importFleet(blob, target, force)
	if err != nil {
		fatal(err)
	}
	fmt.Print(summary)
}

// importFleet extracts blob into target, rebases each bubble's dir, writes the
// fleet/inbox, and drops each conversation into ~/.claude/projects so a later
// launch resumes it. Returns a human summary.
func importFleet(blob, target string, force bool) (string, error) {
	if _, ok := loadFleet(target); ok && !force {
		return "", fmt.Errorf(".bubbles/fleet.json already exists in %s — use --force to overwrite", target)
	}
	entries, man, err := readBlob(blob)
	if err != nil {
		return "", err
	}
	fleetData, ok := entries["fleet.json"]
	if !ok {
		return "", fmt.Errorf("blob has no fleet.json — not a bubbles export")
	}
	var fm manifest
	if err := json.Unmarshal(fleetData, &fm); err != nil {
		return "", fmt.Errorf("blob fleet.json is unreadable: %w", err)
	}
	absTarget, _ := filepath.Abs(target)
	home, _ := os.UserHomeDir()

	placed := 0
	for i := range fm.Bubbles {
		b := &fm.Bubbles[i]
		newDir := rebaseDir(b.Dir, man.SourceBase, absTarget)
		if b.SessionID != "" {
			if data, ok := entries["conversations/"+b.SessionID+".jsonl"]; ok {
				proj := filepath.Join(home, ".claude", "projects", claudeSlug(newDir))
				if os.MkdirAll(proj, 0o755) == nil &&
					os.WriteFile(filepath.Join(proj, b.SessionID+".jsonl"), data, 0o644) == nil {
					placed++
				}
			}
		}
		b.Dir = newDir
		_ = os.MkdirAll(newDir, 0o755)
	}

	bdir := filepath.Join(target, ".bubbles")
	if err := os.MkdirAll(bdir, 0o755); err != nil {
		return "", err
	}
	_ = os.WriteFile(filepath.Join(bdir, ".gitignore"), []byte("*\n"), 0o644)
	out, _ := json.MarshalIndent(fm, "", "  ")
	if err := os.WriteFile(fleetPath(target), out, 0o644); err != nil {
		return "", err
	}
	if inboxData, ok := entries["inbox.json"]; ok {
		_ = os.WriteFile(inboxPath(target), inboxData, 0o644)
	}

	var s strings.Builder
	fmt.Fprintf(&s, "imported %d bubble(s) into %s\n", len(fm.Bubbles), absTarget)
	fmt.Fprintf(&s, "restored %d conversation(s) (best effort — any that don't resume start fresh, keeping their inbox)\n", placed)
	if man.MsgPoll > 0 && man.MsgPoll != 10 {
		fmt.Fprintf(&s, "note: source ran with --message_polling %d\n", man.MsgPoll)
	}
	fmt.Fprintf(&s, "run `bubbles` in %s to bring the fleet up.\n", absTarget)
	return s.String(), nil
}

// readBlob reads a .tgz export into memory: every file by name, plus the parsed
// manifest.
func readBlob(path string) (map[string][]byte, portManifest, error) {
	var man portManifest
	f, err := os.Open(path)
	if err != nil {
		return nil, man, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, man, fmt.Errorf("%s is not a gzip blob: %w", path, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	entries := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, man, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, man, err
		}
		entries[h.Name] = data
	}
	if m, ok := entries["manifest.json"]; ok {
		_ = json.Unmarshal(m, &man)
	}
	return entries, man, nil
}

func tarWrite(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

func tarCopy(tw *tar.Writer, name, srcPath string) error {
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return err
	}
	return tarWrite(tw, name, data)
}
