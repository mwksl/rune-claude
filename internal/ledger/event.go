// Package ledger records the files Claude Code changes as an append-only
// JSONL event log plus pre-edit snapshots, shared between the hook CLI
// (writer) and the Rune extension (reader).
package ledger

import "time"

// Kind classifies a ledger event.
type Kind string

const (
	// KindChange records one applied file modification.
	KindChange Kind = "change"
	// KindSessionStart records a Claude Code session starting.
	KindSessionStart Kind = "session_start"
	// KindSessionEnd records a Claude Code session ending.
	KindSessionEnd Kind = "session_end"
)

// Action describes what a change did to the file, judged against the
// pre-edit snapshot taken the first time a session touches it.
type Action string

const (
	// ActionCreated means the file did not exist before this session touched it.
	ActionCreated Action = "created"
	// ActionModified means the file existed and was edited.
	ActionModified Action = "modified"
)

// Event is one line in events.jsonl.
type Event struct {
	Time    time.Time `json:"time"`
	Kind    Kind      `json:"kind"`
	Session string    `json:"session"`
	CWD     string    `json:"cwd,omitempty"`
	Tool    string    `json:"tool,omitempty"`
	Path    string    `json:"path,omitempty"`
	Action  Action    `json:"action,omitempty"`
}
