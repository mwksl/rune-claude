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

	"github.com/matthewstingel/rune-claude/internal/ledger"
)

func main() {
	meta := extensionapi.Metadata{
		DeveloperID:      "matthewstingel",
		DeveloperKey:     "0000",
		DeveloperEmail:   "matthewstingel@fastmail.com",
		ExtensionID:      "claude-changes",
		ExtensionName:    "Claude Changes",
		ExtensionVersion: "0.1.0",
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

	manual := textapi.CommandManual{
		Name:     "claude-changes",
		Summary:  "Show the files Claude Code is changing in this and other sessions.",
		Synopsis: "[open|clear]",
	}
	if err := ws.RegisterCommand(manual, svc); err != nil {
		return fmt.Errorf("register command: %w", err)
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

		if !changed || p == nil {
			continue
		}
		s.refresh(ctx, p)
	}
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

// HandleCommand implements the `claude-changes` command: no argument (or
// "open") toggles the panel; "clear" wipes the ledger.
func (s *service) HandleCommand(ctx context.Context, cmd textapi.Command) error {
	sub := "open"
	if len(cmd.Args) > 0 {
		sub = cmd.Args[0]
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
func (s *service) Complete(context.Context, string, []string) (iterator.Iterator[string], error) {
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
		open:     func(row FileRow) { go s.openFile(ctx, row) },
		diff:     func(row FileRow) { go s.openDiff(ctx, row) },
		clear:    func() { go s.clearLedger(ctx) },
		closeReq: func() { go s.closePanel() },
		onClosed: func() { s.forgetPanel() },
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
