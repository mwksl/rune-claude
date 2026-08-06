package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/unstablebuild/rune-go-sdk/term"

	"github.com/matthewstingel/rune-claude/internal/ledger"
)

// panelActions are the callbacks the panel fires on user input. They must
// not block: the service runs the real work on its own goroutines.
type panelActions struct {
	open     func(FileRow)
	diff     func(FileRow)
	clear    func()
	closeReq func()
	onClosed func()
}

// line is one rendered content row: left-aligned text plus a right-aligned
// suffix, with an optional selectable file row index.
type line struct {
	left  string
	right string
	attrs term.Attributes
	row   int // index into panel.rows, -1 if not selectable
}

// panel is the "Claude changes" list: a browserapi.Handler installed into a
// Rune window split. It renders session groups and dispatches key actions.
type panel struct {
	actions panelActions

	mu        sync.Mutex
	groups    []SessionGroup
	rows      []FileRow
	lines     []line
	sel       int
	top       int
	width     int
	height    int
	closeOnce sync.Once
}

func newPanel(actions panelActions) *panel {
	return &panel{actions: actions, sel: -1}
}

// SetGroups swaps in a fresh model, keeping the selection on the same
// (session, path) row when it still exists.
func (p *panel) SetGroups(groups []SessionGroup) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var prevSession, prevPath string
	if p.sel >= 0 && p.sel < len(p.rows) {
		prevSession, prevPath = p.rows[p.sel].Session, p.rows[p.sel].Path
	}

	p.groups = groups
	p.rebuildLines()

	p.sel = -1
	for i, r := range p.rows {
		if r.Session == prevSession && r.Path == prevPath {
			p.sel = i
			break
		}
	}
	if p.sel == -1 && len(p.rows) > 0 {
		p.sel = 0
	}
	p.ensureVisibleLocked()
}

// rebuildLines flattens groups into display lines. Caller holds p.mu.
func (p *panel) rebuildLines() {
	p.rows = p.rows[:0]
	p.lines = p.lines[:0]

	p.lines = append(p.lines, line{
		left:  " Claude changes",
		attrs: term.Attributes{Attrs: term.AttrBold},
		row:   -1,
	})

	if len(p.groups) == 0 {
		p.lines = append(p.lines,
			line{row: -1},
			line{left: "  waiting for Claude Code activity…", row: -1},
			line{left: "  (run `rune-claude setup` in your project)", row: -1},
		)
		return
	}

	for _, g := range p.groups {
		p.lines = append(p.lines, line{row: -1})

		state := "active"
		if g.Ended {
			state = "ended"
		}
		header := fmt.Sprintf(" ● %s · %s · %s", shortID(g.Session), shortCWD(g.CWD), state)
		attrs := term.Attributes{Attrs: term.AttrBold}
		if g.Ended {
			attrs = term.Attributes{}
		}
		p.lines = append(p.lines, line{left: header, attrs: attrs, row: -1})

		for _, f := range g.Files {
			marker, attrs := "M", term.Attributes{Fg: term.ColorYellow}
			if f.Action == ledger.ActionCreated {
				marker, attrs = "A", term.Attributes{Fg: term.ColorGreen}
			}
			count := ""
			if f.Count > 1 {
				count = fmt.Sprintf(" ×%d", f.Count)
			}
			p.rows = append(p.rows, f)
			p.lines = append(p.lines, line{
				left:  fmt.Sprintf("   %s %s", marker, displayPath(f.Path, f.CWD)),
				right: fmt.Sprintf("%s%s ", f.Last.Format("15:04"), count),
				attrs: attrs,
				row:   len(p.rows) - 1,
			})
		}
	}
}

func (p *panel) Resize(width, height int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.width, p.height = width, height
	p.ensureVisibleLocked()
}

func (p *panel) Draw(w term.Writer) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.width <= 0 || p.height <= 0 {
		return
	}
	contentH := p.height - 1

	for y := 0; y < contentH; y++ {
		idx := p.top + y
		if idx >= len(p.lines) {
			blankLine(w, y, p.width)
			continue
		}
		l := p.lines[idx]
		attrs := l.attrs
		if l.row >= 0 && l.row == p.sel {
			attrs.Attrs |= term.AttrReverse
		}
		drawLine(w, y, p.width, l.left, l.right, attrs)
	}

	help := " ⏎ open · d diff · c clear · r refresh · q close"
	drawLine(w, p.height-1, p.width, help, "", term.Attributes{Attrs: term.AttrUnderline})
}

func (p *panel) Handle(ev term.Event) (exit, handled bool) {
	switch {
	case ev.Key == term.KeyArrowDown || ev.Ch == 'j':
		p.move(1)
		return false, true
	case ev.Key == term.KeyArrowUp || ev.Ch == 'k':
		p.move(-1)
		return false, true
	case ev.Key == term.MouseWheelDown:
		p.scroll(3)
		return false, true
	case ev.Key == term.MouseWheelUp:
		p.scroll(-3)
		return false, true
	case ev.Key == term.MouseLeft:
		p.click(ev.MouseY)
		return false, true
	case ev.Key == term.KeyEnter:
		if row, ok := p.selected(); ok && p.actions.open != nil {
			p.actions.open(row)
		}
		return false, true
	case ev.Ch == 'd':
		if row, ok := p.selected(); ok && p.actions.diff != nil {
			p.actions.diff(row)
		}
		return false, true
	case ev.Ch == 'c':
		if p.actions.clear != nil {
			p.actions.clear()
		}
		return false, true
	case ev.Ch == 'q' || ev.Key == term.KeyEsc:
		if p.actions.closeReq != nil {
			p.actions.closeReq()
		}
		return false, true
	case ev.Ch == 'r':
		return false, true // poller refreshes on its own; swallow for redraw
	}
	return false, false
}

func (p *panel) Cursor() (term.Coordinates, term.CursorStyle, bool) {
	return term.Coordinates{}, term.CursorStyleDefault, false
}

func (p *panel) Selection() (string, bool) { return "", false }

// Close is called when the window hosting the panel goes away.
func (p *panel) Close() error {
	p.closeOnce.Do(func() {
		if p.actions.onClosed != nil {
			p.actions.onClosed()
		}
	})
	return nil
}

func (p *panel) selected() (FileRow, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sel < 0 || p.sel >= len(p.rows) {
		return FileRow{}, false
	}
	return p.rows[p.sel], true
}

func (p *panel) move(delta int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.rows) == 0 {
		return
	}
	p.sel = clamp(p.sel+delta, 0, len(p.rows)-1)
	p.ensureVisibleLocked()
}

func (p *panel) scroll(delta int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	maxTop := max(0, len(p.lines)-(p.height-1))
	p.top = clamp(p.top+delta, 0, maxTop)
}

func (p *panel) click(y int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx := p.top + y
	if idx < 0 || idx >= len(p.lines) {
		return
	}
	if r := p.lines[idx].row; r >= 0 {
		p.sel = r
	}
}

// ensureVisibleLocked keeps the selected row inside the viewport. Caller
// holds p.mu.
func (p *panel) ensureVisibleLocked() {
	contentH := p.height - 1
	if contentH <= 0 {
		return
	}
	selLine := -1
	for i, l := range p.lines {
		if l.row == p.sel && p.sel >= 0 {
			selLine = i
			break
		}
	}
	if selLine == -1 {
		p.top = clamp(p.top, 0, max(0, len(p.lines)-contentH))
		return
	}
	if selLine < p.top {
		p.top = selLine
	}
	if selLine >= p.top+contentH {
		p.top = selLine - contentH + 1
	}
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// drawLine renders left- and right-aligned text on row y, padding the rest
// of the row so stale cells never linger.
func drawLine(w term.Writer, y, width int, left, right string, attrs term.Attributes) {
	rightRunes := []rune(right)
	rightStart := width - len(rightRunes)
	if rightStart < 0 {
		rightRunes, rightStart = rightRunes[:0], width
	}

	x := 0
	for _, r := range left {
		if x >= rightStart-1 && right != "" || x >= width {
			break
		}
		w.SetCell(term.Coordinates{X: x, Y: y}, term.NewCell(r, 1, attrs))
		x++
	}
	for ; x < rightStart; x++ {
		w.SetCell(term.Coordinates{X: x, Y: y}, term.NewCell(' ', 1, attrs))
	}
	for i, r := range rightRunes {
		w.SetCell(term.Coordinates{X: rightStart + i, Y: y}, term.NewCell(r, 1, attrs))
	}
}

func blankLine(w term.Writer, y, width int) {
	for x := 0; x < width; x++ {
		w.SetCell(term.Coordinates{X: x, Y: y}, term.NewCell(' ', 1, term.Attributes{}))
	}
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	if id == "" {
		return "(none)"
	}
	return id
}

func shortCWD(cwd string) string {
	if cwd == "" {
		return "?"
	}
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(cwd, home) {
		return "~" + strings.TrimPrefix(cwd, home)
	}
	return cwd
}

func displayPath(path, cwd string) string {
	if cwd != "" {
		if rel, err := filepath.Rel(cwd, path); err == nil && rel != "" &&
			!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
			return rel
		}
	}
	return path
}
