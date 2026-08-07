package ledger

import (
	"encoding/json"
	"fmt"
	"os"
)

// RemoveSessions deletes all events and snapshots belonging to the given
// sessions. The event log is rewritten via temp file + atomic rename; an
// append racing the rewrite can be lost, which is acceptable for a local
// observer — the hook will keep recording the live session afterwards.
func (s *Store) RemoveSessions(ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	drop := map[string]bool{}
	for _, id := range ids {
		drop[id] = true
	}

	evs, err := s.Events()
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(s.dir, "events-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp event log: %w", err)
	}
	defer os.Remove(tmp.Name())

	for _, ev := range evs {
		if drop[ev.Session] {
			continue
		}
		b, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		if _, err := tmp.Write(append(b, '\n')); err != nil {
			tmp.Close()
			return fmt.Errorf("write temp event log: %w", err)
		}
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp event log: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.EventsPath()); err != nil {
		return fmt.Errorf("replace event log: %w", err)
	}

	for id := range drop {
		if err := os.RemoveAll(s.sessionDir(id)); err != nil {
			return fmt.Errorf("remove snapshots of %s: %w", id, err)
		}
	}
	return nil
}

// EndedSessions returns the sessions whose most recent lifecycle event is
// session_end. A session that ended and was later resumed (a new
// session_start) does not count as ended.
func EndedSessions(evs []Event) []string {
	state := map[string]bool{}
	var order []string
	for _, ev := range evs {
		if _, seen := state[ev.Session]; !seen {
			order = append(order, ev.Session)
		}
		switch ev.Kind {
		case KindSessionEnd:
			state[ev.Session] = true
		case KindSessionStart:
			state[ev.Session] = false
		default:
			if _, seen := state[ev.Session]; !seen {
				state[ev.Session] = false
			}
		}
	}
	var out []string
	for _, id := range order {
		if state[id] {
			out = append(out, id)
		}
	}
	return out
}
