package main

import (
	"sort"
	"time"

	"github.com/matthewstingel/rune-claude/internal/ledger"
)

// FileRow is one changed file inside a session, aggregated over all of the
// session's change events for that path.
type FileRow struct {
	Session string
	Path    string
	CWD     string
	Action  ledger.Action
	Count   int
	Last    time.Time
}

// SessionGroup is one Claude Code session and the files it changed, newest
// activity first.
type SessionGroup struct {
	Session string
	CWD     string
	Started time.Time
	Ended   bool
	Last    time.Time
	Files   []FileRow
}

// BuildModel folds the raw event log into session groups ordered by most
// recent activity. A file that was ever "created" in a session stays
// "created" even if edited again afterwards — its baseline is still empty.
func BuildModel(evs []ledger.Event) []SessionGroup {
	type acc struct {
		group SessionGroup
		files map[string]*FileRow
		order []string
	}
	sessions := map[string]*acc{}
	var order []string

	get := func(id string) *acc {
		a, ok := sessions[id]
		if !ok {
			a = &acc{group: SessionGroup{Session: id}, files: map[string]*FileRow{}}
			sessions[id] = a
			order = append(order, id)
		}
		return a
	}

	for _, ev := range evs {
		a := get(ev.Session)
		if ev.CWD != "" && a.group.CWD == "" {
			a.group.CWD = ev.CWD
		}
		if ev.Time.After(a.group.Last) {
			a.group.Last = ev.Time
		}
		switch ev.Kind {
		case ledger.KindSessionStart:
			a.group.Started = ev.Time
			a.group.Ended = false
		case ledger.KindSessionEnd:
			a.group.Ended = true
		case ledger.KindChange:
			if ev.Path == "" {
				continue
			}
			row, ok := a.files[ev.Path]
			if !ok {
				row = &FileRow{Session: ev.Session, Path: ev.Path, CWD: ev.CWD, Action: ev.Action}
				a.files[ev.Path] = row
				a.order = append(a.order, ev.Path)
			}
			if ev.Action == ledger.ActionCreated {
				row.Action = ledger.ActionCreated
			}
			row.Count++
			if ev.Time.After(row.Last) {
				row.Last = ev.Time
			}
		}
	}

	groups := make([]SessionGroup, 0, len(order))
	for _, id := range order {
		a := sessions[id]
		files := make([]FileRow, 0, len(a.order))
		for _, p := range a.order {
			files = append(files, *a.files[p])
		}
		sort.SliceStable(files, func(i, j int) bool { return files[i].Last.After(files[j].Last) })
		a.group.Files = files
		groups = append(groups, a.group)
	}
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].Last.After(groups[j].Last) })
	return groups
}
