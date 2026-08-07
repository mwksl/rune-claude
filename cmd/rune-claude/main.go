// Command rune-claude records the files Claude Code changes so the Rune IDE
// extension can display them.
//
//	rune-claude hook            consume one hook payload from stdin (wired by setup)
//	rune-claude setup [--user]  install Claude Code hooks pointing at this binary
//	rune-claude status          list recorded sessions and changed files
//	rune-claude diff <file>     unified diff of a file against its pre-edit baseline
//	rune-claude clear           forget all recorded changes and snapshots
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mwksl/rune-claude/internal/hookio"
	"github.com/mwksl/rune-claude/internal/ledger"
	"github.com/mwksl/rune-claude/internal/transcript"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "hook":
		runHook() // always exits 0
	case "setup":
		exitOn(runSetup(os.Args[2:]))
	case "status":
		exitOn(runStatus())
	case "diff":
		exitOn(runDiff(os.Args[2:]))
	case "feed":
		exitOn(runFeed(os.Args[2:]))
	case "clear":
		exitOn(runClear(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `rune-claude — see the files Claude Code is changing, from the Rune IDE

usage:
  rune-claude hook            consume one Claude Code hook payload from stdin
  rune-claude setup [--user]  install hooks into .claude/settings.local.json
                              (--user targets ~/.claude/settings.json instead)
  rune-claude status          list recorded sessions and changed files
  rune-claude diff <file>     unified diff of a file against its baseline
  rune-claude feed [--follow] print the latest session's conversation feed
  rune-claude clear           forget all recorded changes and snapshots
        --session <id>        forget one session (id or unique prefix)
        --ended               forget only sessions that have ended
`)
}

func exitOn(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "rune-claude:", err)
		os.Exit(1)
	}
}

// runHook processes one payload and always exits 0: a broken observer must
// never block or fail Claude Code's tool loop. Errors go to hook.log.
func runHook() {
	err := func() error {
		st, err := ledger.Open(ledger.DefaultDir())
		if err != nil {
			return err
		}
		p, err := hookio.Parse(os.Stdin)
		if err != nil {
			return err
		}
		return hookio.Process(st, p, time.Now())
	}()
	if err != nil {
		logHookError(err)
	}
	os.Exit(0)
}

func logHookError(err error) {
	f, ferr := os.OpenFile(
		filepath.Join(ledger.DefaultDir(), "hook.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600,
	)
	if ferr != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %v\n", time.Now().Format(time.RFC3339), err)
}

// hookEvents lists the Claude Code hook events setup installs, with the
// matcher each should carry (empty = match everything).
var hookEvents = []struct{ event, matcher string }{
	{"PreToolUse", "Edit|Write|MultiEdit|NotebookEdit"},
	{"PostToolUse", "Edit|Write|MultiEdit|NotebookEdit"},
	{"SessionStart", ""},
	{"SessionEnd", ""},
}

func runSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	user := fs.Bool("user", false, "install into ~/.claude/settings.json instead of the project")
	if err := fs.Parse(args); err != nil {
		return err
	}

	target := filepath.Join(".claude", "settings.local.json")
	if *user {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		target = filepath.Join(home, ".claude", "settings.json")
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate own binary: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("resolve own binary: %w", err)
	}
	command := fmt.Sprintf("%q hook", exe)

	changed, err := installHooks(target, command)
	if err != nil {
		return err
	}
	if changed {
		fmt.Printf("installed rune-claude hooks in %s\n", target)
		fmt.Println("restart running Claude Code sessions (or /hooks) to pick them up")
	} else {
		fmt.Printf("rune-claude hooks already present in %s\n", target)
	}
	return nil
}

// installHooks merges our hook entries into the settings file, preserving
// everything already there. Idempotent: entries whose command mentions
// "rune-claude" are treated as ours and refreshed in place.
func installHooks(path, command string) (changed bool, err error) {
	root := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &root); err != nil {
			return false, fmt.Errorf("%s exists but is not valid JSON: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return false, err
	}

	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}

	for _, he := range hookEvents {
		entries, _ := hooks[he.event].([]any)
		entry := map[string]any{
			"hooks": []any{map[string]any{"type": "command", "command": command}},
		}
		if he.matcher != "" {
			entry["matcher"] = he.matcher
		}

		replaced := false
		for i, e := range entries {
			if entryMentionsRuneClaude(e) {
				if !sameEntry(entries[i], entry) {
					entries[i] = entry
					changed = true
				}
				replaced = true
				break
			}
		}
		if !replaced {
			entries = append(entries, entry)
			changed = true
		}
		hooks[he.event] = entries
	}
	root["hooks"] = hooks

	if !changed {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return false, err
	}
	return true, os.WriteFile(path, append(b, '\n'), 0o644)
}

func entryMentionsRuneClaude(e any) bool {
	entry, ok := e.(map[string]any)
	if !ok {
		return false
	}
	inner, _ := entry["hooks"].([]any)
	for _, h := range inner {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if cmd, _ := hm["command"].(string); strings.Contains(cmd, "rune-claude") {
			return true
		}
	}
	return false
}

func sameEntry(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ab) == string(bb)
}

func runStatus() error {
	st, err := ledger.Open(ledger.DefaultDir())
	if err != nil {
		return err
	}
	evs, err := st.Events()
	if err != nil {
		return err
	}
	if len(evs) == 0 {
		fmt.Println("no recorded Claude Code activity")
		fmt.Println("run `rune-claude setup` in a project, then let Claude edit something")
		return nil
	}

	type fileState struct {
		action ledger.Action
		count  int
		last   time.Time
	}
	type session struct {
		id      string
		cwd     string
		started time.Time
		ended   bool
		last    time.Time
		files   map[string]*fileState
	}

	sessions := map[string]*session{}
	get := func(id string) *session {
		s, ok := sessions[id]
		if !ok {
			s = &session{id: id, files: map[string]*fileState{}}
			sessions[id] = s
		}
		return s
	}
	for _, ev := range evs {
		s := get(ev.Session)
		if ev.CWD != "" && s.cwd == "" {
			s.cwd = ev.CWD
		}
		if ev.Time.After(s.last) {
			s.last = ev.Time
		}
		switch ev.Kind {
		case ledger.KindSessionStart:
			s.started = ev.Time
		case ledger.KindSessionEnd:
			s.ended = true
		case ledger.KindChange:
			fs := s.files[ev.Path]
			if fs == nil {
				fs = &fileState{action: ev.Action}
				s.files[ev.Path] = fs
			}
			if ev.Action == ledger.ActionCreated {
				fs.action = ledger.ActionCreated
			}
			fs.count++
			fs.last = ev.Time
		}
	}

	ordered := make([]*session, 0, len(sessions))
	for _, s := range sessions {
		ordered = append(ordered, s)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].last.After(ordered[j].last) })

	for _, s := range ordered {
		state := "active"
		if s.ended {
			state = "ended"
		}
		fmt.Printf("session %s  %s  %s  (%d files)\n", shortID(s.id), s.cwd, state, len(s.files))
		paths := make([]string, 0, len(s.files))
		for p := range s.files {
			paths = append(paths, p)
		}
		sort.Slice(paths, func(i, j int) bool { return s.files[paths[i]].last.After(s.files[paths[j]].last) })
		for _, p := range paths {
			fs := s.files[p]
			marker := "M"
			if fs.action == ledger.ActionCreated {
				marker = "A"
			}
			fmt.Printf("  %s  %-60s  %s  ×%d\n", marker, displayPath(p, s.cwd), fs.last.Format("15:04:05"), fs.count)
		}
	}
	return nil
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	if id == "" {
		return "(none)"
	}
	return id
}

func displayPath(path, cwd string) string {
	if cwd == "" {
		return path
	}
	if rel, err := filepath.Rel(cwd, path); err == nil && !filepath.IsAbs(rel) && rel != "" && !isDotDot(rel) {
		return rel
	}
	return path
}

func isDotDot(rel string) bool {
	return rel == ".." || (len(rel) > 2 && rel[:3] == "../")
}

func runDiff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	session := fs.String("session", "", "diff against this session's baseline (default: newest)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: rune-claude diff [--session <id>] <file>")
	}
	path, err := filepath.Abs(fs.Arg(0))
	if err != nil {
		return err
	}

	st, err := ledger.Open(ledger.DefaultDir())
	if err != nil {
		return err
	}
	patch, err := st.DiffAgainstBaseline(*session, path)
	if err != nil {
		return err
	}
	if patch == "" {
		fmt.Println("no differences against baseline")
		return nil
	}
	fmt.Print(patch)
	return nil
}

// runFeed prints the most recently active session's conversation feed:
// the same data the Rune feed window renders, for terminals and debugging.
func runFeed(args []string) error {
	fs := flag.NewFlagSet("feed", flag.ExitOnError)
	follow := fs.Bool("follow", false, "keep tailing and print new entries as they arrive")
	tailN := fs.Int("n", 40, "number of trailing entries to print initially")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := ledger.Open(ledger.DefaultDir())
	if err != nil {
		return err
	}
	var candidates []string
	if evs, err := st.Events(); err == nil {
		seen := map[string]bool{}
		for i := len(evs) - 1; i >= 0; i-- {
			if t := evs[i].Transcript; t != "" && !seen[t] {
				seen[t] = true
				candidates = append(candidates, t)
			}
		}
	}
	path, _ := transcript.Latest(candidates)
	if path == "" {
		return fmt.Errorf("no Claude Code transcript found")
	}
	fmt.Printf("── session %s · %s\n", transcript.SessionLabel(path), path)

	tail := transcript.NewTail(path)
	entries, err := tail.Next()
	if err != nil {
		return err
	}
	if len(entries) > *tailN {
		entries = entries[len(entries)-*tailN:]
	}
	printEntries(entries)

	if !*follow {
		return nil
	}
	for {
		time.Sleep(500 * time.Millisecond)
		entries, err := tail.Next()
		if err != nil {
			return err
		}
		printEntries(entries)
	}
}

func printEntries(entries []transcript.Entry) {
	for _, e := range entries {
		ts := e.Time.Local().Format("15:04:05")
		switch e.Kind {
		case "tool":
			detail := e.Text
			if detail != "" {
				detail = " · " + detail
			}
			fmt.Printf("%s  ⚒ %s%s\n", ts, e.Tool, detail)
		case "thinking":
			if e.Text == "" {
				fmt.Printf("%s  ✻ thinking…\n", ts)
			} else {
				fmt.Printf("%s  ✻ %s\n", ts, e.Text)
			}
		default:
			marker := "•"
			if e.Role == "user" {
				marker = "❯"
			}
			fmt.Printf("%s  %s %s\n", ts, marker, e.Text)
		}
	}
}

func runClear(args []string) error {
	fs := flag.NewFlagSet("clear", flag.ExitOnError)
	session := fs.String("session", "", "forget one session (id or unique prefix)")
	ended := fs.Bool("ended", false, "forget only sessions that have ended")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *session != "" && *ended {
		return fmt.Errorf("--session and --ended are mutually exclusive")
	}

	st, err := ledger.Open(ledger.DefaultDir())
	if err != nil {
		return err
	}

	switch {
	case *session != "":
		id, err := resolveSession(st, *session)
		if err != nil {
			return err
		}
		if err := st.RemoveSessions(id); err != nil {
			return err
		}
		fmt.Printf("forgot session %s\n", shortID(id))
	case *ended:
		evs, err := st.Events()
		if err != nil {
			return err
		}
		ids := ledger.EndedSessions(evs)
		if len(ids) == 0 {
			fmt.Println("no ended sessions to forget")
			return nil
		}
		if err := st.RemoveSessions(ids...); err != nil {
			return err
		}
		fmt.Printf("forgot %d ended session(s)\n", len(ids))
	default:
		if err := st.Clear(); err != nil {
			return err
		}
		fmt.Println("cleared recorded changes and snapshots")
	}
	return nil
}

// resolveSession expands a session id or unique prefix against the ledger.
func resolveSession(st *ledger.Store, query string) (string, error) {
	evs, err := st.Events()
	if err != nil {
		return "", err
	}
	seen := map[string]bool{}
	var matches []string
	for _, ev := range evs {
		if seen[ev.Session] {
			continue
		}
		seen[ev.Session] = true
		if ev.Session == query {
			return query, nil
		}
		if strings.HasPrefix(ev.Session, query) {
			matches = append(matches, ev.Session)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no session matches %q (see rune-claude status)", query)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%q is ambiguous: matches %d sessions", query, len(matches))
	}
}
