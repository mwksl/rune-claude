package ledger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustOpen(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return st
}

func TestAppendAndEvents(t *testing.T) {
	st := mustOpen(t)

	evs := []Event{
		{Time: time.Now(), Kind: KindSessionStart, Session: "s1", CWD: "/tmp/p"},
		{Time: time.Now(), Kind: KindChange, Session: "s1", Tool: "Edit", Path: "/tmp/p/a.go", Action: ActionModified},
		{Time: time.Now(), Kind: KindChange, Session: "s1", Tool: "Write", Path: "/tmp/p/b.go", Action: ActionCreated},
	}
	for _, ev := range evs {
		if err := st.Append(ev); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	got, err := st.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(got) != len(evs) {
		t.Fatalf("got %d events, want %d", len(got), len(evs))
	}
	for i := range evs {
		if got[i].Kind != evs[i].Kind || got[i].Path != evs[i].Path || got[i].Action != evs[i].Action {
			t.Errorf("event %d = %+v, want %+v", i, got[i], evs[i])
		}
	}
}

func TestEventsSkipsMalformedLines(t *testing.T) {
	st := mustOpen(t)
	if err := st.Append(Event{Kind: KindChange, Session: "s1", Path: "/x"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(st.EventsPath(), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not json\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := st.Append(Event{Kind: KindChange, Session: "s1", Path: "/y"}); err != nil {
		t.Fatal(err)
	}

	got, err := st.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(got) != 2 || got[0].Path != "/x" || got[1].Path != "/y" {
		t.Fatalf("got %+v, want the two valid events", got)
	}
}

func TestSnapshotFirstTouchWins(t *testing.T) {
	st := mustOpen(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(target, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	meta, err := st.EnsureSnapshot("s1", target)
	if err != nil {
		t.Fatalf("EnsureSnapshot: %v", err)
	}
	if meta.Absent {
		t.Fatal("baseline marked absent for existing file")
	}

	// Mutate the file, snapshot again: baseline must not move.
	if err := os.WriteFile(target, []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureSnapshot("s1", target); err != nil {
		t.Fatalf("EnsureSnapshot second touch: %v", err)
	}

	_, orig, ok, err := st.SnapshotFor("s1", target)
	if err != nil || !ok {
		t.Fatalf("SnapshotFor: ok=%v err=%v", ok, err)
	}
	b, err := os.ReadFile(orig)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "original\n" {
		t.Fatalf("baseline content = %q, want original", b)
	}
}

func TestSnapshotAbsentFile(t *testing.T) {
	st := mustOpen(t)
	target := filepath.Join(t.TempDir(), "new.txt")

	meta, err := st.EnsureSnapshot("s1", target)
	if err != nil {
		t.Fatalf("EnsureSnapshot: %v", err)
	}
	if !meta.Absent {
		t.Fatal("baseline for missing file not marked absent")
	}
	_, orig, ok, err := st.SnapshotFor("s1", target)
	if err != nil || !ok {
		t.Fatalf("SnapshotFor: ok=%v err=%v", ok, err)
	}
	if orig != "" {
		t.Fatalf("absent baseline should have no orig file, got %q", orig)
	}
}

func TestDiffAgainstBaseline(t *testing.T) {
	st := mustOpen(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(target, []byte("line one\nline two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureSnapshot("s1", target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("line one\nline 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	patch, err := st.DiffAgainstBaseline("s1", target)
	if err != nil {
		t.Fatalf("DiffAgainstBaseline: %v", err)
	}
	if !strings.Contains(patch, "-line two") || !strings.Contains(patch, "+line 2") {
		t.Fatalf("patch missing expected hunks:\n%s", patch)
	}

	// Unknown file → ErrNoBaseline.
	if _, err := st.DiffAgainstBaseline("s1", filepath.Join(dir, "never.txt")); err != ErrNoBaseline {
		t.Fatalf("err = %v, want ErrNoBaseline", err)
	}
}

func TestDiffCreatedFile(t *testing.T) {
	st := mustOpen(t)
	target := filepath.Join(t.TempDir(), "new.txt")
	if _, err := st.EnsureSnapshot("s1", target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	patch, err := st.DiffAgainstBaseline("s1", target)
	if err != nil {
		t.Fatalf("DiffAgainstBaseline: %v", err)
	}
	if !strings.Contains(patch, "+hello") {
		t.Fatalf("patch missing +hello:\n%s", patch)
	}
}

func TestLatestSnapshotPicksNewest(t *testing.T) {
	st := mustOpen(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(target, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureSnapshot("old-session", target); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(target, []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureSnapshot("new-session", target); err != nil {
		t.Fatal(err)
	}

	session, _, orig, ok, err := st.LatestSnapshot(target)
	if err != nil || !ok {
		t.Fatalf("LatestSnapshot: ok=%v err=%v", ok, err)
	}
	if session != "new-session" {
		t.Fatalf("session = %q, want new-session", session)
	}
	b, _ := os.ReadFile(orig)
	if string(b) != "v2\n" {
		t.Fatalf("latest baseline = %q, want v2", b)
	}
}

func TestClear(t *testing.T) {
	st := mustOpen(t)
	target := filepath.Join(t.TempDir(), "f.txt")
	os.WriteFile(target, []byte("x"), 0o644)
	st.EnsureSnapshot("s1", target)
	st.Append(Event{Kind: KindChange, Session: "s1", Path: target})

	if err := st.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	evs, err := st.Events()
	if err != nil || len(evs) != 0 {
		t.Fatalf("after Clear: events=%v err=%v", evs, err)
	}
	if _, _, ok, _ := st.SnapshotFor("s1", target); ok {
		t.Fatal("snapshot survived Clear")
	}
}

func TestRemoveSessions(t *testing.T) {
	st := mustOpen(t)
	dir := t.TempDir()
	keepFile := filepath.Join(dir, "keep.txt")
	dropFile := filepath.Join(dir, "drop.txt")
	os.WriteFile(keepFile, []byte("k"), 0o644)
	os.WriteFile(dropFile, []byte("d"), 0o644)
	st.EnsureSnapshot("keep", keepFile)
	st.EnsureSnapshot("drop", dropFile)
	st.Append(Event{Kind: KindSessionStart, Session: "keep"})
	st.Append(Event{Kind: KindChange, Session: "keep", Path: keepFile})
	st.Append(Event{Kind: KindSessionStart, Session: "drop"})
	st.Append(Event{Kind: KindChange, Session: "drop", Path: dropFile})

	if err := st.RemoveSessions("drop"); err != nil {
		t.Fatalf("RemoveSessions: %v", err)
	}

	evs, err := st.Events()
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if ev.Session == "drop" {
			t.Fatalf("drop session survived: %+v", ev)
		}
	}
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2 kept", len(evs))
	}
	if _, _, ok, _ := st.SnapshotFor("drop", dropFile); ok {
		t.Fatal("drop snapshot survived")
	}
	if _, _, ok, _ := st.SnapshotFor("keep", keepFile); !ok {
		t.Fatal("keep snapshot was removed")
	}
}

func TestEndedSessions(t *testing.T) {
	evs := []Event{
		{Kind: KindSessionStart, Session: "a"},
		{Kind: KindSessionEnd, Session: "a"},
		{Kind: KindSessionStart, Session: "b"},
		{Kind: KindSessionEnd, Session: "b"},
		{Kind: KindSessionStart, Session: "b"}, // resumed
		{Kind: KindChange, Session: "c", Path: "/x"},
	}
	got := EndedSessions(evs)
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("EndedSessions = %v, want [a]", got)
	}
}
