package ledger

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const eventsFile = "events.jsonl"

// Store is a ledger rooted at a state directory. All methods are safe for
// concurrent use across processes: appends are single O_APPEND writes and
// readers only trust complete lines.
type Store struct {
	dir string
}

// DefaultDir resolves the state directory: $RUNE_CLAUDE_STATE_DIR, then
// $XDG_STATE_HOME/rune-claude, then ~/.local/state/rune-claude.
func DefaultDir() string {
	if v := os.Getenv("RUNE_CLAUDE_STATE_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "rune-claude")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "rune-claude")
	}
	return filepath.Join(home, ".local", "state", "rune-claude")
}

// Open returns a Store rooted at dir, creating it if needed.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Dir returns the state directory backing this store.
func (s *Store) Dir() string { return s.dir }

// EventsPath returns the path of the JSONL event log.
func (s *Store) EventsPath() string { return filepath.Join(s.dir, eventsFile) }

// Append writes one event as a single JSONL line.
func (s *Store) Append(ev Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	f, err := os.OpenFile(s.EventsPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open event log: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("append event: %w", err)
	}
	return nil
}

// Events reads every complete event in the log. Malformed lines are skipped
// so one bad write can never wedge readers.
func (s *Store) Events() ([]Event, error) {
	f, err := os.Open(s.EventsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open event log: %w", err)
	}
	defer f.Close()

	var evs []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		evs = append(evs, ev)
	}
	if err := sc.Err(); err != nil {
		return evs, fmt.Errorf("scan event log: %w", err)
	}
	return evs, nil
}

// Size returns the current byte size of the event log, 0 if absent. Readers
// poll this to detect growth (or truncation) cheaply.
func (s *Store) Size() (int64, error) {
	fi, err := os.Stat(s.EventsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return fi.Size(), nil
}

// Clear removes the event log and all snapshots.
func (s *Store) Clear() error {
	if err := os.Remove(s.EventsPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove event log: %w", err)
	}
	if err := os.RemoveAll(s.snapshotsRoot()); err != nil {
		return fmt.Errorf("remove snapshots: %w", err)
	}
	return nil
}
