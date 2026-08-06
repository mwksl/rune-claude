// Package transcript reads Claude Code session transcripts — the JSONL
// files under ~/.claude/projects/<project>/<session>.jsonl — and converts
// them into feed entries: user prompts, assistant text, thinking markers,
// and tool calls. Parsing is forgiving; unknown records yield nothing.
package transcript

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Entry is one item in the conversation feed.
type Entry struct {
	Time time.Time
	Role string // "user" | "assistant"
	Kind string // "text" | "thinking" | "tool"
	Text string // prompt/response/thinking text; tool detail for Kind "tool"
	Tool string // tool name when Kind == "tool"
}

// record is the subset of a transcript line this package consumes.
type record struct {
	Type        string `json:"type"`
	Timestamp   string `json:"timestamp"`
	IsSidechain bool   `json:"isSidechain"`
	Message     struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// block is one content block of a user or assistant message.
type block struct {
	Type     string         `json:"type"`
	Text     string         `json:"text"`
	Thinking string         `json:"thinking"`
	Name     string         `json:"name"`
	Input    map[string]any `json:"input"`
}

// ParseLine converts one transcript line into feed entries. Most lines
// (tool results, bookkeeping records, sidechains) produce none.
func ParseLine(line []byte) []Entry {
	line = []byte(strings.TrimSpace(string(line)))
	if len(line) == 0 {
		return nil
	}
	var rec record
	if err := json.Unmarshal(line, &rec); err != nil {
		return nil
	}
	if rec.IsSidechain || (rec.Type != "user" && rec.Type != "assistant") {
		return nil
	}
	ts, _ := time.Parse(time.RFC3339, rec.Timestamp)

	// content is either a plain string (user prompts, local-command noise)
	// or an array of blocks.
	var asString string
	if err := json.Unmarshal(rec.Message.Content, &asString); err == nil {
		if rec.Type == "user" && isRealPrompt(asString) {
			return []Entry{{Time: ts, Role: "user", Kind: "text", Text: asString}}
		}
		return nil
	}

	var blocks []block
	if err := json.Unmarshal(rec.Message.Content, &blocks); err != nil {
		return nil
	}

	var out []Entry
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if rec.Type == "user" && !isRealPrompt(b.Text) {
				continue
			}
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			out = append(out, Entry{Time: ts, Role: rec.Type, Kind: "text", Text: b.Text})
		case "thinking":
			if rec.Type != "assistant" {
				continue
			}
			// Claude Code usually persists only the signature, not the
			// thinking text itself — an empty Text still marks activity.
			out = append(out, Entry{Time: ts, Role: "assistant", Kind: "thinking", Text: strings.TrimSpace(b.Thinking)})
		case "tool_use":
			out = append(out, Entry{
				Time: ts, Role: "assistant", Kind: "tool",
				Tool: b.Name, Text: summarizeInput(b.Input),
			})
		case "image":
			if rec.Type == "user" {
				out = append(out, Entry{Time: ts, Role: "user", Kind: "text", Text: "[image]"})
			}
		}
	}
	return out
}

// isRealPrompt filters out Claude Code's bookkeeping user records
// (local-command output, caveats, command echoes).
func isRealPrompt(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return false
	}
	for _, p := range []string{"<local-command", "<command-name>", "<command-message>", "Caveat:", "<system-reminder>", "<task-notification>"} {
		if strings.HasPrefix(t, p) {
			return false
		}
	}
	return true
}

// summarizeInput picks the most descriptive field of a tool call's input.
func summarizeInput(input map[string]any) string {
	for _, key := range []string{"file_path", "notebook_path", "command", "pattern", "query", "url", "description", "prompt", "skill"} {
		if v, ok := input[key].(string); ok && v != "" {
			return truncate(strings.ReplaceAll(v, "\n", " "), 120)
		}
	}
	return ""
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// Latest returns the transcript with the newest modification time among
// candidates (paths recorded by the hook) and, every call, the newest
// *.jsonl under the Claude projects directory as a fallback so the feed
// works even for sessions without hooks installed.
func Latest(candidates []string) (string, time.Time) {
	var best string
	var bestT time.Time
	consider := func(path string) {
		fi, err := os.Stat(path)
		if err != nil || fi.IsDir() {
			return
		}
		if fi.ModTime().After(bestT) {
			best, bestT = path, fi.ModTime()
		}
	}
	for _, c := range candidates {
		consider(c)
	}
	for _, p := range scanProjects() {
		consider(p)
	}
	return best, bestT
}

// scanProjects lists transcript files under the Claude projects dir,
// cheaply: only project dirs modified recently are read.
func scanProjects() []string {
	root := projectsDir()
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		info, err := d.Info()
		if err != nil || time.Since(info.ModTime()) > 24*time.Hour {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, d.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if strings.HasSuffix(f.Name(), ".jsonl") {
				out = append(out, filepath.Join(root, d.Name(), f.Name()))
			}
		}
	}
	return out
}

func projectsDir() string {
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return filepath.Join(v, "projects")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// SessionLabel derives a short display label from a transcript path
// (the file stem is the session id).
func SessionLabel(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if len(base) > 8 {
		base = base[:8]
	}
	if base == "" {
		return "?"
	}
	return base
}

// String renders an entry for logs and tests.
func (e Entry) String() string {
	switch e.Kind {
	case "tool":
		return fmt.Sprintf("[%s tool %s] %s", e.Role, e.Tool, e.Text)
	default:
		return fmt.Sprintf("[%s %s] %s", e.Role, e.Kind, e.Text)
	}
}
