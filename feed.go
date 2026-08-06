package main

import (
	"strings"
	"sync"

	"github.com/unstablebuild/rune-go-sdk/term"

	"github.com/mwksl/rune-claude/internal/transcript"
)

// maxFeedEntries bounds memory for long sessions; older entries fall off.
const maxFeedEntries = 500

// feedActions are the callbacks the feed fires on user input.
type feedActions struct {
	closeReq func()
	onClosed func()
}

// feedLine is one wrapped display line.
type feedLine struct {
	text  string
	attrs term.Attributes
}

// feed renders a live conversation view of one Claude Code session:
// prompts, assistant text, thinking activity, and tool calls.
type feed struct {
	actions feedActions

	mu           sync.Mutex
	label        string
	entries      []transcript.Entry
	lines        []feedLine
	top          int
	follow       bool
	showThinking bool
	width        int
	height       int
	closeOnce    sync.Once
}

func newFeed(actions feedActions) *feed {
	return &feed{actions: actions, follow: true, showThinking: true}
}

// Reset clears the feed for a new session.
func (f *feed) Reset(label string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.label = label
	f.entries = nil
	f.top = 0
	f.follow = true
	f.rebuildLocked()
}

// Append adds new entries and, in follow mode, keeps the view pinned to the
// bottom.
func (f *feed) Append(entries []transcript.Entry) {
	if len(entries) == 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, entries...)
	if len(f.entries) > maxFeedEntries {
		f.entries = f.entries[len(f.entries)-maxFeedEntries:]
	}
	f.rebuildLocked()
}

// rebuildLocked re-wraps all entries into display lines. Caller holds f.mu.
func (f *feed) rebuildLocked() {
	f.lines = f.lines[:0]
	width := max(f.width, 8)

	prevKind, prevRole := "", ""
	for _, e := range f.entries {
		if e.Kind == "thinking" && !f.showThinking {
			continue
		}
		// Collapse runs of empty thinking markers into one.
		if e.Kind == "thinking" && e.Text == "" && prevKind == "thinking" {
			continue
		}

		// Blank separator when the speaker or texture changes; tool calls
		// cluster without gaps.
		if len(f.lines) > 0 && !(e.Kind == "tool" && prevKind == "tool") {
			if e.Kind != prevKind || e.Role != prevRole {
				f.lines = append(f.lines, feedLine{})
			}
		}

		switch e.Kind {
		case "tool":
			text := " ⚒ " + e.Tool
			if e.Text != "" {
				text += " · " + e.Text
			}
			f.appendWrapped(text, "   ", term.Attributes{Fg: term.ColorBlue}, width)
		case "thinking":
			f.appendWrapped(" ✻ thinking…", "   ", term.Attributes{Fg: term.ColorGray}, width)
			if e.Text != "" {
				f.appendWrapped(e.Text, "   ", term.Attributes{Fg: term.ColorGray}, width)
			}
		default: // text
			if e.Role == "user" {
				f.appendWrapped(" ❯ "+e.Text, "   ", term.Attributes{Attrs: term.AttrBold}, width)
			} else {
				f.appendWrapped(" "+e.Text, " ", term.Attributes{}, width)
			}
		}
		prevKind, prevRole = e.Kind, e.Role
	}

	if f.follow {
		f.top = f.maxTopLocked()
	} else {
		f.top = clamp(f.top, 0, f.maxTopLocked())
	}
}

// appendWrapped word-wraps text (which may span paragraphs) into lines,
// indenting continuation lines with cont.
func (f *feed) appendWrapped(text, cont string, attrs term.Attributes, width int) {
	for _, para := range strings.Split(text, "\n") {
		line, lineLen := "", 0
		flush := func() {
			if lineLen > 0 {
				f.lines = append(f.lines, feedLine{text: line, attrs: attrs})
				line, lineLen = cont, len([]rune(cont))
			}
		}
		if para == "" {
			f.lines = append(f.lines, feedLine{attrs: attrs})
			continue
		}
		for _, word := range strings.Split(para, " ") {
			wlen := len([]rune(word))
			switch {
			case lineLen == 0 || lineLen+1+wlen <= width:
				if lineLen > 0 && line != "" && !strings.HasSuffix(line, " ") {
					line += " "
					lineLen++
				}
				line += word
				lineLen += wlen
			default:
				flush()
				// Hard-split words wider than the pane.
				for wlen+lineLen > width {
					take := width - lineLen
					r := []rune(word)
					line += string(r[:take])
					f.lines = append(f.lines, feedLine{text: line, attrs: attrs})
					word, wlen = string(r[take:]), wlen-take
					line, lineLen = cont, len([]rune(cont))
				}
				line += word
				lineLen += wlen
			}
		}
		flush()
	}
}

func (f *feed) maxTopLocked() int {
	return max(0, len(f.lines)-(f.height-2))
}

func (f *feed) Resize(width, height int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if width != f.width {
		f.width, f.height = width, height
		f.rebuildLocked()
		return
	}
	f.height = height
	if f.follow {
		f.top = f.maxTopLocked()
	}
}

func (f *feed) Draw(w term.Writer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.width <= 0 || f.height <= 0 {
		return
	}

	state := "following"
	if !f.follow {
		state = "paused — f to follow"
	}
	title := " Claude feed · " + f.label + " · " + state
	drawLine(w, 0, f.width, title, "", term.Attributes{Attrs: term.AttrBold})

	contentH := f.height - 2
	for y := 0; y < contentH; y++ {
		idx := f.top + y
		if idx >= len(f.lines) {
			blankLine(w, y+1, f.width)
			continue
		}
		drawLine(w, y+1, f.width, f.lines[idx].text, "", f.lines[idx].attrs)
	}

	think := "t hide thinking"
	if !f.showThinking {
		think = "t show thinking"
	}
	drawLine(w, f.height-1, f.width, " "+think+" · f follow · q close", "",
		term.Attributes{Attrs: term.AttrUnderline})
}

func (f *feed) Handle(ev term.Event) (exit, handled bool) {
	switch {
	case ev.Key == term.KeyArrowUp || ev.Ch == 'k' || ev.Key == term.MouseWheelUp:
		f.scrollBy(-3)
		return false, true
	case ev.Key == term.KeyArrowDown || ev.Ch == 'j' || ev.Key == term.MouseWheelDown:
		f.scrollBy(3)
		return false, true
	case ev.Ch == 'f' || ev.Key == term.KeyEnd:
		f.mu.Lock()
		f.follow = true
		f.top = f.maxTopLocked()
		f.mu.Unlock()
		return false, true
	case ev.Ch == 't':
		f.mu.Lock()
		f.showThinking = !f.showThinking
		f.rebuildLocked()
		f.mu.Unlock()
		return false, true
	case ev.Ch == 'q' || ev.Key == term.KeyEsc:
		if f.actions.closeReq != nil {
			f.actions.closeReq()
		}
		return false, true
	}
	return false, false
}

func (f *feed) scrollBy(delta int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	maxTop := f.maxTopLocked()
	f.top = clamp(f.top+delta, 0, maxTop)
	// Scrolling up pauses; landing back on the bottom resumes following.
	f.follow = f.top == maxTop
}

func (f *feed) Cursor() (term.Coordinates, term.CursorStyle, bool) {
	return term.Coordinates{}, term.CursorStyleDefault, false
}

func (f *feed) Selection() (string, bool) { return "", false }

func (f *feed) Close() error {
	f.closeOnce.Do(func() {
		if f.actions.onClosed != nil {
			f.actions.onClosed()
		}
	})
	return nil
}
