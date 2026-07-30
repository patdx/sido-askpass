# sido-askpass

Single-runtime-file `SUDO_ASKPASS` shim with a build step and dependency-free
end-to-end fixtures.

## Runtime

Node.js 24.x — runs `.ts` directly via built-in type stripping. No flags needed.

## File

- `src/sido-askpass.ts` — the entire project; builds to `dist/sido-askpass.js`

## Usage

```bash
export SUDO_ASKPASS=/path/to/sido-askpass   # npm global bin, or dist/sido-askpass.js
sudo -A <command>          # or plain sudo on modern Fedora (auto-falls back when no TTY)
```

## Detection chain

`SIDO_ADAPTER=auto` (the default) uses this chain:

| Context              | Method                                                           |
| -------------------- | ---------------------------------------------------------------- |
| `$TMUX` set          | `tmux display-popup` + Bash `read -s`, FIFO back to parent       |
| `$HERDR_ENV=1`       | `herdr pane split` + `pane run` **bash read**, FIFO back         |
| macOS + GUI          | `osascript` hidden dialog                                        |
| Linux + `$DISPLAY`   | `zenity` → `kdialog`                                             |
| canonical `/dev/tty` | `read -s` on `/dev/tty` (interactive shell; detected via `stty`) |
| else                 | **watch mode**: park request + FIFO, `sido approve` supplies     |

`SIDO_ADAPTER=tmux|herdr|osascript|zenity|kdialog|tty|watch` forces one exact
adapter. Forced adapters fail rather than falling back. The `run` grammar is
`sido-askpass run [--adapter <name>] -- <command> [args...]`; `--` is required.

## Install / uninstall / status

```bash
./src/sido-askpass.ts install --user       # adds export SUDO_ASKPASS to ~/.profile
./src/sido-askpass.ts install --system      # Path askpass in /etc/sudo.conf via sudo tee
./src/sido-askpass.ts uninstall --user      # reverts ~/.profile
./src/sido-askpass.ts uninstall --system    # reverts /etc/sudo.conf
./src/sido-askpass.ts status [--user|--system]
```

`--user` and `--system` are mutually exclusive (enforced via `parseArgs`).

## Watch mode (agent-agnostic fallback)

When no inline surface is safe — a TUI/agent owns the tty in raw mode (detected
via `stty -a`: canonical+echo = interactive shell; otherwise watch), or there is
no tty at all — the shim parks the request and blocks on a FIFO instead of
failing. Supply the password from any second terminal:

```bash
sido-askpass approve   # approve the most recent pending request (one-shot)
sido-askpass watch     # long-lived: approve requests as they arrive (Ctrl-C to exit)
```

The original terminal is hinted to run `<self> approve`. Requests live under
`$XDG_RUNTIME_DIR/sido` (or `~/.cache/sido`), mode 0700; each request is a
`sido-*` dir holding the prompt and a mode-0600 `password` FIFO.

- `SIDO_ADAPTER=watch` — force watch mode.
- `SIDO_WATCH_TIMEOUT=<sec>` — how long the shim waits for an approver before
  giving up (default 120). On timeout sudo fails rather than hanging forever.

This is the recommended path for `ssh` → coding agent → `sudo` without tmux/Herdr,
and the only viable path for Codex (whose TUI cannot render a password prompt).

## Security

Passwords never touch disk — tmux, Herdr, and watch mode all use FIFOs (kernel memory). TTY fallback pipes stdout directly. Watch's approver feeds the password via stdin (`cat > fifo`), never argv, so it never appears in `ps`.

## Developer commands

```bash
pnpm typecheck             # tsc (noEmit is in tsconfig)
pnpm format                # prettier --write .
pnpm format:check          # prettier --check .
pnpm test                   # fake tmux/Herdr commands + real FIFOs (Linux)
```

## Key details agents miss

- Backward compatibility is **not** a default requirement. Prefer polishing and
  converging on the best API over preserving legacy flags or behavior. Only add
  compatibility paths when explicitly requested.
- This is a **Node.js** project. There are no runtime deps.
- `package.json` pins pnpm version (`packageManager`) and has dev deps only (`@types/node`, `amaro`, `prettier`, `typescript`). No runtime deps.
- `scripts/build.ts` strips types with Amaro and writes the executable `dist/sido-askpass.js`; there is no bundle.
- Code style: `snake_case` for all local functions and variables, no semicolons, single quotes, `verbatimModuleSyntax` (type imports must use `import type`).
- tmux and Herdr prompt functions use embedded bash scripts (`spawnSync('bash', ['-c', ...])`) with shell job control (`&`, `wait`) instead of native `fs` on FIFOs — this is intentional: libuv threadpool `open()` on a FIFO cannot be cancelled from JS, while the shell naturally reaps the background `cat` on cancel. The shared FIFO-drain + reap-on-cancel skeleton is built by `fifo_drain_script()`.
- In askpass mode (`argv[2]` is not `install`/`uninstall`/`status`/`watch`/`approve`/`run`), the first positional argument is the sudo prompt — not parsed by `parseArgs`.
- Adapter selection is `SIDO_ADAPTER=auto|tmux|herdr|osascript|zenity|kdialog|tty|watch`; `auto` is the default, while every explicit adapter is exact and must not fall back. `run` accepts `--adapter <name>` before its required `--` command separator.
- Watch mode is the final fallback when there's no usable inline surface. The tty-vs-watch decision uses a **termios heuristic**: `stty -a < /dev/tty` is parsed for `icanon`+`echo` (canonical → interactive shell → inline `read -s`); a raw-mode tty (a TUI/agent owns the screen) or no tty at all → watch. `stty -a` is read-only and never alters the owning app's terminal state. `SIDO_ADAPTER=watch` forces watch and bypasses detection.
- Watch parks each request under `$XDG_RUNTIME_DIR/sido` (or `~/.cache/sido`) as a `sido-*` dir (mode 0700) with a mode-0600 `password` FIFO. The shim blocks on a child `cat` of the FIFO with a `spawnSync` timeout (`SIDO_WATCH_TIMEOUT`, default 120s) — killable, unlike a libuv FIFO `open()`. The approver (`approve`/`watch`) writes the password via stdin (`cat > fifo`), never argv.
