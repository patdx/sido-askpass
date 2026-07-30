#!/usr/bin/env node

// Uses type stripping (Amaro) — line numbers in dist/sido-askpass.js match
// the .ts source exactly, so error stacks point back to the right line.

import { spawnSync } from 'node:child_process'
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  rmSync,
  statSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir, userInfo, hostname, homedir } from 'node:os'
import { join, resolve } from 'node:path'
import { parseArgs } from 'node:util'
import package_json from '../package.json' with { type: 'json' }

const self_path = resolve(process.argv[1]!)
const command = process.argv[2]

if (command === '_inner_prompt_receiver') {
  const prompt_file = process.argv[3]
  const fifo = process.argv[4]
  if (!prompt_file || !fifo) process.exit(1)
  inner_prompt_receiver(prompt_file, fifo)
  process.exit(0)
}

// ── help / version ──────────────────────────────────────────────────────────

if (command === '--help' || command === '-h' || command === 'help') {
  console.log(`sido-askpass — SUDO_ASKPASS shim for headless agent environments

Name:    sido-askpass
Version: ${package_json.version}
Author:  patdx
Repo:    https://github.com/patdx/sido-askpass

Usage:
  sido-askpass                        askpass mode (invoked by sudo)
  sido-askpass install --user|--system
  sido-askpass uninstall --user|--system
  sido-askpass status [--user|--system]
  sido-askpass run <command> [args...]  run <command> with SUDO_ASKPASS set
  sido-askpass watch                     wait for and approve pending requests
  sido-askpass approve                   approve the most recent pending request
  sido-askpass --help
  sido-askpass --version

Env:
  SIDO_WATCH=1             force watch mode (skip inline TTY read)
  SIDO_WATCH_TIMEOUT=<sec> approver wait timeout (default 120)`)
  process.exit(0)
}

if (command === '--version' || command === '-V' || command === 'version') {
  console.log(package_json.version)
  process.exit(0)
}

// ── install / uninstall / status ────────────────────────────────────────────

if (command === 'install' || command === 'uninstall') {
  const { values } = parseArgs({
    args: process.argv.slice(3),
    options: {
      user: { type: 'boolean' },
      system: { type: 'boolean' },
    },
  })
  if ((values.user && values.system) || (!values.user && !values.system)) {
    console.error(`[sido] usage: ${self_path} ${command} --user|--system`)
    process.exit(1)
  }
  const scope = values.user ? '--user' : '--system'
  if (command === 'install') do_install(scope)
  else do_uninstall(scope)
  process.exit(0)
}

if (command === 'status') {
  const { values } = parseArgs({
    args: process.argv.slice(3),
    options: {
      user: { type: 'boolean' },
      system: { type: 'boolean' },
    },
  })
  if (values.user && values.system) {
    console.error(`[sido] usage: ${self_path} status [--user|--system]`)
    process.exit(1)
  }
  const scope = values.user ? '--user' : values.system ? '--system' : undefined
  do_status(scope)
  process.exit(0)
}

if (command === 'run') {
  if (!process.argv[3]) {
    console.error(`[sido] usage: ${self_path} run <command> [args...]`)
    process.exit(1)
  }
  do_run(process.argv[3], process.argv.slice(4))
}

if (command === 'watch') {
  do_watch()
  process.exit(0)
}

if (command === 'approve') {
  do_approve()
  process.exit(0)
}

// ── askpass mode ───────────────────────────────────────────────────────────

const prompt = (command || `[sudo] password for ${userInfo().username}: `)
  .replace(/%u/g, userInfo().username)
  .replace(/%h/g, hostname())
const requesting_command = get_requesting_command()
const display_prompt = requesting_command
  ? `Command: ${requesting_command}\n${prompt}`
  : prompt

const is_tmux = !!process.env.TMUX
const is_herdr = process.env.HERDR_ENV === '1'
const is_mac = process.platform === 'darwin'
const is_linux = process.platform === 'linux'
const has_display = !!(process.env.DISPLAY || process.env.WAYLAND_DISPLAY)
const can_gui = is_mac || (is_linux && has_display)

interface PromptResources {
  dir: string
  fifo: string
  prompt_file: string
}

function create_prompt_resources(base: string = tmpdir()): PromptResources {
  const dir = mkdtempSync(join(base, 'sido-'))
  const fifo = join(dir, 'password')
  const result = spawnSync('mkfifo', ['-m', '600', fifo], {
    stdio: ['pipe', 'pipe', 'pipe'],
  })
  if (result.error || result.status !== 0) {
    rmSync(dir, { recursive: true, force: true })
    const detail =
      result.error?.message ||
      result.stderr?.toString().trim() ||
      'unknown error'
    throw new Error(`mkfifo failed: ${detail}`)
  }
  const resources = {
    dir,
    fifo,
    prompt_file: join(dir, 'prompt'),
  }
  try {
    writeFileSync(resources.prompt_file, display_prompt, { mode: 0o600 })
    return resources
  } catch (error) {
    rmSync(dir, { recursive: true, force: true })
    throw error
  }
}

function remove_prompt_resources(resources: PromptResources): void {
  rmSync(resources.dir, { recursive: true, force: true })
}

function read_stdout(r: {
  status: number | null
  stdout: Buffer | null
}): string {
  if (r.status !== 0 || !r.stdout || r.stdout.length === 0) process.exit(1)
  return r.stdout.toString().replace(/\r?\n$/, '')
}

function read_secret(prompt: string): string | null {
  const r = spawnSync(
    'bash',
    [
      '-c',
      'read -s -p "$1" pw < /dev/tty && printf \'%s\\n\' "$pw"',
      'sido',
      prompt,
    ],
    { stdio: ['ignore', 'pipe', 'inherit'] },
  )
  if (r.status !== 0 || !r.stdout || r.stdout.length === 0) return null
  return r.stdout.toString().replace(/\r?\n$/, '')
}

function inner_prompt_receiver(prompt_file: string, fifo: string): void {
  const prompt = readFileSync(prompt_file, 'utf8')
  const pw = read_secret(prompt)
  if (pw == null) process.exit(1)
  writeFileSync(fifo, `${pw}\n`)
}

function get_requesting_command(): string | undefined {
  try {
    let value: string
    if (process.platform === 'linux') {
      value = readFileSync(`/proc/${process.ppid}/cmdline`, 'utf8').replace(
        /\0/g,
        ' ',
      )
    } else {
      const r = spawnSync(
        'ps',
        ['-o', 'command=', '-p', String(process.ppid)],
        { stdio: ['pipe', 'pipe', 'pipe'] },
      )
      if (r.status !== 0) return
      value = r.stdout?.toString() ?? ''
    }

    value = value
      .replace(/[\u0000-\u001f\u007f-\u009f]/g, ' ')
      .replace(/\s+/g, ' ')
      .trim()
    return value || undefined
  } catch {
    return
  }
}

function main(): void {
  if (is_tmux) return void tmux_prompt()
  if (is_herdr) return void herdr_prompt()
  if (can_gui) return void gui_prompt()
  if (process.env.SIDO_WATCH !== '1' && interactive_shell_tty() && tty_prompt())
    return
  watch_prompt()
}

// Returns true when /dev/tty is our controlling terminal and it is in canonical
// mode (a normal interactive shell owns it) — i.e. safe to read a password
// inline. A raw-mode tty means a TUI/agent owns the screen, and no tty at all
// means headless; both fall through to watch mode. `stty -a` is read-only, so
// it never disturbs the owning application's terminal state.
function interactive_shell_tty(): boolean {
  const r = spawnSync('bash', ['-c', 'stty -a < /dev/tty 2>/dev/null'], {
    stdio: ['ignore', 'pipe', 'inherit'],
  })
  if (r.status !== 0) return false
  const out = r.stdout?.toString() ?? ''
  // Linux prints `icanon`/`echo`; BSD/macOS prints `canon`/`echo`. Negated
  // flags are prefixed with `-`, which the lookbehind excludes.
  return /(?<!-)(?:i?canon)\b/.test(out) && /(?<!-)echo\b/.test(out)
}

function sudo_conf_path(): string {
  return '/etc/sudo.conf'
}

function user_profile_path(): string {
  return join(homedir(), '.profile')
}

function do_run(command: string, args: string[]): never {
  const proc = spawnSync(command, args, {
    env: { ...process.env, SUDO_ASKPASS: self_path },
    stdio: 'inherit',
  })
  if (proc.error) {
    console.error(`[sido] failed to run ${command}: ${proc.error.message}`)
    process.exit(1)
  }
  if (proc.signal) {
    process.kill(process.pid, proc.signal)
  }
  process.exit(proc.status ?? 1)
}

function do_install(scope: string): void {
  if (scope === '--user') {
    const path = user_profile_path()
    const line = `export SUDO_ASKPASS="${self_path}"`
    let content = existsSync(path) ? readFileSync(path, 'utf-8') + '\n' : ''
    const managed_pattern = /^# sido\nexport SUDO_ASKPASS=.*$/gm
    if (managed_pattern.test(content)) {
      content = content.replace(managed_pattern, `# sido\n${line}`)
    } else if (/^export SUDO_ASKPASS=.*$/m.test(content)) {
      const old_values = [
        ...content.matchAll(/^export SUDO_ASKPASS=(.*)$/gm),
      ].map((match) => match[1])
      for (const old_value of old_values) {
        console.error(
          `[sido] warning: replacing SUDO_ASKPASS=${old_value} with "${self_path}"`,
        )
      }
      content = content.replace(/^export SUDO_ASKPASS=.*$/gm, `# sido\n${line}`)
    } else {
      content += `\n# sido\n${line}\n`
    }
    writeFileSync(path, content)
    console.error(`[sido] installed to ${path}`)
    console.error(`[sido] run: source ${path}`)
  } else {
    const path = sudo_conf_path()
    const line = `Path askpass ${self_path}`
    let content = ''
    if (existsSync(path)) {
      const r = spawnSync('cat', [path], { stdio: ['pipe', 'pipe', 'pipe'] })
      content = r.stdout?.toString() ?? ''
    }
    const managed_pattern = /^# sido\nPath askpass .*$/gm
    if (managed_pattern.test(content)) {
      content = content.replace(managed_pattern, `# sido\n${line}`)
    } else if (/^Path askpass /m.test(content)) {
      const old_values = [...content.matchAll(/^Path askpass (.*)$/gm)].map(
        (match) => match[1],
      )
      for (const old_value of old_values) {
        console.error(
          `[sido] warning: replacing Path askpass ${old_value} with "${self_path}"`,
        )
      }
      content = content.replace(/^Path askpass .*$/gm, `# sido\n${line}`)
    } else {
      content += `\n# sido\n${line}\n`
    }
    const proc = spawnSync('sudo', ['tee', path], {
      input: content,
      stdio: ['pipe', 'inherit', 'inherit'],
    })
    if (proc.status === 0) {
      console.error(`[sido] installed to ${path}`)
    } else {
      console.error('[sido] install failed — do you have sudo?')
      process.exit(1)
    }
  }
}

function do_uninstall(scope: string): void {
  if (scope === '--user') {
    const path = user_profile_path()
    if (!existsSync(path)) {
      console.error('[sido] nothing to uninstall')
      process.exit(1)
    }
    let content = readFileSync(path, 'utf-8')
    content = content.replace(/^# sido\nexport SUDO_ASKPASS=.*$\n?/gm, '')
    content = content.replace(/\n{3,}/g, '\n\n').trim()
    writeFileSync(path, content + '\n')
    console.error(`[sido] removed from ${path}`)
  } else {
    const path = sudo_conf_path()
    if (!existsSync(path)) {
      console.error('[sido] nothing to uninstall')
      process.exit(1)
    }
    const r = spawnSync('cat', [path], { stdio: ['pipe', 'pipe', 'pipe'] })
    let content = r.stdout?.toString() ?? ''
    content = content.replace(/^# sido\n/gm, '')
    content = content.replace(/^Path askpass .*$/gm, '')
    content = content.replace(/\n{3,}/g, '\n\n').trim()
    const proc = spawnSync('sudo', ['tee', path], {
      input: content + '\n',
      stdio: ['pipe', 'inherit', 'inherit'],
    })
    if (proc.status === 0) {
      console.error(`[sido] removed from ${path}`)
    } else {
      console.error('[sido] uninstall failed')
      process.exit(1)
    }
  }
}

function do_status(scope?: string): void {
  if (!scope || scope === '--system') {
    const sys_path = sudo_conf_path()
    if (existsSync(sys_path)) {
      const r = spawnSync('cat', [sys_path], {
        stdio: ['pipe', 'pipe', 'pipe'],
      })
      const m = r.stdout?.toString().match(/^Path askpass (.*)$/m)
      console.error(
        m
          ? `[sido] system askpass: ${m[1]}`
          : `[sido] no system askpass in ${sys_path}`,
      )
    } else {
      console.error(`[sido] ${sys_path} does not exist`)
    }
  }

  if (!scope || scope === '--user') {
    const user_path = user_profile_path()
    if (existsSync(user_path)) {
      const content = readFileSync(user_path, 'utf-8')
      const m = content.match(/^export SUDO_ASKPASS=(.*)$/m)
      console.error(
        m
          ? `[sido] user askpass in ~/.profile: ${m[1]}`
          : `[sido] no SUDO_ASKPASS in ~/.profile`,
      )
    }
  }

  const env_active = process.env.SUDO_ASKPASS
  if (env_active) {
    console.error(`[sido] active (env): ${env_active}`)
  }
}

// Background `cat` drains the FIFO back to the parent while a blocking command
// (tmux popup / herdr pane run) fills it. Shell job control reaps the reader on
// cancel — see AGENTS.md: native fs open() on a FIFO cannot be cancelled from JS,
// but the shell naturally reaps the background cat on cancel.
function fifo_drain_script(
  fifo_ref: string,
  block_cmd: string,
  status_var: string,
): string {
  return [
    `cat "${fifo_ref}" &`,
    `reader=$!`,
    block_cmd,
    `${status_var}=$?`,
    `if [ "$${status_var}" -ne 0 ]; then`,
    `  kill "$reader" 2>/dev/null`,
    `  wait "$reader" 2>/dev/null`,
    `  exit "$${status_var}"`,
    `fi`,
    `wait "$reader" 2>/dev/null`,
  ].join('\n')
}

// ── tmux ────────────────────────────────────────────────────────────────────
//
// tmux command-prompt cannot hide input (-N means numeric-only, not secret).
// Use a popup running bash read -s instead. The password still travels only
// through a FIFO in a private temporary directory.

function tmux_prompt(): void {
  const check = spawnSync('tmux', ['display-message', '-p', '#S'], {
    stdio: ['pipe', 'pipe', 'pipe'],
  })
  if (check.status !== 0) {
    const detail = check.stderr?.toString().trim()
    if (detail) console.error(detail)
    console.error(
      '[sido] tmux access denied; retry the sudo command with escalated permissions',
    )
    process.exit(1)
  }

  let resources: PromptResources
  try {
    resources = create_prompt_resources()
  } catch (error) {
    console.error(`[sido] ${String(error)}`)
    process.exit(1)
  }

  const block_cmd = [
    `tmux display-popup -E -w 60% -h 5 \\`,
    `  -e "SIDO_ASKPASS=$2" -e "SIDO_PROMPT=$3" -e "SIDO_FIFO=$1" \\`,
    `  '"$SIDO_ASKPASS" _inner_prompt_receiver "$SIDO_PROMPT" "$SIDO_FIFO"'`,
  ].join('\n')
  const script = fifo_drain_script('$1', block_cmd, 'popup_status')

  let r
  try {
    r = spawnSync(
      'bash',
      ['-c', script, 'sido', resources.fifo, self_path, resources.prompt_file],
      { stdio: ['inherit', 'pipe', 'inherit'] },
    )
  } finally {
    remove_prompt_resources(resources)
  }
  process.stdout.write(read_stdout(r))
}

// ── Herdr ───────────────────────────────────────────────────────────────────
// Same bash spawnSync pattern as tmux: background cat on the FIFO + blocking
// herdr pane run. Shell job control (&, wait) keeps coordination in one sync
// call and reaps the background cat on cancel.

function herdr_prompt(): void {
  let direction = 'right'
  try {
    const layout = spawnSync('herdr', ['pane', 'layout', '--current'], {
      stdio: ['inherit', 'pipe', 'pipe'],
    })
    if (layout.status === 0) {
      const info = JSON.parse(layout.stdout?.toString() ?? '{}')
      if ((info.result?.pane?.width ?? 0) <= 150) direction = 'down'
    }
  } catch {}

  console.error(
    `[sido] password requested — see new pane (${direction === 'right' ? 'right' : 'bottom'})`,
  )

  const split = spawnSync(
    'herdr',
    ['pane', 'split', '--current', '--direction', direction, '--cwd', '/'],
    { stdio: ['inherit', 'pipe', 'pipe'] },
  )
  if (split.status !== 0) return void fallback_herdr()
  const pane_id = JSON.parse(split.stdout?.toString() ?? '{}').result?.pane
    ?.pane_id
  if (!pane_id) return void fallback_herdr()

  let resources: PromptResources
  try {
    resources = create_prompt_resources()
  } catch (error) {
    spawnSync('herdr', ['pane', 'close', pane_id], {
      stdio: ['pipe', 'pipe', 'pipe'],
    })
    console.error(`[sido] ${String(error)}`)
    process.exit(1)
  }

  const block_cmd = `herdr pane run "$1" "$3" _inner_prompt_receiver "$4" "$2"`
  const runner_script = fifo_drain_script('$2', block_cmd, 'run_status')

  let r
  try {
    r = spawnSync(
      'bash',
      [
        '-c',
        runner_script,
        'sido',
        pane_id,
        resources.fifo,
        self_path,
        resources.prompt_file,
      ],
      { stdio: ['inherit', 'pipe', 'inherit'] },
    )
  } finally {
    remove_prompt_resources(resources)
    spawnSync('herdr', ['pane', 'close', pane_id], {
      stdio: ['pipe', 'pipe', 'pipe'],
    })
  }
  process.stdout.write(read_stdout(r))
}

function fallback_herdr(): void {
  console.error('[sido] herdr pane split failed, falling back')
  if (can_gui) return void gui_prompt()
  if (tty_prompt()) return
  process.exit(1)
}

// ── GUI (macOS / Linux) ────────────────────────────────────────────────────

function gui_prompt(): void {
  if (is_mac) return void mac_gui()
  linux_gui()
}

function mac_gui(): void {
  const dialog_title = 'Administrator Authentication'
  const r = spawnSync(
    'osascript',
    [
      '-e',
      'on run argv',
      '-e',
      'display dialog (item 1 of argv) with title (item 2 of argv) with icon caution with hidden answer default answer "" buttons {"Cancel", "Authenticate"} default button "Authenticate" cancel button "Cancel"',
      '-e',
      'text returned of result',
      '-e',
      'end run',
      display_prompt,
      dialog_title,
    ],
    { stdio: ['inherit', 'pipe', 'inherit'] },
  )
  if (r.status !== 0) process.exit(1)
  process.stdout.write(r.stdout?.toString().replace(/\r?\n$/, '') ?? '')
}

function linux_gui(): void {
  for (const prog of [
    ['zenity', '--password', '--title', display_prompt],
    ['kdialog', '--password', display_prompt],
  ]) {
    const [cmd, ...args] = prog as [string, ...string[]]
    const r = spawnSync(cmd, args, { stdio: ['inherit', 'pipe', 'inherit'] })
    if (r.status === 0) {
      process.stdout.write(r.stdout?.toString().replace(/\n$/, '') ?? '')
      return
    }
  }
  console.error('[sido] no GUI dialog found')
  tty_prompt()
}

// ── TTY fallback ───────────────────────────────────────────────────────────

function tty_prompt(): boolean {
  const pw = read_secret(display_prompt)
  if (pw == null) return false
  process.stdout.write(pw)
  return true
}

// ── Watch mode (fallback for raw/headless ttys) ────────────────────────────
//
// When no inline surface is safe (a TUI/agent owns the tty in raw mode, or
// there is no tty at all), park the request in a user-private dir and block on
// a FIFO. A second terminal runs `sido approve` / `sido watch` to supply the
// password, which is written through the FIFO — never to disk, never to the
// agent transcript. This is agent-agnostic: it works for opencode, Codex, Pi,
// plain SSH, and headless scripts alike.

function sido_dir(): string {
  const base = process.env.XDG_RUNTIME_DIR || join(homedir(), '.cache')
  const dir = join(base, 'sido')
  if (!existsSync(dir)) mkdirSync(dir, { recursive: true, mode: 0o700 })
  return dir
}

function watch_timeout_ms(): number {
  const raw = Number(process.env.SIDO_WATCH_TIMEOUT)
  if (Number.isFinite(raw) && raw > 0) return raw * 1000
  return 120_000
}

function watch_prompt(): void {
  let resources: PromptResources
  try {
    resources = create_prompt_resources(sido_dir())
  } catch (error) {
    console.error(`[sido] ${String(error)}`)
    process.exit(1)
  }

  const timeout_ms = watch_timeout_ms()
  console.error(
    `[sido] password requested${requesting_command ? ` for: ${requesting_command}` : ''}`,
  )
  console.error(
    `[sido] from another terminal run: ${self_path} approve  (waiting up to ${Math.round(timeout_ms / 1000)}s)`,
  )

  // External `cat` opens the FIFO read-only and blocks until a writer (the
  // approver) connects. spawnSync's timeout kills it if nobody approves in time
  // — avoiding libuv's uncancellable threadpool open() on a FIFO.
  let r
  try {
    r = spawnSync('bash', ['-c', 'cat "$1"', 'sido', resources.fifo], {
      stdio: ['ignore', 'pipe', 'inherit'],
      timeout: timeout_ms,
      killSignal: 'SIGTERM',
    })
  } finally {
    remove_prompt_resources(resources)
  }

  if (r!.signal === 'SIGTERM' || r!.status === null) {
    console.error('[sido] timed out waiting for approver')
    process.exit(1)
  }
  process.stdout.write(read_stdout(r!))
}

function do_watch(): void {
  console.error(
    `[sido] watching ${sido_dir()} for password requests (Ctrl-C to exit)`,
  )
  while (true) {
    const req = newest_request()
    if (req) handle_request(req)
    else spawnSync('sleep', ['1'], { stdio: ['ignore', 'ignore', 'inherit'] })
  }
}

function do_approve(): void {
  const req = newest_request()
  if (!req) {
    console.error('[sido] no pending password requests')
    process.exit(0)
  }
  handle_request(req)
}

function newest_request(): string | undefined {
  let entries: string[]
  try {
    entries = readdirSync(sido_dir())
  } catch {
    return undefined
  }
  let newest: string | undefined
  let newest_mtime = -Infinity
  for (const entry of entries) {
    if (!entry.startsWith('sido-')) continue
    const path = join(sido_dir(), entry)
    const m = statSync(path).mtimeMs
    if (m > newest_mtime) {
      newest_mtime = m
      newest = path
    }
  }
  return newest
}

function handle_request(req_dir: string): void {
  let prompt = '[sudo] password: '
  try {
    prompt = readFileSync(join(req_dir, 'prompt'), 'utf8')
  } catch {}

  const password = read_secret(prompt)
  if (password == null) {
    console.error('[sido] no password entered')
    process.exit(1)
  }

  // Feed the password via stdin, not argv, so it never appears in `ps`. The
  // redirection opens the FIFO write-only and blocks until the waiting shim's
  // reader connects; the timeout covers a shim that already exited/timed out.
  const r = spawnSync(
    'bash',
    ['-c', 'cat > "$1"', 'sido', join(req_dir, 'password')],
    {
      input: `${password}\n`,
      stdio: ['pipe', 'pipe', 'inherit'],
      timeout: 5_000,
    },
  )
  console.error(
    r.status === 0 ? '[sido] password sent' : '[sido] request expired',
  )
  rmSync(req_dir, { recursive: true, force: true })
}

main()
