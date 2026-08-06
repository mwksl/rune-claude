package transcript

import (
	"bytes"
	"io"
	"os"
)

// Tail incrementally reads entries appended to one transcript file. Only
// complete lines are consumed; a truncated file restarts from the top.
type Tail struct {
	Path   string
	offset int64
}

// NewTail tails path from the beginning.
func NewTail(path string) *Tail { return &Tail{Path: path} }

// Next returns entries parsed from bytes appended since the last call.
func (t *Tail) Next() ([]Entry, error) {
	fi, err := os.Stat(t.Path)
	if err != nil {
		if os.IsNotExist(err) {
			t.offset = 0
			return nil, nil
		}
		return nil, err
	}
	if fi.Size() < t.offset {
		t.offset = 0 // truncated or rotated
	}
	if fi.Size() == t.offset {
		return nil, nil
	}

	f, err := os.Open(t.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return nil, err
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}

	// Consume only through the final newline; a partial trailing line is
	// left for the next call.
	end := bytes.LastIndexByte(buf, '\n')
	if end == -1 {
		return nil, nil
	}
	consumed := buf[:end+1]
	t.offset += int64(len(consumed))

	var out []Entry
	for _, line := range bytes.Split(consumed, []byte{'\n'}) {
		out = append(out, ParseLine(line)...)
	}
	return out, nil
}
