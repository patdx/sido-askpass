# sido-askpass

`SUDO_ASKPASS` shim for headless agent environments. Written in Go; ships as a
single native binary per OS (Linux x86_64, macOS arm64) distributed via npm.

## Runtime

- Two OS binaries: `sido-mac` (darwin/arm64), `sido-linux` (linux/amd64).
- Two thin POSIX `sh` launchers: `sido` and `sido-askpass`. Each resolves its own
  symlink (so it works behind npm's bin symlinks) and execs the platform binary.
  `sido-askpass` is just an alias for `sido askpass`.
- No Node.js, bash, or `mkfifo` required at runtime. The launchers are POSIX
  `sh`, not bash; the only runtime shell is those two thin launchers. An adapter
  needs its external tool (`tmux`, `herdr`, `zenity`/`kdialog`) when selected.

## Repository layout

- `go-src/` — the Go module (`module sido-go`).
  - `internal/sido/sido.go` — all implementation (one package).
  - `internal/sido/tty_linux.go` / `tty_darwin.go` / `tty_other.go` — termios
    detection (`ttyFlags`) behind build tags.
  - `internal/sido/sido_test.go` — unit tests (semver, config regexes, FIFO
    round-trip / cancel / empty-password / timeout).
  - `cmd/sido/main.go` — single entry point; dispatches subcommands.
  - `test-fakes/` — fake `tmux` / `herdr` scripts for manual e2e checks.
- `scripts/`
  - `build-go.sh` — cross-compiles both binaries into `dist/` and copies the
    launchers. Reads the version from `package.json` (ldflags `-X`).
  - `sido.sh`, `sido-askpass.sh` — the launcher sources (copied into `dist/`).
- `package.json` — npm packaging only (`bin`, `files`, `prepack` builds Go).
- `.node-version` — kept for CI (the npm publish step runs under Node).

## Entry-point dispatch (`cmd/sido/main.go`)

- `sido askpass [prompt]` → `AskpassMain(args)` — the askpass protocol; the first
  arg is the sudo/SSH prompt. This is what `sido-askpass` forwards to.
- `sido _inner_prompt_receiver <promptFile> <fifo> <dir> <shimPid> [paneID]` →
  `ReceiverMain(args)` — hidden subcommand. Runs inside a tmux popup / herdr
  pane; collects the password and writes it to the FIFO. **Top-level**, not under
  `askpass`.
- anything else → `ManagerMain()` — `install`, `uninstall`, `status`, `upgrade`,
  `run`, `watch`, `approve`, `--help`, `--version`.

## Usage

```bash
export SUDO_ASKPASS=/path/to/sido-askpass   # npm global bin, or dist/sido-askpass
sudo -A <command>          # or plain sudo on modern Fedora (auto-falls back when no TTY)
```

## Detection chain

`SIDO_ADAPTER=auto` (the default) tries, in order:

| Context              | Method                                                       |
| -------------------- | ------------------------------------------------------------ |
| `$TMUX` set          | `tmux display-popup` running the receiver; FIFO return       |
| `$HERDR_ENV=1`       | `herdr pane split` + `pane run` running the receiver         |
| macOS + GUI          | `osascript` hidden dialog                                    |
| Linux + `$DISPLAY`   | `zenity` → `kdialog`                                         |
| canonical `/dev/tty` | hidden read on `/dev/tty` (interactive shell; via termios)   |
| else                 | **watch mode**: park request + FIFO, `sido approve` supplies |

`SIDO_ADAPTER=tmux|herdr|osascript|zenity|kdialog|tty|watch` forces one exact
adapter; forced adapters fail rather than fall back. `run` grammar:
`sido run [--adapter <name>] -- <command> [args...]` (`--` required).

## Install / uninstall / status

```bash
sido install --user        # first user install to shell startup file
sido install               # reapplies detected managed scopes
sido install --system      # Path askpass in /etc/sudo.conf via sudo tee
sido uninstall --user      # removes managed shell entries
sido uninstall --system    # reverts /etc/sudo.conf
sido upgrade               # npm upgrade + refreshes managed config
sido status [--user|--system]
```

`--user` and `--system` are mutually exclusive. `install` writes the absolute
path to the `sido-askpass` launcher into the managed block.

## Watch mode (agent-agnostic fallback)

When no inline surface is safe, the shim parks the request and blocks on a FIFO.
Supply the password from a second terminal:

```bash
sido approve   # most recent pending request (one-shot)
sido watch     # long-lived (Ctrl-C to exit)
```

Requests live under `$XDG_RUNTIME_DIR/sido` (or `~/.cache/sido`), mode 0700; each
is a `sido-*` dir holding the prompt and a mode-0600 `password` FIFO.
`SIDO_ADAPTER=watch` forces it; `SIDO_WATCH_TIMEOUT=<sec>` (default 120).

## Architecture notes (the important stuff)

- **No embedded bash.** The only shell scripts in the package are the two thin
  POSIX `sh` launchers (`sido`, `sido-askpass`) that just exec the platform
  binary; the adapter logic itself is pure Go. Password reads use
  `golang.org/x/term.ReadPassword`; tty detection reads kernel termios via
  `golang.org/x/sys/unix` `ioctl` (no `stty -a` parsing); `mkfifo` is
  `unix.Mkfifo`; tmux/herdr/zenity/etc. are invoked with `exec.Command`
  structured args (no shell, no quoting/injection surface).
- **FIFO + cancel detection.** `readFifoPassword` opens the FIFO
  `O_RDONLY|O_NONBLOCK` and classifies each `read`: `EAGAIN` = writer connected,
  still typing; data = password; `EOF` *after* the writer connected = the
  surface closed without a password = **cancel** (detected the instant the
  receiver dies, even via `SIGKILL`, because the kernel closes its fd). EOF
  *before* any writer = "not started yet" (keep waiting). This is why a closed
  pane/popup now bails immediately instead of hanging. The earlier `O_RDWR`
  trick was abandoned precisely because holding our own write end masked EOF.
- **Orphan handling lives in the receiver.** The receiver process runs inside the
  popup/pane and survives the shim. It pid-polls the shim
  (`syscall.Kill(shimPid, 0)`, portable) and on shim death removes the temp dir
  + closes the pane + exits → tmux `-E` / herdr `pane run` close the surface.
  (Linux `pidfd_open` / macOS `kqueue` would be event-driven refinements.)
- **herdr `pane run` is fire-and-forget.** `promptViaSurface` therefore waits on
  the FIFO for the password (independent of when the UI command returns), bailing
  only if the surface reports an error — matching the old `wait $reader` without
  the shell.
- **`doRun` re-raises signals.** A child killed by a signal is distinguished from
  a spawn failure (`spawn` sets `err` only when `ProcessState == nil`) and the
  signal is re-raised to self (`os.Exit(128 + sig)`).
- **Self paths.** `selfPath()` = the real per-OS binary (used to spawn the
  receiver and for the npm-package path check); `cliPath()`/`askpassPath()` are
  the sibling `sido`/`sido-askpass` launchers (used in messages, `SUDO_ASKPASS`,
  and install).

## Security

Passwords never touch disk — tmux, Herdr, and watch use FIFOs (kernel memory).
The receiver writes the password through the FIFO; the approver feeds it over
stdin (never argv), so it never appears in `ps`. The prompt file holds the
displayed command and prompt, never the password.

## Developer commands

```bash
cd go-src && go vet ./...           # vet
cd go-src && go test ./...          # unit tests
bash scripts/build-go.sh            # cross-build dist/{sido,sido-askpass,sido-mac,sido-linux}
npm test                            # go test via package.json script
```

Code style: standard Go conventions (`gofmt`, `camelCase`, exported identifiers
capitalized). Dependencies are `golang.org/x/term` and `golang.org/x/sys` only
(pure Go, so cross-compilation needs no cgo toolchain). `dist/` is gitignored and
rebuilt by `prepack` before `npm publish`.

## Key details agents miss

- Backward compatibility is **not** a default requirement. Prefer polishing and
  converging on the best API over preserving legacy flags or behavior. Only add
  compatibility paths when explicitly requested.
- The TS/Node implementation has been **removed**; Go is now the sole
  implementation. There is no `src/`, no `tsc`, no `prettier`.
- `.node-version` is kept only because CI publishes to npm (which needs the npm
  CLI / Node) — the tool itself does not require Node.
- `sido-askpass` is a shell-launcher alias for `sido askpass`; the hidden
  `_inner_prompt_receiver` is a **top-level** subcommand, not under `askpass`.
- tmux's `display-popup -E` still runs one fixed shell string (tmux's contract);
  it is built from trusted env vars (`$SIDO_BIN`, `$SIDO_PROMPT`, …), never user
  input. That is the only shell involved anywhere, and it is tmux's, not ours.
- Manual e2e: put fakes on `PATH` (see `go-src/test-fakes/`), unset
  `DISPLAY`/`WAYLAND_DISPLAY` so failures don't fall through to a GUI dialog, and
  wrap calls in `timeout` so a regression fails fast instead of hanging.
