# sido-askpass

`SUDO_ASKPASS` shim for environments without a usable TTY — tmux, Herdr, and coding agents (Codex, OpenCode, Pi).

When sudo needs a password and can't read from the terminal, it runs this program instead. Passwords never touch disk (FIFOs / stdout only).

## Install

```bash
npm i -g sido-askpass
# or: pnpm add -g sido-askpass
```

Requires **Node.js 24+**.

## Setup

Session-only:

```bash
export SUDO_ASKPASS=$(command -v sido-askpass)
```

Persist:

```bash
sido-askpass install --user    # ~/.profile → SUDO_ASKPASS
# or
sido-askpass install --system  # /etc/sudo.conf → Path askpass
```

Then:

```bash
sudo -A <command>
```

On modern Fedora, plain `sudo` often falls back to askpass automatically when there is no TTY.

```bash
sido-askpass status [--user|--system]
sido-askpass uninstall --user|--system
```

## How it prompts

| Context                      | Method                                                 |
| ---------------------------- | ------------------------------------------------------ |
| `$TMUX` set                  | `tmux command-prompt -N` (secret), FIFO back to parent |
| `$HERDR_ENV=1`               | Herdr pane split + bash `read -s`, FIFO back           |
| macOS + GUI                  | `osascript` hidden dialog                              |
| Linux + `$DISPLAY` / Wayland | `zenity` → `kdialog`                                   |
| else                         | `read -s` on `/dev/tty`                                |

### tmux

Inside a tmux session, sudo password entry uses tmux's built-in secret prompt — no GUI dialog, no extra pane. Works well when agents or nested shells run under tmux and can't steal the TTY.

### Agents / Herdr

In Herdr (`HERDR_ENV=1`), a temporary pane collects the password. Same idea for Codex / OpenCode / Pi: point `SUDO_ASKPASS` at this binary so privilege prompts don't hang the session.

## Security

Passwords are passed through kernel FIFOs (tmux / Herdr) or process stdout (GUI / TTY). Nothing is written to a regular file.
