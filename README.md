# sido-askpass

[![npm version](https://img.shields.io/npm/v/sido-askpass)](https://www.npmjs.com/package/sido-askpass)
[![CI](https://github.com/patdx/sido-askpass/actions/workflows/ci.yml/badge.svg)](https://github.com/patdx/sido-askpass/actions/workflows/ci.yml)
[![License](https://img.shields.io/npm/l/sido-askpass)](https://github.com/patdx/sido-askpass/blob/main/LICENSE)

`SUDO_ASKPASS` shim for tmux, Herdr, coding agents, and other environments
without a usable TTY.

When sudo uses askpass, `sido-askpass` opens a hidden password prompt and
returns the password to sudo. Passwords never touch a regular file.

`sido` is written in Go and ships as a single native binary per platform
(Linux x86_64 and macOS arm64), distributed through npm. The `sido` and
`sido-askpass` executables are tiny POSIX `sh` launchers that pick the right
binary for your OS; `sido-askpass` is just an alias for `sido askpass`. No
Node.js or bash is required at runtime.

Works with:

- **Linux** — hidden GUI or terminal password prompts
- **macOS** — hidden native password dialog
- **Codex automatic review** — approved retries can authenticate outside the sandbox
- **Codex Full Access** — sudo authentication from agent-run commands
- **tmux** — hidden popup prompt without stealing the agent's TTY
- **Herdr** — temporary password prompt pane that closes automatically
- **Raw terminals** — `/dev/tty` fallback when an interactive shell owns the TTY
- **Watch mode** — approve from a second terminal when a TUI/agent owns the TTY (e.g. `ssh` → coding agent → `sudo` with no tmux/Herdr)

Supports Linux (x86_64) and macOS (arm64). Distributed as native binaries — no
Node.js or bash at runtime. tmux/Herdr/GUI prompting requires the respective
external tool (`tmux`, `herdr`, or `zenity`/`kdialog`); the `/dev/tty` and watch
backends need only a POSIX shell (for the launcher). Windows is not supported.

## Quick start

The package installs two executable names: `sido` is the command-line interface,
while `sido-askpass` is reserved for the askpass protocol (it is an alias for
`sido askpass` and treats its first argument as a prompt). Both are shell
launchers that pick the binary for the current OS and architecture
(`sido-linux-amd64`, `sido-linux-arm64`, `sido-darwin-amd64`, `sido-darwin-arm64`).

### User setup (recommended)

Install the command, add `SUDO_ASKPASS` to `~/.profile`, and load it into the
current shell:

```bash
npm install -g sido-askpass && sido install --user
```

User setup selects the startup file from `$SHELL`: `~/.zshrc` for zsh,
`~/.bashrc` for Bash, and `~/.profile` for other shells. The managed entry is
appended after existing shell setup so tools such as fnm are initialized first.
Existing sido-managed entries in other supported startup files are migrated to
the selected file. For zsh, the managed block also adds
`alias sudo='sudo -A'`, making askpass the default for interactive commands.
Unmanaged `SUDO_ASKPASS` entries are preserved.

Restart the shell after installation, or activate the current session with the
exact `export SUDO_ASKPASS=...` command printed by the installer.
Shell startup configuration covers interactive shell sessions; GUI applications
launched without that shell environment may not inherit `SUDO_ASKPASS`.

### System setup

Install the command and configure `Path askpass` in `/etc/sudo.conf`:

```bash
npm install -g sido-askpass && sido install --system
```

System setup runs `sudo tee` to update `/etc/sudo.conf`, so the installation
command itself must be run somewhere sudo can authenticate.

After first setup, rerun the detected managed user and/or system installation
without remembering its scope:

```bash
sido install
```

If no managed installation exists yet, `install` requires an explicit `--user`
or `--system`.

To use sido for one command without changing your profile or sudo
configuration:

```bash
sido run -- sudo -A <command>
```

Everything after `--` is run directly without shell parsing. `run` sets
`SUDO_ASKPASS` but does not add `-A` or otherwise rewrite the command, so pass
`-A` explicitly when sudo must use askpass even if a TTY is available. Select
an exact prompt adapter for the command with:

```bash
sido run --adapter watch -- sudo -A <command>
```

## Using sudo

For the most reliable behavior, explicitly tell sudo to use askpass:

```bash
sudo -A <command>
```

On modern sudo installations, plain sudo commonly falls back to askpass when
there is no usable TTY. This automatic fallback has been verified on Fedora:

```bash
sudo <command>
```

TTY allocation is the important distinction:

- Without a TTY, plain sudo can automatically use the configured askpass
  helper.
- With a real or pseudo-TTY, plain sudo prompts on that terminal instead and
  does not call askpass.
- Some coding agents start background commands with a pseudo-TTY, so automatic
  fallback cannot be assumed even when the agent UI has no visible terminal.
- `sudo -A` requests askpass regardless of whether a TTY exists.

The zsh user installer adds this alias to its managed block, making askpass the
default for interactive commands:

```bash
alias sudo='sudo -A'
```

Shell aliases usually do not affect commands launched directly by coding
agents or other non-interactive processes. Bash and other shell installations
do not add the alias. `sido-askpass` does not rewrite sudo commands or add `-A`
outside the zsh alias.

## Codex test matrix

| Platform and terminal        | Codex permissions | Prompt backend | Command        | Result      | Notes                                                                                                                                 |
| ---------------------------- | ----------------- | -------------- | -------------- | ----------- | ------------------------------------------------------------------------------------------------------------------------------------- |
| Linux, tmux 3.7b             | Automatic review  | tmux popup     | `sudo -A`      | Tested      | The sandbox cannot reach tmux; the approved retry outside the sandbox succeeds.                                                       |
| Linux, Herdr 0.7.5           | Automatic review  | Herdr pane     | `sudo -A`      | Tested      | The sandbox cannot reach Herdr; the approved retry outside the sandbox succeeds.                                                      |
| Fedora 44, raw Wayland shell | Automatic review  | Zenity 4.2.2   | `sudo -A true` | Tested      | 2026-07-30, sudo 1.9.17p2; no tmux, Herdr, or stdin TTY.                                                                             |
| Linux, any terminal          | Full Access       | First matching | `sudo -A`      | Supported   | sudo and the selected prompt backend can run outside the sandbox.                                                                     |
| Linux, any terminal          | Workspace-write   | None           | Any sudo       | Unsupported | `no_new_privileges` blocks sudo; changing the askpass backend cannot bypass this.                                                     |
| Linux, pseudo-TTY command    | Any sudo-capable  | Askpass        | Plain `sudo`   | Skipped     | sudo uses the allocated TTY instead of askpass; use `sudo -A` to force askpass.                                                       |
| Linux, raw terminal          | Automatic review  | `/dev/tty`     | `sudo -A`      | Untested    | No tmux, Herdr, `DISPLAY`, or Wayland; requires an accessible controlling terminal.                                                   |
| Linux, pi (no TTY, GUI)      | Automatic review  | Zenity         | Plain `sudo`   | Tested      | 2026-07-30, sudo 1.9.17p2: no tmux, Herdr, or TTY; `DISPLAY`+zenity available. Plain sudo auto-fell back to askpass.                  |
| macOS                        | Any               | AppleScript    | `sudo -A`      | Untested    | The dialog backend exists, but Codex sandbox and approval behavior has not been tested.                                               |

Codex automatic approval review does not widen the sandbox itself. It is useful
because it can approve an eligible retry outside the sandbox. Full Access also
works, but grants Codex substantially broader system access.

When a sandboxed Linux Codex process cannot reach tmux, `sido-askpass` exits
immediately instead of hanging and prints:

```text
[sido] tmux access denied; retry the sudo command with escalated permissions
```

This gives Codex an actionable reason to retry with escalated permission.

## Other clients

| Client   | Compatibility | Notes                                                                                                                       |
| -------- | ------------- | --------------------------------------------------------------------------------------------------------------------------- |
| Pi       | Tested        | No TTY — plain `sudo` auto-falls back to askpass; zenity GUI prompt. 2026-07-30, Fedora 44, sudo 1.9.17p2.                   |
| OpenCode | Tested        | 2026-07-30, Fedora 44, `/dev/tty` fallback, `sudo -A whoami` → `root`.                                                       |

## Prompt backend compatibility

By default, `SIDO_ADAPTER=auto` uses the first matching context:

| Context                      | Prompt method                                       | Status                  |
| ---------------------------- | --------------------------------------------------- | ----------------------- |
| `$TMUX` set                  | tmux popup with a hidden native read; FIFO return   | Tested on Linux         |
| `$HERDR_ENV=1`               | Temporary Herdr pane with a hidden native read      | Tested on Linux         |
| macOS                        | Hidden `osascript` dialog                           | Implemented, unverified |
| Linux + `$DISPLAY` / Wayland | `zenity`, then `kdialog`                            | Tested on Linux         |
| Canonical `/dev/tty`         | Hidden native read on `/dev/tty`                    | Fallback                |
| Raw/no TTY                   | Watch mode: park request + FIFO, `approve`          | New                     |

Set `SIDO_ADAPTER` to bypass detection and require one exact adapter:

| Adapter     | Behavior                                              |
| ----------- | ----------------------------------------------------- |
| `auto`      | Use the detection chain above; this is the default    |
| `tmux`      | Open a hidden prompt in a tmux popup                  |
| `herdr`     | Open a hidden prompt in a temporary Herdr pane        |
| `osascript` | Open the native macOS password dialog; requires macOS |
| `zenity`    | Open a Zenity password dialog                         |
| `kdialog`   | Open a KDE password dialog                            |
| `tty`       | Read a hidden password from `/dev/tty`                |
| `watch`     | Wait for `sido approve` from another terminal         |

An explicitly selected adapter either succeeds or exits with an error. It never
falls back to another adapter. `run` accepts the same selection as
`--adapter <name>`:

```bash
SIDO_ADAPTER=osascript sudo -A <command>
sido run --adapter tty -- sudo -A <command>
```

The prompt also shows the requesting command when the parent process command
line is available. If process inspection is blocked, it silently shows the
normal password prompt without the command. The displayed command includes its
arguments, which may reveal sensitive arguments to anyone who can see the
prompt.

## Watch mode

When no inline prompt is safe — a coding agent's TUI owns the TTY in raw mode,
or there is no TTY at all — `sido-askpass` parks the request and waits for a
password from a second terminal:

```bash
sido approve   # approve the most recent pending request (one-shot)
sido watch     # approve requests as they arrive (Ctrl-C to exit)
```

The original terminal prints a hint telling you which command to run. Requests
live under `$XDG_RUNTIME_DIR/sido` (or `~/.cache/sido`); the password travels
through a kernel FIFO, never a file. `SIDO_ADAPTER=watch` selects watch mode;
`SIDO_WATCH_TIMEOUT=<sec>` sets how long it waits before giving up (default
120).

## Status and removal

Upgrade an npm installation to the latest version and refresh the user
configuration:

```bash
sido upgrade
sido update          # alias for upgrade
```

The upgrade command currently supports npm global installations only and
rejects commands installed by another package manager. It checks the latest
published version, upgrades only when that version is newer (or always with
`--force`), then runs `sido install` to refresh the existing managed
configuration. If npm fails, existing configuration is left unchanged.

Inspect the active environment and installed configuration:

```bash
sido status
sido status --user
sido status --system
```

Remove either setup:

```bash
sido uninstall --user
sido uninstall --system
```

`--user` and `--system` are mutually exclusive for install, uninstall, and
status. Omitting both is supported only by status.

## Security

Password input is hidden. tmux, Herdr, and watch mode return the password
through a kernel FIFO; GUI and TTY backends return it through process stdout.
Each tmux, Herdr, or watch prompt uses a private directory (mode 0700 under
`$XDG_RUNTIME_DIR/sido` for watch) with a mode-0600 FIFO. Its FIFO and prompt
are accessible only to the current user and are removed when prompting ends.
Watch's approver feeds the password through the FIFO over stdin, never argv, so
it never appears in `ps`. The prompt file contains the displayed command and
prompt, but never the password.
