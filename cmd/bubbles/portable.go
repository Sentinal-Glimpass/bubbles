package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"
)

// portManifest is the export blob's top-level descriptor. The fleet structure
// travels as a verbatim copy of fleet.json/inbox.json inside the blob; this
// records what's needed to REBASE it onto a different machine (the original base
// dir) plus what file scope was captured.
type portManifest struct {
	Version    int    `json:"version"`
	Created    string `json:"created"`
	SourceBase string `json:"sourceBase"`     // absolute base dir on the exporting machine
	MsgPoll    int    `json:"msgPollMinutes"` // the message-poll setting, for reference on import
	Convos     int    `json:"conversations"`  // how many claude transcripts are bundled
	Files      string `json:"files"`          // scope: metadata | text | nomedia | all
	FileCount  int    `json:"fileCount"`      // working files bundled
	Full       bool   `json:"full,omitempty"` // --full: includes env, schedules, plugins, mcp, identity
}

// exportModes are the four working-file scopes.
var exportModes = map[string]bool{"metadata": true, "text": true, "nomedia": true, "all": true}

// textExt / textName select what counts as a text/code file in "text" mode.
var textExt = map[string]bool{
	".txt": true, ".md": true, ".markdown": true, ".rst": true, ".json": true, ".jsonc": true,
	".yaml": true, ".yml": true, ".toml": true, ".ini": true, ".cfg": true, ".conf": true, ".env": true,
	".csv": true, ".tsv": true, ".xml": true, ".html": true, ".htm": true, ".css": true, ".scss": true, ".sass": true,
	".js": true, ".jsx": true, ".mjs": true, ".cjs": true, ".ts": true, ".tsx": true, ".vue": true, ".svelte": true,
	".go": true, ".py": true, ".rb": true, ".rs": true, ".java": true, ".kt": true, ".kts": true, ".scala": true,
	".c": true, ".h": true, ".cpp": true, ".hpp": true, ".cc": true, ".cs": true, ".m": true, ".mm": true,
	".php": true, ".sh": true, ".bash": true, ".zsh": true, ".fish": true, ".ps1": true, ".bat": true,
	".sql": true, ".r": true, ".lua": true, ".pl": true, ".swift": true, ".clj": true, ".ex": true, ".exs": true,
	".erl": true, ".hs": true, ".ml": true, ".dart": true, ".gradle": true, ".groovy": true, ".proto": true,
	".graphql": true, ".gql": true, ".tf": true, ".hcl": true, ".lock": true, ".mk": true, ".cmake": true,
	".gitignore": true, ".gitattributes": true, ".dockerignore": true, ".editorconfig": true,
}
var textName = map[string]bool{
	"dockerfile": true, "makefile": true, "readme": true, "license": true, "licence": true,
	"notice": true, "authors": true, "changelog": true, "copying": true, ".gitignore": true,
	".gitattributes": true, ".env": true, ".dockerignore": true, ".editorconfig": true, ".npmrc": true,
}

// dropMedia are the extensions excluded in "nomedia" mode: images, audio, video,
// archives, binaries, fonts, and PDFs.
var dropMedia = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".bmp": true, ".ico": true,
	".tif": true, ".tiff": true, ".svg": true, ".heic": true, ".avif": true, ".psd": true,
	".mp3": true, ".wav": true, ".flac": true, ".aac": true, ".ogg": true, ".m4a": true, ".wma": true,
	".mp4": true, ".mov": true, ".avi": true, ".mkv": true, ".webm": true, ".m4v": true, ".flv": true, ".wmv": true,
	".zip": true, ".tar": true, ".gz": true, ".tgz": true, ".bz2": true, ".xz": true, ".7z": true, ".rar": true, ".zst": true,
	".exe": true, ".dll": true, ".so": true, ".dylib": true, ".bin": true, ".wasm": true, ".o": true, ".a": true,
	".class": true, ".jar": true, ".pyc": true, ".node": true,
	".ttf": true, ".otf": true, ".woff": true, ".woff2": true, ".eot": true, ".pdf": true,
}

// includeFile reports whether a file is captured under the given scope.
func includeFile(mode, path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch mode {
	case "all":
		return true
	case "nomedia":
		return !dropMedia[ext]
	case "text":
		return textExt[ext] || textName[strings.ToLower(filepath.Base(path))]
	}
	return false
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

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fK", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// runExport bundles the fleet, inbox, every bubble's claude conversation, and
// (per scope) the working files into a single .tgz. Usage:
//
//	bubbles export [outfile] [--files metadata|text|nomedia|all] [--dir base] [--full]
func runExport(args []string) {
	base, out, mode := defaultWorkspace(), "", ""
	full := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--full":
			full = true
		case strings.HasPrefix(a, "--files="):
			mode = strings.TrimPrefix(a, "--files=")
		case a == "--files" && i+1 < len(args):
			mode, i = args[i+1], i+1
		case a == "--dir" && i+1 < len(args):
			base, i = args[i+1], i+1
		case !strings.HasPrefix(a, "-"):
			out = a
		}
	}
	if full {
		mode = "all"
	} else if mode == "" {
		if term.IsTerminal(int(os.Stdin.Fd())) {
			mode = chooseMode()
		} else {
			mode = "metadata"
		}
	}
	if !exportModes[mode] {
		fatal(fmt.Errorf("unknown --files scope %q (want: metadata | text | nomedia | all)", mode))
	}
	if out == "" {
		out = fmt.Sprintf("bubbles-export-%s.tgz", time.Now().Format("20060102-150405"))
	}
	convos, files, bytes, err := exportFleet(base, out, mode, full)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("exported [%s]: fleet + inbox + %d conversation(s)", mode, convos)
	if mode != "metadata" {
		fmt.Printf(" + %d working file(s) (%s)", files, humanSize(bytes))
	}
	if full {
		fmt.Print(" + env + schedules + plugins + mcp + identity")
	}
	fmt.Printf(" -> %s\n", out)
	fmt.Println("(reflects the last saved state; detach the client first for a fully current export)")
	fmt.Printf("import it elsewhere with:  bubbles import %s\n", filepath.Base(out))
}

func chooseMode() string {
	fmt.Println("Export scope:")
	fmt.Println("  1) metadata only  — fleet + inbox + conversations")
	fmt.Println("  2) + text/code    — also txt, md, json, source files")
	fmt.Println("  3) except media   — everything but images/audio/video/archives/binaries")
	fmt.Println("  4) everything     — byte-for-byte, including .git and node_modules")
	fmt.Print("choose [1-4] (default 1): ")
	sc := bufio.NewScanner(os.Stdin)
	sc.Scan()
	switch strings.TrimSpace(sc.Text()) {
	case "2":
		return "text"
	case "3":
		return "nomedia"
	case "4":
		return "all"
	default:
		return "metadata"
	}
}

// exportFleet writes the blob and returns (conversations, files, bytes) bundled.
func exportFleet(base, outPath, mode string, full bool) (convos, files int, fileBytes int64, err error) {
	fm, ok := loadFleet(base)
	if !ok {
		return 0, 0, 0, fmt.Errorf("no fleet found in %s (start bubbles there first)", base)
	}
	absBase, _ := filepath.Abs(base)
	absOut, _ := filepath.Abs(outPath)
	home, _ := os.UserHomeDir()

	f, err := os.Create(outPath)
	if err != nil {
		return 0, 0, 0, err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	for _, b := range fm.Bubbles {
		if b.SessionID != "" && b.Dir != "" {
			if _, e := os.Stat(convPath(home, b.Dir, b.SessionID)); e == nil {
				convos++
			}
		}
	}
	// manifest first, then fleet/inbox, then conversations, then working files —
	// this order lets the importer stream (read metadata up front, stream the bulk).
	man := portManifest{Version: 1, Created: time.Now().UTC().Format(time.RFC3339), SourceBase: absBase, MsgPoll: messagePollMinutes(), Convos: convos, Files: mode, Full: full}
	// count working files first so the manifest is accurate
	if mode != "metadata" {
		_ = walkWorkdirs(fm, absBase, absOut, mode, func(_, _ string, fi os.FileInfo) error {
			files++
			fileBytes += fi.Size()
			return nil
		})
	}
	man.FileCount = files
	manData, _ := json.MarshalIndent(man, "", "  ")
	if err := tarWrite(tw, "manifest.json", manData); err != nil {
		return 0, 0, 0, err
	}
	if err := tarCopy(tw, "fleet.json", fleetPath(base)); err != nil {
		return 0, 0, 0, err
	}
	_ = tarCopy(tw, "inbox.json", inboxPath(base))

	// schedules
	schedPath := filepath.Join(base, ".bubbles", "schedules.json")
	_ = tarCopy(tw, "schedules.json", schedPath)

	for _, b := range fm.Bubbles {
		if b.SessionID == "" || b.Dir == "" {
			continue
		}
		_ = tarCopy(tw, "conversations/"+b.SessionID+".jsonl", convPath(home, b.Dir, b.SessionID))
	}
	if mode != "metadata" {
		if err := walkWorkdirs(fm, absBase, absOut, mode, func(rel, full string, _ os.FileInfo) error {
			return tarStreamFile(tw, "workdir/"+rel, full)
		}); err != nil {
			return 0, 0, 0, err
		}
	}

	// --full: bundle everything needed for cold-start replication
	if full {
		// env vars (relevant ones from the running process)
		envData := exportEnvVars()
		if len(envData) > 0 {
			_ = tarWrite(tw, "full/env.sh", envData)
		}

		// global MCP config
		_ = tarCopy(tw, "full/mcp.json", filepath.Join(home, ".mcp.json"))

		// per-bubble identity files (~/.claude/bubbles/)
		bubblesDir := filepath.Join(home, ".claude", "bubbles")
		_ = tarDir(tw, "full/claude-bubbles/", bubblesDir)

		// global plugins (~/.claude/plugins/ — skip large cache/repos dirs)
		pluginsDir := filepath.Join(home, ".claude", "plugins")
		_ = tarDirFiltered(tw, "full/claude-plugins/", pluginsDir, func(rel string) bool {
			return !strings.HasPrefix(rel, "cache/") &&
				!strings.HasPrefix(rel, "repos/") &&
				!strings.HasPrefix(rel, "marketplaces/")
		})

		// per-project .mcp.json files for each bubble's dir
		seen := map[string]bool{}
		for _, b := range fm.Bubbles {
			if b.Dir == "" {
				continue
			}
			mcpFile := filepath.Join(b.Dir, ".mcp.json")
			if seen[mcpFile] {
				continue
			}
			seen[mcpFile] = true
			if rel, err := filepath.Rel(absBase, mcpFile); err == nil && !strings.HasPrefix(rel, "..") {
				_ = tarCopy(tw, "full/project-mcp/"+rel, mcpFile)
			}
		}
	}

	return convos, files, fileBytes, nil
}

// exportEnvVars dumps relevant env vars as a sourceable shell script.
func exportEnvVars() []byte {
	prefixes := []string{"AWS_", "ANTHROPIC_", "CLAUDE_CODE_USE_", "BUBBLES_", "OPENAI_", "GH_TOKEN", "GITHUB_TOKEN"}
	skip := map[string]bool{
		"CLAUDE_CODE_CHILD_SESSION": true, "CLAUDE_CODE_SESSION_ID": true,
		"CLAUDE_CODE_SSE_PORT": true, "CLAUDE_CODE_ENTRYPOINT": true,
		"CLAUDE_CODE_EXECPATH": true,
	}
	var b strings.Builder
	b.WriteString("#!/bin/bash\n# Bubbles full export — env vars (source this or add to .bashrc)\n")
	for _, e := range os.Environ() {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) != 2 || skip[parts[0]] {
			continue
		}
		match := false
		for _, p := range prefixes {
			if strings.HasPrefix(parts[0], p) {
				match = true
				break
			}
		}
		if match {
			b.WriteString(fmt.Sprintf("export %s=%q\n", parts[0], parts[1]))
		}
	}
	return []byte(b.String())
}

// tarDir adds all regular files in dir under prefix in the tar.
func tarDir(tw *tar.Writer, prefix, dir string) error {
	return filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || !fi.Mode().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		return tarStreamFile(tw, prefix+filepath.ToSlash(rel), p)
	})
}

// tarDirFiltered adds files matching filter (rel path from dir) under prefix.
func tarDirFiltered(tw *tar.Writer, prefix, dir string, filter func(string) bool) error {
	return filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || !fi.Mode().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if !filter(filepath.ToSlash(rel)) {
			return nil
		}
		return tarStreamFile(tw, prefix+filepath.ToSlash(rel), p)
	})
}

// walkWorkdirs visits every in-scope working file across the fleet's bubble dirs,
// deduped (a nested bubble's dir is covered by its ancestor's walk) and confined
// to files under the base. `.bubbles` (our own metadata) and the output blob are
// skipped. rel is the path relative to base (so import can rebase it).
func walkWorkdirs(fm manifest, absBase, absOut, mode string, fn func(rel, full string, fi os.FileInfo) error) error {
	// minimal covering set of dirs under the base
	var dirs []string
	for _, b := range fm.Bubbles {
		if b.Dir == "" {
			continue
		}
		d, _ := filepath.Abs(b.Dir)
		if rel, err := filepath.Rel(absBase, d); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			continue // outside the base: files can't be rebased cleanly, skip
		}
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	var roots []string
	for _, d := range dirs {
		covered := false
		for _, r := range roots {
			if d == r || strings.HasPrefix(d, r+string(os.PathSeparator)) {
				covered = true
				break
			}
		}
		if !covered {
			roots = append(roots, d)
		}
	}
	seen := map[string]bool{}
	for _, root := range roots {
		err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
			if err != nil {
				return nil // unreadable entry: skip, don't abort the whole export
			}
			if fi.IsDir() {
				if fi.Name() == ".bubbles" { // our own store travels separately
					return filepath.SkipDir
				}
				return nil
			}
			if !fi.Mode().IsRegular() {
				return nil // skip symlinks/devices/sockets
			}
			abs, _ := filepath.Abs(p)
			if abs == absOut || seen[abs] { // don't bundle the blob into itself; dedupe overlaps
				return nil
			}
			if !includeFile(mode, p) {
				return nil
			}
			rel, err := filepath.Rel(absBase, abs)
			if err != nil {
				return nil
			}
			seen[abs] = true
			return fn(filepath.ToSlash(rel), abs, fi)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// runImport rehydrates a blob into a target workspace (default: cwd), relocating
// bubble dirs, placing each conversation in claude's store, and restoring any
// bundled working files. Usage:
//
//	bubbles import <file> [--dir target] [--force]
func runImport(args []string) {
	blob, target, force := "", defaultWorkspace(), false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dir":
			if i+1 < len(args) {
				target, i = args[i+1], i+1
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

// importFleet streams a blob into target: it reads the metadata up front, rebases
// each bubble's dir, then streams conversations into ~/.claude/projects and
// working files into the target — without loading the whole (possibly multi-GB)
// blob into memory.
func importFleet(blob, target string, force bool) (string, error) {
	if _, ok := loadFleet(target); ok && !force {
		return "", fmt.Errorf(".bubbles/fleet.json already exists in %s — use --force to overwrite", target)
	}
	f, err := os.Open(blob)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("%s is not a gzip blob: %w", blob, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	absTarget, _ := filepath.Abs(target)
	home, _ := os.UserHomeDir()

	var man portManifest
	var fm manifest
	sidToDir := map[string]string{} // session id -> rebased dir (for conversation placement)
	placed, restored, fullFiles := 0, 0, 0
	var fleetSeen bool
	var envScript string

	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		switch {
		case h.Name == "manifest.json":
			data, _ := io.ReadAll(tr)
			_ = json.Unmarshal(data, &man)
		case h.Name == "fleet.json":
			data, err := io.ReadAll(tr)
			if err != nil {
				return "", err
			}
			if err := json.Unmarshal(data, &fm); err != nil {
				return "", fmt.Errorf("blob fleet.json is unreadable: %w", err)
			}
			for i := range fm.Bubbles {
				b := &fm.Bubbles[i]
				nd := rebaseDir(b.Dir, man.SourceBase, absTarget)
				if b.SessionID != "" {
					sidToDir[b.SessionID] = nd
				}
				b.Dir = nd
				_ = os.MkdirAll(nd, 0o755)
			}
			fleetSeen = true
		case h.Name == "inbox.json":
			if err := streamToFile(inboxPath(target), 0o644, tr); err != nil {
				return "", err
			}
		case h.Name == "schedules.json":
			dst := filepath.Join(target, ".bubbles", "schedules.json")
			_ = streamToFile(dst, 0o644, tr)
		case strings.HasPrefix(h.Name, "conversations/"):
			sid := strings.TrimSuffix(strings.TrimPrefix(h.Name, "conversations/"), ".jsonl")
			dir := sidToDir[sid]
			if dir == "" {
				continue
			}
			dst := filepath.Join(home, ".claude", "projects", claudeSlug(dir), sid+".jsonl")
			if streamToFile(dst, 0o644, tr) == nil {
				placed++
			}
		case strings.HasPrefix(h.Name, "workdir/"):
			rel := strings.TrimPrefix(h.Name, "workdir/")
			dst, ok := safeJoin(absTarget, rel)
			if !ok {
				continue
			}
			if streamToFile(dst, os.FileMode(h.Mode).Perm(), tr) == nil {
				restored++
			}

		// --full extras
		case h.Name == "full/env.sh":
			data, _ := io.ReadAll(tr)
			envScript = string(data)
			dst := filepath.Join(target, ".bubbles", "env.sh")
			_ = os.WriteFile(dst, data, 0o600)
			fullFiles++
		case h.Name == "full/mcp.json":
			dst := filepath.Join(home, ".mcp.json")
			_ = streamToFile(dst, 0o644, tr)
			fullFiles++
		case strings.HasPrefix(h.Name, "full/claude-bubbles/"):
			rel := strings.TrimPrefix(h.Name, "full/claude-bubbles/")
			dst := filepath.Join(home, ".claude", "bubbles", rel)
			_ = streamToFile(dst, 0o644, tr)
			fullFiles++
		case strings.HasPrefix(h.Name, "full/claude-plugins/"):
			rel := strings.TrimPrefix(h.Name, "full/claude-plugins/")
			dst := filepath.Join(home, ".claude", "plugins", rel)
			_ = streamToFile(dst, 0o644, tr)
			fullFiles++
		case strings.HasPrefix(h.Name, "full/project-mcp/"):
			rel := strings.TrimPrefix(h.Name, "full/project-mcp/")
			dst, ok := safeJoin(absTarget, rel)
			if ok {
				_ = streamToFile(dst, 0o644, tr)
				fullFiles++
			}
		}
	}
	if !fleetSeen {
		return "", fmt.Errorf("blob has no fleet.json — not a bubbles export")
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

	var s strings.Builder
	fmt.Fprintf(&s, "imported %d bubble(s) into %s\n", len(fm.Bubbles), absTarget)
	fmt.Fprintf(&s, "restored %d conversation(s)\n", placed)
	if man.Files != "" && man.Files != "metadata" {
		fmt.Fprintf(&s, "restored %d working file(s) [%s scope]\n", restored, man.Files)
	}
	if man.Full {
		fmt.Fprintf(&s, "restored %d full-export files (env, plugins, mcp, identity)\n", fullFiles)
		if envScript != "" {
			fmt.Fprintf(&s, "\n*** ENV VARS saved to %s/.bubbles/env.sh ***\n", absTarget)
			fmt.Fprintf(&s, "    source %s/.bubbles/env.sh   # then restart bubbles\n", absTarget)
		}
	}
	if man.MsgPoll > 0 && man.MsgPoll != 10 {
		fmt.Fprintf(&s, "note: source ran with --message_polling %d\n", man.MsgPoll)
	}
	fmt.Fprintf(&s, "run `bubbles` in %s to bring the fleet up.\n", absTarget)
	return s.String(), nil
}

// safeJoin joins rel onto base, refusing paths that escape base (a hostile blob
// with ../ entries).
func safeJoin(base, rel string) (string, bool) {
	p := filepath.Join(base, filepath.FromSlash(rel))
	if p == base || strings.HasPrefix(p, base+string(os.PathSeparator)) {
		return p, true
	}
	return "", false
}

func streamToFile(dstPath string, mode os.FileMode, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return err
	}
	if mode == 0 {
		mode = 0o644
	}
	f, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, r)
	return err
}

func tarWrite(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// tarCopy adds a small file to the tar by reading it whole (used for the
// manifest/fleet/inbox metadata).
func tarCopy(tw *tar.Writer, name, srcPath string) error {
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return err
	}
	return tarWrite(tw, name, data)
}

// tarStreamFile adds a file to the tar by streaming it (used for conversations
// and working files, which can be large), preserving its mode.
func tarStreamFile(tw *tar.Writer, name, srcPath string) error {
	fi, err := os.Stat(srcPath)
	if err != nil {
		return err
	}
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: int64(fi.Mode().Perm()), Size: fi.Size(), Typeflag: tar.TypeReg, ModTime: fi.ModTime()}); err != nil {
		return err
	}
	_, err = io.Copy(tw, src)
	return err
}
