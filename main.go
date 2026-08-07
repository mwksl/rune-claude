// The claude-changes extension shows the files Claude Code is changing,
// live, inside the Rune IDE. It reads the ledger maintained by the
// rune-claude hook CLI and renders a panel with open/diff actions.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/unstablebuild/rune-go-sdk/api/browserapi"
	"github.com/unstablebuild/rune-go-sdk/api/config"
	"github.com/unstablebuild/rune-go-sdk/api/extensionapi"
	"github.com/unstablebuild/rune-go-sdk/api/textapi"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"github.com/unstablebuild/rune-go-sdk/iterator"
	"github.com/unstablebuild/rune-go-sdk/term"

	"github.com/mwksl/rune-claude/internal/ledger"
	"github.com/mwksl/rune-claude/internal/transcript"
)

func main() {
	meta := extensionapi.Metadata{
		DeveloperID:      "mwksl",
		DeveloperKey:     "0000",
		DeveloperEmail:   "matthewstingel@fastmail.com",
		ExtensionID:      "claude-changes",
		ExtensionName:    "Claude Changes",
		ExtensionVersion: "0.3.0",
		Permissions: extensionapi.NewPermissions(
			extensionapi.PermissionCommands,
			extensionapi.PermissionFileSystem,
			extensionapi.PermissionBrowserResourceOpener,
			extensionapi.PermissionBrowserWindowManager,
			extensionapi.PermissionNotifications,
			extensionapi.PermissionInterrupt,
		),
	}

	if err := extensionapi.ServeWorkspaceExtension(
		extensionapi.FuncWorkspaceExtension(run), meta,
	); err != nil {
		slog.Error("claude-changes exited", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, ws *extensionapi.Workspace, _ config.Config) error {
	st, err := ledger.Open(ledger.DefaultDir())
	if err != nil {
		return fmt.Errorf("open ledger: %w", err)
	}

	svc := &service{
		st:     st,
		wm:     ws.WindowManager(ctx),
		opener: ws.ResourceOpener(ctx),
		notif:  ws.Notifications(ctx),
		fs:     ws.FileSystem(ctx),
		intr:   ws.Interrupter(ctx),
	}

	changesManual := textapi.CommandManual{
		Name:     "claude-changes",
		Summary:  "Show the files Claude Code is changing in this and other sessions.",
		Synopsis: "[open|clear]",
	}
	if err := ws.RegisterCommand(changesManual, svc); err != nil {
		return fmt.Errorf("register command: %w", err)
	}
	feedManual := textapi.CommandManual{
		Name:     "claude-feed",
		Summary:  "Live feed of the most recent Claude Code conversation.",
		Synopsis: "[open]",
	}
	if err := ws.RegisterCommand(feedManual, svc); err != nil {
		return fmt.Errorf("register feed command: %w", err)
	}

	go svc.poll(ctx)

	slog.Info("claude-changes ready", "ledger", st.EventsPath())
	return nil
}

// service owns the panel lifecycle and bridges ledger updates into redraws.
type service struct {
	st     *ledger.Store
	wm     browserapi.WindowManager
	opener browserapi.ResourceOpener
	notif  browserapi.Notifications
	fs     workspaceapi.FileSystem
	intr   term.Interrupter

	mu       sync.Mutex
	panel    *panel
	win      browserapi.Window
	origin   browserapi.Window
	lastSize int64

	feed       *feed
	feedWin    browserapi.Window
	tail       *transcript.Tail
	followPath string
	scanTick   int
}

// poll watches the ledger for growth (or truncation) and pushes fresh
// models into the open panel.
func (s *service) poll(ctx context.Context) {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}

		size, err := s.st.Size()
		if err != nil {
			continue
		}
		s.mu.Lock()
		changed := size != s.lastSize
		s.lastSize = size
		p := s.panel
		s.mu.Unlock()

		if changed && p != nil {
			s.refresh(ctx, p)
		}

		s.feedTick(ctx)
	}
}

// feedTick keeps the open feed pointed at the most recently active
// transcript and appends whatever that session wrote since last tick.
func (s *service) feedTick(ctx context.Context) {
	s.mu.Lock()
	f := s.feed
	tail := s.tail
	s.scanTick++
	rescan := s.scanTick%4 == 0 || tail == nil // re-pick target every ~2s
	s.mu.Unlock()
	if f == nil {
		return
	}

	if rescan {
		path, _ := transcript.Latest(s.transcriptCandidates())
		if path != "" {
			s.mu.Lock()
			if path != s.followPath {
				s.followPath = path
				tail = transcript.NewTail(path)
				s.tail = tail
				s.mu.Unlock()
				f.Reset(transcript.SessionLabel(path))
			} else {
				s.mu.Unlock()
			}
		}
	}
	if tail == nil {
		s.mu.Lock()
		tail = s.tail
		s.mu.Unlock()
		if tail == nil {
			return
		}
	}

	entries, err := tail.Next()
	if err != nil || len(entries) == 0 {
		return
	}
	f.Append(entries)
	if err := s.intr.Interrupt(ctx); err != nil {
		slog.Warn("interrupt failed", "error", err)
	}
}

// transcriptCandidates lists transcript paths recorded in the ledger,
// most recent activity first.
func (s *service) transcriptCandidates() []string {
	evs, err := s.st.Events()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for i := len(evs) - 1; i >= 0; i-- {
		t := evs[i].Transcript
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

func (s *service) refresh(ctx context.Context, p *panel) {
	evs, err := s.st.Events()
	if err != nil {
		return
	}
	p.SetGroups(BuildModel(evs))
	if err := s.intr.Interrupt(ctx); err != nil {
		slog.Warn("interrupt failed", "error", err)
	}
}

// HandleCommand dispatches both registered commands: `claude-changes`
// toggles the file panel (or clears the ledger), `claude-feed` toggles the
// live conversation feed.
func (s *service) HandleCommand(ctx context.Context, cmd textapi.Command) error {
	sub := "open"
	if len(cmd.Args) > 0 {
		sub = cmd.Args[0]
	}

	if cmd.Name == "claude-feed" {
		if sub != "open" {
			return fmt.Errorf("unknown subcommand %q: claude-feed [open]", sub)
		}
		return s.toggleFeed(ctx, cmd.Window)
	}

	switch sub {
	case "open":
		return s.togglePanel(ctx, cmd.Window)
	case "clear":
		if err := s.st.Clear(); err != nil {
			return err
		}
		s.mu.Lock()
		p := s.panel
		s.mu.Unlock()
		if p != nil {
			s.refresh(ctx, p)
		}
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q: claude-changes [open|clear]", sub)
	}
}

// Complete implements command completion.
func (s *service) Complete(_ context.Context, cmd string, _ []string) (iterator.Iterator[string], error) {
	if cmd == "claude-feed" {
		return iterator.FromSlice([]string{"open"}), nil
	}
	return iterator.FromSlice([]string{"open", "clear"}), nil
}

func (s *service) togglePanel(ctx context.Context, origin browserapi.Window) error {
	s.mu.Lock()
	if s.win != nil {
		win := s.win
		s.win, s.panel, s.origin = nil, nil, nil
		s.mu.Unlock()
		return s.wm.CloseWindow(win)
	}
	s.mu.Unlock()

	if origin == nil {
		f, err := s.wm.Focus()
		if err != nil {
			return fmt.Errorf("no window to split: %w", err)
		}
		origin = f
	}

	p := newPanel(panelActions{
		open:          func(row FileRow) { go s.openFile(ctx, row) },
		diff:          func(row FileRow) { go s.openDiff(ctx, row) },
		clear:         func() { go s.clearLedger(ctx) },
		removeSession: func(id string) { go s.removeSession(ctx, id) },
		removeEnded:   func() { go s.removeEnded(ctx) },
		closeReq:      func() { go s.closePanel() },
		onClosed:      func() { s.forgetPanel() },
	})

	evs, err := s.st.Events()
	if err != nil {
		return err
	}
	p.SetGroups(BuildModel(evs))

	win, err := s.wm.Split(browserapi.OrientationRight, origin, p)
	if err != nil {
		return fmt.Errorf("split window: %w", err)
	}

	s.mu.Lock()
	s.panel, s.win, s.origin = p, win, origin
	size, _ := s.st.Size()
	s.lastSize = size
	s.mu.Unlock()
	return nil
}

func (s *service) toggleFeed(ctx context.Context, origin browserapi.Window) error {
	s.mu.Lock()
	if s.feedWin != nil {
		win := s.feedWin
		s.feedWin, s.feed, s.tail, s.followPath = nil, nil, nil, ""
		s.mu.Unlock()
		return s.wm.CloseWindow(win)
	}
	s.mu.Unlock()

	if origin == nil {
		w, err := s.wm.Focus()
		if err != nil {
			return fmt.Errorf("no window to split: %w", err)
		}
		origin = w
	}

	f := newFeed(feedActions{
		closeReq: func() { go s.closeFeed() },
		onClosed: func() { s.forgetFeed() },
	})

	// Point at the current transcript immediately so the feed opens with
	// history instead of waiting for the next poll tick.
	path, _ := transcript.Latest(s.transcriptCandidates())
	var tail *transcript.Tail
	if path != "" {
		tail = transcript.NewTail(path)
		f.Reset(transcript.SessionLabel(path))
		if entries, err := tail.Next(); err == nil {
			f.Append(entries)
		}
	} else {
		f.Reset("no session found")
	}

	win, err := s.wm.Split(browserapi.OrientationBottom, origin, f)
	if err != nil {
		return fmt.Errorf("split window: %w", err)
	}

	s.mu.Lock()
	s.feed, s.feedWin, s.tail, s.followPath = f, win, tail, path
	s.mu.Unlock()
	return nil
}

func (s *service) closeFeed() {
	s.mu.Lock()
	win := s.feedWin
	s.feedWin, s.feed, s.tail, s.followPath = nil, nil, nil, ""
	s.mu.Unlock()
	if win == nil {
		return
	}
	if err := s.wm.CloseWindow(win); err != nil {
		s.notifyErr("close feed: %v", err)
	}
}

// forgetFeed drops feed state without touching the window — used when the
// host already closed it.
func (s *service) forgetFeed() {
	s.mu.Lock()
	s.feedWin, s.feed, s.tail, s.followPath = nil, nil, nil, ""
	s.mu.Unlock()
}

func (s *service) closePanel() {
	s.mu.Lock()
	win := s.win
	s.win, s.panel, s.origin = nil, nil, nil
	s.mu.Unlock()
	if win == nil {
		return
	}
	if err := s.wm.CloseWindow(win); err != nil {
		s.notifyErr("close panel: %v", err)
	}
}

// forgetPanel drops panel state without touching the window — used when the
// host already closed it.
func (s *service) forgetPanel() {
	s.mu.Lock()
	s.win, s.panel, s.origin = nil, nil, nil
	s.mu.Unlock()
}

func (s *service) openFile(ctx context.Context, row FileRow) {
	if _, err := os.Stat(row.Path); err != nil {
		s.notifyWarn("%s no longer exists on disk", row.Path)
		return
	}
	uri, err := s.fs.URI(row.Path)
	if err != nil {
		s.notifyErr("resolve %s: %v", row.Path, err)
		return
	}
	h, err := s.opener.Open(uri)
	if err != nil {
		s.notifyErr("open %s: %v", row.Path, err)
		return
	}
	s.showInOrigin(h)
}

func (s *service) openDiff(ctx context.Context, row FileRow) {
	patch, err := s.st.DiffAgainstBaseline(row.Session, row.Path)
	switch {
	case errors.Is(err, ledger.ErrNoBaseline):
		s.notifyWarn("no pre-edit baseline recorded for %s", filepath.Base(row.Path))
		return
	case err != nil:
		s.notifyErr("diff %s: %v", row.Path, err)
		return
	}
	if patch == "" {
		s.notifyInfo("%s matches its pre-edit baseline", filepath.Base(row.Path))
		return
	}

	dir, err := os.MkdirTemp("", "rune-claude-diff-")
	if err != nil {
		s.notifyErr("create diff dir: %v", err)
		return
	}
	path := filepath.Join(dir, filepath.Base(row.Path)+".diff")
	if err := os.WriteFile(path, []byte(patch), 0o600); err != nil {
		s.notifyErr("write diff: %v", err)
		return
	}

	uri, err := s.fs.URI(path)
	if err != nil {
		s.notifyErr("resolve diff: %v", err)
		return
	}
	h, err := s.opener.Open(uri)
	if err != nil {
		s.notifyErr("open diff: %v", err)
		return
	}
	s.showInOrigin(h)
}

// showInOrigin focuses the opened tab in the window the panel was launched
// from; if that window is gone the tab still exists in the tab bar.
func (s *service) showInOrigin(h browserapi.Handler) {
	s.mu.Lock()
	origin := s.origin
	s.mu.Unlock()
	if origin == nil {
		s.notifyInfo("opened in a new tab")
		return
	}
	if err := s.wm.SetWindowContent(origin, h); err != nil {
		s.notifyInfo("opened in a new tab (original window is gone)")
	}
}

// removeSession drops one session's events and snapshots from the ledger.
func (s *service) removeSession(ctx context.Context, id string) {
	if err := s.st.RemoveSessions(id); err != nil {
		s.notifyErr("drop session: %v", err)
		return
	}
	s.refreshOpenPanel(ctx)
	s.notifyInfo("dropped session %s", shortID(id))
}

// removeEnded drops every session whose last lifecycle event is an end.
func (s *service) removeEnded(ctx context.Context) {
	evs, err := s.st.Events()
	if err != nil {
		s.notifyErr("drop ended sessions: %v", err)
		return
	}
	ids := ledger.EndedSessions(evs)
	if len(ids) == 0 {
		s.notifyInfo("no ended sessions to drop")
		return
	}
	if err := s.st.RemoveSessions(ids...); err != nil {
		s.notifyErr("drop ended sessions: %v", err)
		return
	}
	s.refreshOpenPanel(ctx)
	s.notifyInfo("dropped %d ended session(s)", len(ids))
}

func (s *service) refreshOpenPanel(ctx context.Context) {
	s.mu.Lock()
	p := s.panel
	s.mu.Unlock()
	if p != nil {
		s.refresh(ctx, p)
	}
}

func (s *service) clearLedger(ctx context.Context) {
	if err := s.st.Clear(); err != nil {
		s.notifyErr("clear: %v", err)
		return
	}
	s.mu.Lock()
	p := s.panel
	s.mu.Unlock()
	if p != nil {
		s.refresh(ctx, p)
	}
	s.notifyInfo("cleared recorded Claude Code changes")
}

func (s *service) notifyErr(format string, args ...any) {
	if _, err := s.notif.Notify(browserapi.LevelError, format, args...); err != nil {
		slog.Warn("notify failed", "error", err)
	}
}

func (s *service) notifyWarn(format string, args ...any) {
	if _, err := s.notif.Notify(browserapi.LevelWarn, format, args...); err != nil {
		slog.Warn("notify failed", "error", err)
	}
}

func (s *service) notifyInfo(format string, args ...any) {
	if _, err := s.notif.Notify(browserapi.LevelInfo, format, args...); err != nil {
		slog.Warn("notify failed", "error", err)
	}
}
