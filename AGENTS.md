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

| Context            | Method                                                     |
| ------------------ | ---------------------------------------------------------- |
| `$TMUX` set        | `tmux display-popup` + Bash `read -s`, FIFO back to parent |
| `$HERDR_ENV=1`     | `herdr pane split` + `pane run` **bash read**, FIFO back   |
| macOS + GUI        | `osascript` hidden dialog                                  |
| Linux + `$DISPLAY` | `zenity` → `kdialog`                                       |
| else               | `read -s` on `/dev/tty`                                    |

## Install / uninstall / status

```bash
./src/sido-askpass.ts install --user       # adds export SUDO_ASKPASS to ~/.profile
./src/sido-askpass.ts install --system      # Path askpass in /etc/sudo.conf via sudo tee
./src/sido-askpass.ts uninstall --user      # reverts ~/.profile
./src/sido-askpass.ts uninstall --system    # reverts /etc/sudo.conf
./src/sido-askpass.ts status [--user|--system]
```

`--user` and `--system` are mutually exclusive (enforced via `parseArgs`).

## Security

Passwords never touch disk — tmux and Herdr use FIFOs (kernel memory). TTY fallback pipes stdout directly.

## Developer commands

```bash
pnpm typecheck             # tsc (noEmit is in tsconfig)
pnpm format                # prettier --write .
pnpm format:check          # prettier --check .
pnpm test                   # fake tmux/Herdr commands + real FIFOs (Linux)
```

## Key details agents miss

- This is a **Node.js** project. There are no runtime deps.
- `package.json` pins pnpm version (`packageManager`) and has dev deps only (`@types/node`, `amaro`, `prettier`, `typescript`). No runtime deps.
- `scripts/build.ts` strips types with Amaro and writes the executable `dist/sido-askpass.js`; there is no bundle.
- Code style: `snake_case` for all local functions and variables, no semicolons, single quotes, `verbatimModuleSyntax` (type imports must use `import type`).
- tmux and Herdr prompt functions use embedded bash scripts (`spawnSync('bash', ['-c', ...])`) with shell job control (`&`, `wait`) instead of native `fs` on FIFOs — this is intentional: libuv threadpool `open()` on a FIFO cannot be cancelled from JS, while the shell naturally reaps the background `cat` on cancel. The shared FIFO-drain + reap-on-cancel skeleton is built by `fifo_drain_script()`.
- In askpass mode (`argv[2]` is not `install`/`uninstall`/`status`), the first positional argument is the sudo prompt — not parsed by `parseArgs`.
