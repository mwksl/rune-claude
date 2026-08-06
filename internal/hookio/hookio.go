// Package hookio parses Claude Code hook payloads (JSON on stdin) and
// applies them to the ledger. It is deliberately forgiving: hooks run inside
// Claude Code's tool loop, so unknown events, missing fields, and malformed
// input must degrade to no-ops, never failures.
package hookio

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/mwksl/rune-claude/internal/ledger"
)

// Payload is the subset of a Claude Code hook event this tool consumes.
// See https://code.claude.com/docs/en/hooks for the full schema.
type Payload struct {
	HookEventName  string          `json:"hook_event_name"`
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	CWD            string          `json:"cwd"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	Timestamp      string          `json:"timestamp"`
}

type toolInput struct {
	FilePath     string `json:"file_path"`
	NotebookPath string `json:"notebook_path"`
}

// Parse decodes one hook payload from r.
func Parse(r io.Reader) (Payload, error) {
	var p Payload
	dec := json.NewDecoder(r)
	if err := dec.Decode(&p); err != nil {
		return Payload{}, fmt.Errorf("decode hook payload: %w", err)
	}
	return p, nil
}

// fileTools are the Claude Code tools that modify files on disk.
var fileTools = map[string]bool{
	"Edit":         true,
	"Write":        true,
	"MultiEdit":    true,
	"NotebookEdit": true,
}

// IsFileTool reports whether name is a file-modifying Claude Code tool.
func IsFileTool(name string) bool { return fileTools[name] }

// TargetPath extracts the absolute path of the file the tool call touches,
// or "" when the payload has none. Relative paths resolve against the
// payload's cwd.
func (p Payload) TargetPath() string {
	if len(p.ToolInput) == 0 {
		return ""
	}
	var in toolInput
	if err := json.Unmarshal(p.ToolInput, &in); err != nil {
		return ""
	}
	path := in.FilePath
	if path == "" {
		path = in.NotebookPath
	}
	if path == "" {
		return ""
	}
	if !filepath.IsAbs(path) && p.CWD != "" {
		path = filepath.Join(p.CWD, path)
	}
	return filepath.Clean(path)
}

// Process applies one hook payload to the store:
//
//   - PreToolUse on a file tool: snapshot the file's baseline (first touch
//     per session wins). No ledger event — the edit may still be rejected.
//   - PostToolUse on a file tool: append a change event; the action is
//     "created" when the baseline says the file was absent.
//   - SessionStart / SessionEnd: append lifecycle events.
//
// Anything else is ignored.
func Process(st *ledger.Store, p Payload, now time.Time) error {
	base := ledger.Event{Time: now, Session: p.SessionID, CWD: p.CWD, Transcript: p.TranscriptPath}

	switch p.HookEventName {
	case "PreToolUse":
		path := p.TargetPath()
		if !IsFileTool(p.ToolName) || path == "" {
			return nil
		}
		_, err := st.EnsureSnapshot(p.SessionID, path)
		return err

	case "PostToolUse":
		path := p.TargetPath()
		if !IsFileTool(p.ToolName) || path == "" {
			return nil
		}
		base.Kind = ledger.KindChange
		base.Tool = p.ToolName
		base.Path = path
		base.Action = ledger.ActionModified
		if meta, _, ok, err := st.SnapshotFor(p.SessionID, path); err == nil && ok && meta.Absent {
			base.Action = ledger.ActionCreated
		}
		return st.Append(base)

	case "SessionStart":
		base.Kind = ledger.KindSessionStart
		return st.Append(base)

	case "SessionEnd":
		base.Kind = ledger.KindSessionEnd
		return st.Append(base)

	default:
		return nil
	}
}
