package main

import (
	"testing"
	"time"

	"github.com/matthewstingel/rune-claude/internal/ledger"
)

func TestBuildModelGroupsAndOrders(t *testing.T) {
	t0 := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }

	evs := []ledger.Event{
		{Time: at(0), Kind: ledger.KindSessionStart, Session: "old", CWD: "/p1"},
		{Time: at(1), Kind: ledger.KindChange, Session: "old", Path: "/p1/a.go", Action: ledger.ActionModified, CWD: "/p1"},
		{Time: at(2), Kind: ledger.KindSessionEnd, Session: "old"},
		{Time: at(3), Kind: ledger.KindSessionStart, Session: "new", CWD: "/p2"},
		{Time: at(4), Kind: ledger.KindChange, Session: "new", Path: "/p2/x.go", Action: ledger.ActionCreated, CWD: "/p2"},
		{Time: at(5), Kind: ledger.KindChange, Session: "new", Path: "/p2/y.go", Action: ledger.ActionModified, CWD: "/p2"},
		{Time: at(6), Kind: ledger.KindChange, Session: "new", Path: "/p2/x.go", Action: ledger.ActionModified, CWD: "/p2"},
	}

	groups := BuildModel(evs)
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2", len(groups))
	}

	// Most recent activity first.
	if groups[0].Session != "new" || groups[1].Session != "old" {
		t.Fatalf("group order = %s,%s want new,old", groups[0].Session, groups[1].Session)
	}
	if !groups[1].Ended {
		t.Error("old session not marked ended")
	}

	newg := groups[0]
	if len(newg.Files) != 2 {
		t.Fatalf("new session has %d files, want 2", len(newg.Files))
	}
	// x.go touched last → listed first; created wins over the later edit.
	if newg.Files[0].Path != "/p2/x.go" || newg.Files[0].Action != ledger.ActionCreated || newg.Files[0].Count != 2 {
		t.Fatalf("first row = %+v, want x.go created ×2", newg.Files[0])
	}
	if newg.Files[1].Path != "/p2/y.go" || newg.Files[1].Action != ledger.ActionModified {
		t.Fatalf("second row = %+v, want y.go modified", newg.Files[1])
	}
}

func TestBuildModelEmpty(t *testing.T) {
	if got := BuildModel(nil); len(got) != 0 {
		t.Fatalf("BuildModel(nil) = %+v, want empty", got)
	}
}
