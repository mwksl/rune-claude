# rune-claude

See the files [Claude Code](https://www.anthropic.com/claude-code) is changing, live, inside the
[Rune IDE](https://rune.build).

Rune's official Claude Code integration wires up semantic tools and
notifications, but nothing shows you *what Claude is editing*. This package
fills that gap with two pieces:

- **`rune-claude`** — a CLI that Claude Code's hooks invoke on every file
  edit. It appends events to a ledger (`~/.local/state/rune-claude/`) and
  snapshots each file's pre-edit content the first time a session touches it.
- **`claude-changes`** — a Rune extension that watches that ledger and shows
  a live panel of changed files per Claude session: open them, view a diff
  against the pre-edit baseline, or clear the list.

```
 Claude changes
 ● 4f2a91c3 · ~/Development/myapp · active
   M cmd/server/main.go        15:07 ×3
   A internal/api/routes.go    15:04
 ⏎ open · d diff · c clear · q close
```

Because the ledger is just files, everything works no matter where Claude
Code runs — a Rune terminal, another terminal app, even over SSH — and the
CLI is useful on its own (`rune-claude status` / `diff`) without Rune.

## Install

Requires Go 1.25+.

```sh
# the hook CLI, onto your PATH
go install github.com/matthewstingel/rune-claude/cmd/rune-claude@latest

# the extension, as a Rune package (from Rune's console)
pkg install github.com/matthewstingel/rune-claude
```

To run the extension from a checkout instead (Rune console):

```
extensions start claude-changes /path/to/rune-claude/extension
```

On first use Rune will prompt to authorize the extension's permissions
(commands, file system, window manager, resource opener, notifications,
interrupt).

## Wire up Claude Code

In each project you want tracked (or once with `--user` for everything):

```sh
rune-claude setup          # writes .claude/settings.local.json
rune-claude setup --user   # or ~/.claude/settings.json
```

This installs `PreToolUse`/`PostToolUse` hooks (matcher
`Edit|Write|MultiEdit|NotebookEdit`) plus `SessionStart`/`SessionEnd`,
all pointing at `rune-claude hook`. Existing hooks are preserved; running
setup again is a no-op. Restart Claude Code sessions to pick hooks up.

The hook always exits 0 — a broken observer must never block Claude Code.
Failures are logged to `~/.local/state/rune-claude/hook.log`.

## Use

In Rune, run the `claude-changes` command (command prompt) to toggle the
panel; it splits the current window. Keys:

| key | action |
| --- | ------ |
| `↑`/`↓`, `k`/`j` | move selection |
| `Enter` | open the file in the window the panel was launched from |
| `d` | open a unified diff against the session's pre-edit baseline |
| `c` | clear all recorded changes and snapshots |
| `q` / `Esc` | close the panel |

From any terminal:

```sh
rune-claude status         # sessions and changed files
rune-claude diff <file>    # unified diff vs pre-edit baseline
rune-claude clear
```

## How it works

```
Claude Code ── PreToolUse hook ──▶ rune-claude hook ──▶ snapshot baseline
            ── PostToolUse hook ─▶ rune-claude hook ──▶ append change event
                                                          │
                       ~/.local/state/rune-claude/events.jsonl
                                                          │  (poll)
Rune IDE ◀── claude-changes extension (Go SDK: Split/Open/Notify) ◀──┘
```

- Baselines are per `(session, file)`: the first touch wins, so `d` always
  shows the session's cumulative change even after many edits.
- A file created by Claude diffs from `/dev/null` and is marked `A`.
- `PreToolUse` records no event — the edit may still be rejected. Only
  `PostToolUse` (the edit actually happened) lands in the ledger.

## Roadmap

- Phase 2: implement Claude Code's IDE protocol (WebSocket MCP server +
  `~/.claude/ide/<port>.lock`) so Rune shows proposed edits as diffs
  *before* they're applied, like the official VS Code extension. See
  [claudecode.nvim's PROTOCOL.md](https://github.com/coder/claudecode.nvim/blob/main/PROTOCOL.md).
- Workspace-scoped filtering of sessions.
- `git diff --no-index`-quality colorized diffs in a dedicated viewer.
