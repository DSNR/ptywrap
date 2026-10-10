# ptywrap

A terminal multiplexer TUI built on [Bubble Tea](https://github.com/charmbracelet/bubbletea), with tabs, scrollback, search, mouse selection, and theming.

## Features

- **Tabs** — multiple shell sessions in one window (`F2` new, `F3` close, `F4`/`F6` prev/next)
- **Persistent named sessions** — detach (`F9`) and reattach later, tmux-style
- **Scrollback** with mouse wheel, `PgUp`/`PgDn`, and a draggable scrollbar
- **Search** through scrollback (`/` while scrolled back, `n`/`N` to jump matches)
- **Mouse selection** — click-drag to select, double-click for word, triple-click for line
- **Right-click** copies the current selection, or pastes the clipboard if nothing is selected
- **Ctrl+click** opens URLs under the cursor
- **SSH awareness** — the pane border changes color and shows the target host while an `ssh` session is in the foreground
- **Bell flash** on terminal bell
- **Configurable themes**, including built-in presets (`dracula`, `nord`, `gruvbox-dark`, `solarized-dark`)
- Optional per-session logging to disk

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/DSNR/ptywrap/master/install.sh | bash
```

This downloads the latest release binary for your OS/arch into `~/.local/bin` (override with `PTYWRAP_INSTALL_DIR`). Make sure that directory is on your `PATH`.

### Build from source

```sh
go build -o ptywrap .
```

Requires Go 1.27+.

## Usage

```sh
ptywrap
```

### Sessions

Like tmux, ptywrap runs your tabs inside a background session that survives after you detach or close the terminal.

```sh
ptywrap                  # attach to (or start) the default session "main"
ptywrap attach work      # attach to (or start) the session "work"
ptywrap session list     # list running sessions (alias: ptywrap ls)
ptywrap session kill work      # end a session and all its tabs (alias: ptywrap kill work)
ptywrap session rename work dev  # rename a running session
```

Press `F9` to detach; the session keeps running and you can reattach later. Attaching from a second terminal takes over the session (the previous client exits with `[session attached elsewhere]`). A session ends, and its background process exits, once all of its tabs have exited.

Each session is a daemon listening on `$XDG_RUNTIME_DIR/ptywrap/<name>.sock` (or `/tmp/ptywrap-$UID/<name>.sock`); its log is next to it as `<name>.log`.

Generate a default config file to customize:

```sh
ptywrap --init-config
```

This writes `$XDG_CONFIG_HOME/ptywrap/config.json` (or `~/.config/ptywrap/config.json`) without overwriting an existing one.

Show usage without entering the app:

```sh
ptywrap --help
```

### Keybindings

| Key | Action |
|---|---|
| `F2` | New tab |
| `F3` | Close current tab |
| `F4` / `F6` | Previous / next tab |
| `F9` | Detach from session (it keeps running) |
| `PgUp` / `PgDn` | Scroll scrollback |
| `Home` / `End` (while scrolled) | Jump to oldest / live |
| `/` (while scrolled) | Start search |
| `n` / `N` | Next / previous search match |
| Mouse wheel | Scroll |
| Click-drag | Select text |
| Double-click | Select word |
| Triple-click | Select line |
| Ctrl+click | Open URL under cursor |
| Right-click | Copy selection, or paste clipboard if nothing is selected |

## Configuration

`~/.config/ptywrap/config.json`:

```json
{
  "shell": "",
  "scrollbackSize": 5000,
  "logDir": "",
  "hideStatusBar": false,
  "copyOnSelect": false,
  "cursorStyle": "block",
  "theme": {
    "preset": "",
    "borderColor": "2",
    "bellColor": "196",
    "selectionColor": "237",
    "searchColor": "58",
    "sshColor": "3",
    "sshHostColors": {
      "forge": "#a020f0",
      "65.45.87.101": "4"
    },
    "palette": ["", "", "", "", "", "", "", "", "", "", "", "", "", "", "", ""]
  }
}
```

| Field | Description |
|---|---|
| `shell` | Shell to launch (default: `$SHELL`, else `/bin/sh`) |
| `scrollbackSize` | Lines of history kept per tab |
| `logDir` | If set, each session's raw output is logged to a file here |
| `hideStatusBar` | Hide the tab bar |
| `copyOnSelect` | Copy to the clipboard as soon as a selection is made (default: off; right-click copies/pastes instead) |
| `cursorStyle` | `"block"` (character shown inverted) or `"bar"` (thin vertical bar; blinks over, replacing, the character under it) |
| `theme.preset` | One of `dracula`, `nord`, `gruvbox-dark`, `solarized-dark`; overridden by any explicit fields below it |
| `theme.borderColor` / `bellColor` / `selectionColor` / `searchColor` / `sshColor` | ANSI or hex colors for pane border states |
| `theme.sshHostColors` | Map of substring → ANSI/hex color; overrides `sshColor` when the ssh target (e.g. `user@host`) contains the key |
| `theme.palette` | 16 ANSI color overrides (indices 0–15); empty entries keep the terminal default |

## Releases

Pushing a tag like `v1.0.0` triggers a GitHub Actions build that publishes standalone binaries (linux/darwin, amd64/arm64) to GitHub Releases.
