package ledger

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// ErrNoBaseline is returned when no snapshot exists for the requested file.
var ErrNoBaseline = errors.New("no baseline snapshot recorded for file")

// UnifiedDiff shells out to diff -u and returns the patch text. An empty
// string means the files are identical. Exit status 1 (differences found)
// is success; anything above is an error.
func UnifiedDiff(labelA, fileA, labelB, fileB string) (string, error) {
	cmd := exec.Command("diff", "-u", "-L", labelA, "-L", labelB, fileA, fileB)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return out.String(), nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return out.String(), nil
	}
	return "", fmt.Errorf("diff failed: %w: %s", err, stderr.String())
}

// DiffAgainstBaseline diffs the file's session baseline against its current
// content on disk. A created file diffs from /dev/null; a since-deleted file
// diffs to /dev/null. Pass session "" to use the most recent baseline from
// any session.
func (s *Store) DiffAgainstBaseline(session, path string) (string, error) {
	var (
		meta SnapshotMeta
		orig string
		ok   bool
		err  error
	)
	if session == "" {
		_, meta, orig, ok, err = s.LatestSnapshot(path)
	} else {
		meta, orig, ok, err = s.SnapshotFor(session, path)
	}
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrNoBaseline
	}

	fileA, labelA := orig, path+" (before)"
	if meta.Absent {
		fileA, labelA = os.DevNull, "/dev/null"
	}
	fileB, labelB := path, path+" (after)"
	if _, statErr := os.Stat(path); statErr != nil {
		fileB, labelB = os.DevNull, path+" (deleted)"
	}
	return UnifiedDiff(labelA, fileA, labelB, fileB)
}
