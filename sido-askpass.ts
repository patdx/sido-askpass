#!/usr/bin/env node

import { spawnSync } from 'node:child_process'
import {
  existsSync,
  readFileSync,
  writeFileSync,
  unlinkSync,
  chmodSync,
} from 'node:fs'
import { tmpdir, userInfo, hostname, homedir } from 'node:os'
import { join, resolve } from 'node:path'
import { randomUUID } from 'node:crypto'
import { parseArgs } from 'node:util'

const self_path = resolve(process.argv[1]!)
const command = process.argv[2]

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
    strict: false,
  })
  const scope = values.user ? '--user' : values.system ? '--system' : undefined
  do_status(scope)
  process.exit(0)
}

// ── askpass mode ───────────────────────────────────────────────────────────

const prompt = (command || `[sudo] password for ${userInfo().username}: `)
  .replace(/%u/g, userInfo().username)
  .replace(/%h/g, hostname())

const isTmux = !!process.env.TMUX
const isHerdr = process.env.HERDR_ENV === '1'
const isMac = process.platform === 'darwin'
const isLinux = process.platform === 'linux'
const has_display = !!(process.env.DISPLAY || process.env.WAYLAND_DISPLAY)
const can_gui = isMac || (isLinux && has_display)

function tmp(): string {
  return join(tmpdir(), `sido-${randomUUID()}`)
}

function mkfifo(p: string): void {
  spawnSync('mkfifo', [p], { stdio: ['pipe', 'pipe', 'pipe'] })
}

function read_stdout(r: {
  status: number | null
  stdout: Buffer | null
}): string {
  if (r.status !== 0 || !r.stdout || r.stdout.length === 0) process.exit(1)
  return r.stdout.toString().replace(/\r?\n$/, '')
}

function main(): void {
  if (isTmux) return void tmux_prompt()
  if (isHerdr) return void herdr_prompt()
  if (can_gui) return void gui_prompt()
  tty_prompt()
}

function sudo_conf_path(): string {
  return '/etc/sudo.conf'
}

function user_profile_path(): string {
  return join(homedir(), '.profile')
}

function do_install(scope: string): void {
  if (scope === '--user') {
    const path = user_profile_path()
    const line = `export SUDO_ASKPASS="${self_path}"`
    let content = existsSync(path) ? readFileSync(path, 'utf-8') + '\n' : ''
    if (/^export SUDO_ASKPASS=/m.test(content)) {
      content = content.replace(/^export SUDO_ASKPASS=.*$/gm, line)
    } else {
      content += `\n# sido\nexport SUDO_ASKPASS="${self_path}"\n`
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
    if (/^Path askpass /m.test(content)) {
      content = content.replace(/^Path askpass .*$/gm, line)
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
    content = content.replace(/^# sido\n/gm, '')
    content = content.replace(/^export SUDO_ASKPASS=.*$/gm, '')
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

// ── tmux ────────────────────────────────────────────────────────────────────
//
// We use a single bash spawnSync combining a background cat on the FIFO
// (reader) and the blocking tmux command-prompt (writer). Shell job control
// (&, wait) keeps both in one sync call. Native fs.readFile on a FIFO
// can't be cancelled mid-open (libuv threadpool blocking op), whereas the
// shell naturally reaps the background cat on cancel.

function tmux_prompt(): void {
  const fifo = tmp()
  mkfifo(fifo)

  const safe_prompt = prompt.replace(/"/g, '\\"')
  const script = [
    `cat "${fifo}" &`,
    `reader=$!`,
    `tmux command-prompt -N -p "${safe_prompt}" "run-shell 'echo \\"%1\\" > \\"${fifo}\\"'"`,
    `wait "$reader" 2>/dev/null`,
  ].join('\n')

  const r = spawnSync('bash', ['-c', script], {
    stdio: ['inherit', 'pipe', 'inherit'],
  })
  try {
    unlinkSync(fifo)
  } catch {}
  process.stdout.write(read_stdout(r))
}

// ── Herdr ───────────────────────────────────────────────────────────────────
// Same bash spawnSync pattern as tmux: background cat on the FIFO + blocking
// herdr pane run. Shell job control (&, wait) keeps coordination in one sync
// call and reaps the background cat on cancel.

function herdr_prompt(): void {
  const fifo = tmp()
  const prompt_file = tmp()
  const script_file = tmp()

  mkfifo(fifo)
  writeFileSync(prompt_file, prompt)
  writeFileSync(
    script_file,
    [
      `#!/usr/bin/env bash`,
      `prompt=$(cat "${prompt_file}")`,
      `read -s -p "$prompt" pw`,
      `echo "$pw" > "${fifo}"`,
    ].join('\n'),
  )
  chmodSync(script_file, 0o755)

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

  const runner_script = [
    `cat "${fifo}" &`,
    `reader=$!`,
    `herdr pane run "${pane_id}" bash "${script_file}"`,
    `wait "$reader" 2>/dev/null`,
  ].join('\n')

  const r = spawnSync('bash', ['-c', runner_script], {
    stdio: ['inherit', 'pipe', 'inherit'],
  })

  try {
    unlinkSync(fifo)
  } catch {}
  try {
    unlinkSync(prompt_file)
  } catch {}
  try {
    unlinkSync(script_file)
  } catch {}
  spawnSync('herdr', ['pane', 'close', pane_id], {
    stdio: ['pipe', 'pipe', 'pipe'],
  })

  process.stdout.write(read_stdout(r))
}

function fallback_herdr(): void {
  console.error('[sido] herdr pane split failed, falling back')
  if (can_gui) return void gui_prompt()
  tty_prompt()
}

// ── GUI (macOS / Linux) ────────────────────────────────────────────────────

function gui_prompt(): void {
  if (isMac) return void mac_gui()
  linux_gui()
}

function mac_gui(): void {
  const safe_prompt = prompt.replace(/"/g, '\\"')
  const r = spawnSync(
    'osascript',
    [
      '-e',
      `display dialog "${safe_prompt}" with hidden answer default answer ""`,
      '-e',
      'text returned of result',
    ],
    { stdio: ['inherit', 'pipe', 'inherit'] },
  )
  if (r.status !== 0) process.exit(1)
  process.stdout.write(r.stdout?.toString().trim() ?? '')
}

function linux_gui(): void {
  for (const prog of [
    ['zenity', '--password', '--title', prompt],
    ['kdialog', '--password', prompt],
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

function tty_prompt(): void {
  const safe_prompt = prompt.replace(/"/g, '\\"')
  const r = spawnSync(
    'bash',
    ['-c', `read -s -p "${safe_prompt}" pw && echo "$pw"`],
    { stdio: ['inherit', 'pipe', 'inherit'] },
  )
  process.stdout.write(read_stdout(r))
}

main()
