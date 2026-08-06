package hookio

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matthewstingel/rune-claude/internal/ledger"
)

// payload mimics the JSON Claude Code delivers on hook stdin.
func payload(event, session, cwd, tool, inputJSON string) string {
	return fmt.Sprintf(`{
		"session_id": %q,
		"transcript_path": "/tmp/transcript.jsonl",
		"cwd": %q,
		"hook_event_name": %q,
		"tool_name": %q,
		"tool_input": %s
	}`, session, cwd, event, tool, inputJSON)
}

func TestParseAndTargetPath(t *testing.T) {
	cases := []struct {
		name  string
		tool  string
		input string
		want  string
	}{
		{"edit", "Edit", `{"file_path": "/p/a.go", "old_string": "x", "new_string": "y"}`, "/p/a.go"},
		{"write", "Write", `{"file_path": "/p/b.go", "content": "hi"}`, "/p/b.go"},
		{"multiedit", "MultiEdit", `{"file_path": "/p/c.go", "edits": [{"old_string":"x","new_string":"y"}]}`, "/p/c.go"},
		{"notebook", "NotebookEdit", `{"notebook_path": "/p/n.ipynb", "new_source": "z"}`, "/p/n.ipynb"},
		{"relative resolves against cwd", "Edit", `{"file_path": "sub/d.go"}`, "/proj/sub/d.go"},
		{"no path", "Edit", `{}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Parse(strings.NewReader(payload("PostToolUse", "s1", "/proj", tc.tool, tc.input)))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := p.TargetPath(); got != tc.want {
				t.Fatalf("TargetPath = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	if _, err := Parse(strings.NewReader("not json at all")); err == nil {
		t.Fatal("Parse accepted garbage")
	}
}

func TestProcessPrePostRecordsChange(t *testing.T) {
	st, err := ledger.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "main.go")
	if err := os.WriteFile(target, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	input := fmt.Sprintf(`{"file_path": %q, "old_string": "a", "new_string": "b"}`, target)

	pre, _ := Parse(strings.NewReader(payload("PreToolUse", "s1", dir, "Edit", input)))
	if err := Process(st, pre, time.Now()); err != nil {
		t.Fatalf("Process pre: %v", err)
	}
	// Pre must not write ledger events (the edit could still be rejected).
	if evs, _ := st.Events(); len(evs) != 0 {
		t.Fatalf("pre hook wrote %d events, want 0", len(evs))
	}

	post, _ := Parse(strings.NewReader(payload("PostToolUse", "s1", dir, "Edit", input)))
	if err := Process(st, post, time.Now()); err != nil {
		t.Fatalf("Process post: %v", err)
	}

	evs, err := st.Events()
	if err != nil || len(evs) != 1 {
		t.Fatalf("events = %v err = %v, want exactly 1", evs, err)
	}
	ev := evs[0]
	if ev.Kind != ledger.KindChange || ev.Path != target || ev.Action != ledger.ActionModified || ev.Tool != "Edit" {
		t.Fatalf("unexpected event %+v", ev)
	}
}

func TestProcessNewFileMarkedCreated(t *testing.T) {
	st, _ := ledger.Open(t.TempDir())
	dir := t.TempDir()
	target := filepath.Join(dir, "brand_new.go")
	input := fmt.Sprintf(`{"file_path": %q, "content": "package x\n"}`, target)

	pre, _ := Parse(strings.NewReader(payload("PreToolUse", "s1", dir, "Write", input)))
	if err := Process(st, pre, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Claude writes the file between pre and post.
	if err := os.WriteFile(target, []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	post, _ := Parse(strings.NewReader(payload("PostToolUse", "s1", dir, "Write", input)))
	if err := Process(st, post, time.Now()); err != nil {
		t.Fatal(err)
	}

	evs, _ := st.Events()
	if len(evs) != 1 || evs[0].Action != ledger.ActionCreated {
		t.Fatalf("events = %+v, want one created action", evs)
	}
}

func TestProcessIgnoresNonFileTools(t *testing.T) {
	st, _ := ledger.Open(t.TempDir())
	p, _ := Parse(strings.NewReader(payload("PostToolUse", "s1", "/proj", "Bash", `{"command": "ls"}`)))
	if err := Process(st, p, time.Now()); err != nil {
		t.Fatal(err)
	}
	if evs, _ := st.Events(); len(evs) != 0 {
		t.Fatalf("Bash produced events: %+v", evs)
	}
}

func TestProcessSessionLifecycle(t *testing.T) {
	st, _ := ledger.Open(t.TempDir())
	start, _ := Parse(strings.NewReader(`{"session_id":"s1","cwd":"/proj","hook_event_name":"SessionStart","source":"startup"}`))
	end, _ := Parse(strings.NewReader(`{"session_id":"s1","cwd":"/proj","hook_event_name":"SessionEnd","reason":"exit"}`))
	if err := Process(st, start, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := Process(st, end, time.Now()); err != nil {
		t.Fatal(err)
	}
	evs, _ := st.Events()
	if len(evs) != 2 || evs[0].Kind != ledger.KindSessionStart || evs[1].Kind != ledger.KindSessionEnd {
		t.Fatalf("events = %+v, want start then end", evs)
	}
}

func TestProcessUnknownEventIsNoop(t *testing.T) {
	st, _ := ledger.Open(t.TempDir())
	p, _ := Parse(strings.NewReader(`{"session_id":"s1","hook_event_name":"Notification","message":"hi"}`))
	if err := Process(st, p, time.Now()); err != nil {
		t.Fatal(err)
	}
	if evs, _ := st.Events(); len(evs) != 0 {
		t.Fatalf("unknown event produced ledger entries: %+v", evs)
	}
}
