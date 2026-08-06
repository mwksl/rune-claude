package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Fixture lines mirror the real transcript shapes observed in Claude Code
// v2.1.x: one record per line, assistant content blocks of type
// text/thinking/tool_use, user content as string or block array.
const (
	lineAssistantText = `{"type":"assistant","timestamp":"2026-08-06T21:47:08.306Z","uuid":"u1","message":{"role":"assistant","content":[{"type":"text","text":"I'll research this along two tracks."}]}}`
	lineThinkingEmpty = `{"type":"assistant","timestamp":"2026-08-06T21:47:06.951Z","uuid":"u2","message":{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"abc"}]}}`
	lineThinkingFull  = `{"type":"assistant","timestamp":"2026-08-06T21:47:06.951Z","uuid":"u3","message":{"role":"assistant","content":[{"type":"thinking","thinking":"The user wants a panel.","signature":"abc"}]}}`
	lineToolUse       = `{"type":"assistant","timestamp":"2026-08-06T21:47:08.411Z","uuid":"u4","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Edit","input":{"file_path":"/p/main.go","old_string":"a","new_string":"b"}}]}}`
	lineUserPrompt    = `{"type":"user","timestamp":"2026-08-06T21:46:00.000Z","uuid":"u5","message":{"role":"user","content":[{"type":"text","text":"wow! it works"}]}}`
	lineUserString    = `{"type":"user","timestamp":"2026-08-06T21:46:00.000Z","uuid":"u6","message":{"role":"user","content":"plain typed prompt"}}`
	lineUserCaveat    = `{"type":"user","timestamp":"2026-08-06T21:46:00.000Z","uuid":"u7","message":{"role":"user","content":"<local-command-caveat>Caveat: ...</local-command-caveat>"}}`
	lineToolResult    = `{"type":"user","timestamp":"2026-08-06T21:47:10.000Z","uuid":"u8","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`
	lineSidechain     = `{"type":"assistant","isSidechain":true,"timestamp":"2026-08-06T21:47:08.306Z","uuid":"u9","message":{"role":"assistant","content":[{"type":"text","text":"subagent chatter"}]}}`
	lineBookkeeping   = `{"type":"ai-title","title":"whatever"}`
)

func TestParseLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want []string // Entry.String() forms; empty means no entries
	}{
		{"assistant text", lineAssistantText, []string{"[assistant text] I'll research this along two tracks."}},
		{"thinking without persisted text", lineThinkingEmpty, []string{"[assistant thinking] "}},
		{"thinking with text", lineThinkingFull, []string{"[assistant thinking] The user wants a panel."}},
		{"tool use", lineToolUse, []string{"[assistant tool Edit] /p/main.go"}},
		{"user prompt blocks", lineUserPrompt, []string{"[user text] wow! it works"}},
		{"user prompt string", lineUserString, []string{"[user text] plain typed prompt"}},
		{"local command caveat skipped", lineUserCaveat, nil},
		{"tool result skipped", lineToolResult, nil},
		{"sidechain skipped", lineSidechain, nil},
		{"bookkeeping skipped", lineBookkeeping, nil},
		{"garbage skipped", "{oops", nil},
		{"empty skipped", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseLine([]byte(tc.line))
			if len(got) != len(tc.want) {
				t.Fatalf("got %d entries (%v), want %d", len(got), got, len(tc.want))
			}
			for i := range got {
				if got[i].String() != tc.want[i] {
					t.Errorf("entry %d = %q, want %q", i, got[i].String(), tc.want[i])
				}
			}
		})
	}
}

func TestTailIncremental(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	tail := NewTail(path)

	// Missing file: no entries, no error.
	if evs, err := tail.Next(); err != nil || len(evs) != 0 {
		t.Fatalf("missing file: evs=%v err=%v", evs, err)
	}

	write := func(s string) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		fmt.Fprint(f, s)
	}

	write(lineAssistantText + "\n")
	evs, err := tail.Next()
	if err != nil || len(evs) != 1 {
		t.Fatalf("first read: evs=%v err=%v", evs, err)
	}

	// A partial line must not be consumed until its newline arrives.
	write(lineToolUse[:40])
	if evs, _ := tail.Next(); len(evs) != 0 {
		t.Fatalf("partial line consumed early: %v", evs)
	}
	write(lineToolUse[40:] + "\n")
	evs, err = tail.Next()
	if err != nil || len(evs) != 1 || evs[0].Tool != "Edit" {
		t.Fatalf("completed line: evs=%v err=%v", evs, err)
	}

	// Nothing new → nothing returned.
	if evs, _ := tail.Next(); len(evs) != 0 {
		t.Fatalf("idle read returned %v", evs)
	}

	// Truncation restarts from the top.
	if err := os.WriteFile(path, []byte(lineUserString+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	evs, err = tail.Next()
	if err != nil || len(evs) != 1 || evs[0].Role != "user" {
		t.Fatalf("after truncation: evs=%v err=%v", evs, err)
	}
}

func TestSummarizeInputPriority(t *testing.T) {
	got := summarizeInput(map[string]any{"command": "go test ./...", "description": "Run tests"})
	if got != "go test ./..." {
		t.Fatalf("summarizeInput = %q, want command to win", got)
	}
	if got := summarizeInput(map[string]any{}); got != "" {
		t.Fatalf("empty input = %q, want empty", got)
	}
}
