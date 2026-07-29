# sido-askpass

Single-file `SUDO_ASKPASS` shim. No build, no tests.

## Runtime

Node.js 24.x — runs `.ts` directly via built-in type stripping. No flags needed.

## File

- `sido-askpass.ts` — the entire project (368 lines)

## Usage

```bash
export SUDO_ASKPASS=/path/to/sido-askpass.ts
sudo -A <command>          # or plain sudo on modern Fedora (auto-falls back when no TTY)
```

## Detection chain

| Context            | Method                                                        |
| ------------------ | ------------------------------------------------------------- |
| `$TMUX` set        | `tmux command-prompt -N` (secret prompt, FIFO back to parent) |
| `$HERDR_ENV=1`     | `herdr pane split` + `pane run` **bash read**, FIFO back      |
| macOS + GUI        | `osascript` hidden dialog                                     |
| Linux + `$DISPLAY` | `zenity` → `kdialog`                                          |
| else               | `read -s` on `/dev/tty`                                       |

## Install / uninstall / status

```bash
./sido-askpass.ts install --user       # adds export SUDO_ASKPASS to ~/.profile
./sido-askpass.ts install --system      # Path askpass in /etc/sudo.conf via sudo tee
./sido-askpass.ts uninstall --user      # reverts ~/.profile
./sido-askpass.ts uninstall --system    # reverts /etc/sudo.conf
./sido-askpass.ts status [--user|--system]
```

`--user` and `--system` are mutually exclusive (enforced via `parseArgs`).

## Security

Passwords never touch disk — tmux and Herdr use FIFOs (kernel memory). TTY fallback pipes stdout directly.

## Developer commands

```bash
pnpm typecheck             # tsc (noEmit is in tsconfig)
pnpm format                # prettier --write .
pnpm format:check          # prettier --check .
```

## Key details agents miss

- This is **not** a Bun project despite the `.ts` extension — uses Node.js `child_process.spawnSync`, not `Bun.spawnSync`.
- `package.json` pins pnpm version (`packageManager`) and has dev deps only (`@types/node`, `prettier`, `typescript`). No runtime deps, no build step.
- Code style: `snake_case` for all local functions and variables, no semicolons, single quotes, `verbatimModuleSyntax` (type imports must use `import type`).
- tmux and Herdr prompt functions use embedded bash scripts (`spawnSync('bash', ['-c', ...])`) with shell job control (`&`, `wait`) instead of native `fs` on FIFOs — this is intentional: libuv threadpool `open()` on a FIFO cannot be cancelled from JS, while the shell naturally reaps the background `cat` on cancel.
- In askpass mode (`argv[2]` is not `install`/`uninstall`/`status`), the first positional argument is the sudo prompt — not parsed by `parseArgs`.
