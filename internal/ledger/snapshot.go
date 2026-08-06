package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SnapshotMeta describes the pre-edit baseline of one file in one session.
type SnapshotMeta struct {
	Path   string    `json:"path"`
	Absent bool      `json:"absent"` // file did not exist before the session touched it
	Time   time.Time `json:"time"`
}

func (s *Store) snapshotsRoot() string { return filepath.Join(s.dir, "snapshots") }

func (s *Store) sessionDir(session string) string {
	if session == "" {
		session = "unknown"
	}
	return filepath.Join(s.snapshotsRoot(), session)
}

func pathHash(path string) string {
	h := sha256.Sum256([]byte(path))
	return hex.EncodeToString(h[:])[:20]
}

// EnsureSnapshot records the baseline for (session, path) if none exists yet:
// the file's current content, or an "absent" marker when the file does not
// exist. First touch wins; later calls return the existing baseline.
func (s *Store) EnsureSnapshot(session, path string) (SnapshotMeta, error) {
	if meta, _, ok, err := s.SnapshotFor(session, path); err != nil {
		return SnapshotMeta{}, err
	} else if ok {
		return meta, nil
	}

	dir := s.sessionDir(session)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return SnapshotMeta{}, fmt.Errorf("create snapshot dir: %w", err)
	}

	hash := pathHash(path)
	meta := SnapshotMeta{Path: path, Time: time.Now()}

	content, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		meta.Absent = true
	case err != nil:
		return SnapshotMeta{}, fmt.Errorf("read baseline of %s: %w", path, err)
	default:
		if err := os.WriteFile(filepath.Join(dir, hash+".orig"), content, 0o600); err != nil {
			return SnapshotMeta{}, fmt.Errorf("write baseline: %w", err)
		}
	}

	b, err := json.Marshal(meta)
	if err != nil {
		return SnapshotMeta{}, fmt.Errorf("marshal snapshot meta: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, hash+".json"), b, 0o600); err != nil {
		return SnapshotMeta{}, fmt.Errorf("write snapshot meta: %w", err)
	}
	return meta, nil
}

// SnapshotFor returns the baseline for (session, path) if one was recorded.
// origPath is the stored pre-edit content file; it is empty when the
// baseline is "file was absent".
func (s *Store) SnapshotFor(session, path string) (meta SnapshotMeta, origPath string, ok bool, err error) {
	dir := s.sessionDir(session)
	hash := pathHash(path)

	b, err := os.ReadFile(filepath.Join(dir, hash+".json"))
	if err != nil {
		if os.IsNotExist(err) {
			return SnapshotMeta{}, "", false, nil
		}
		return SnapshotMeta{}, "", false, fmt.Errorf("read snapshot meta: %w", err)
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return SnapshotMeta{}, "", false, fmt.Errorf("parse snapshot meta: %w", err)
	}
	if !meta.Absent {
		origPath = filepath.Join(dir, hash+".orig")
	}
	return meta, origPath, true, nil
}

// LatestSnapshot finds the most recent baseline recorded for path across all
// sessions. Useful when the caller knows the file but not the session.
func (s *Store) LatestSnapshot(path string) (session string, meta SnapshotMeta, origPath string, ok bool, err error) {
	entries, err := os.ReadDir(s.snapshotsRoot())
	if err != nil {
		if os.IsNotExist(err) {
			return "", SnapshotMeta{}, "", false, nil
		}
		return "", SnapshotMeta{}, "", false, fmt.Errorf("list snapshots: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m, orig, found, err := s.SnapshotFor(e.Name(), path)
		if err != nil || !found {
			continue
		}
		if !ok || m.Time.After(meta.Time) {
			session, meta, origPath, ok = e.Name(), m, orig, true
		}
	}
	return session, meta, origPath, ok, nil
}
