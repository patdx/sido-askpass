// Uses type stripping (Amaro) so generated code stays close to this source.

import { spawnSync } from 'node:child_process'
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  realpathSync,
  rmSync,
  statSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir, userInfo, hostname, homedir } from 'node:os'
import { basename, dirname, extname, join, resolve, sep } from 'node:path'
import { parseArgs } from 'node:util'
import package_json from '../package.json' with { type: 'json' }

const self_path = resolve(process.argv[1]!)
const extension = extname(self_path)
const cli_path = join(dirname(self_path), `sido${extension}`)
const askpass_path = join(dirname(self_path), `sido-askpass${extension}`)
const command = process.argv[2]
const adapters = [
  'auto',
  'tmux',
  'herdr',
  'osascript',
  'zenity',
  'kdialog',
  'tty',
  'watch',
] as const
type Adapter = (typeof adapters)[number]

// ── help / version ──────────────────────────────────────────────────────────

export function manager_main(): void {
  if (command === '--help' || command === '-h' || command === 'help') {
    console.log(`sido-askpass — SUDO_ASKPASS shim for headless agent environments

Name:    sido-askpass
Version: ${package_json.version}
Author:  patdx
Repo:    https://github.com/patdx/sido-askpass

Usage:
  sido install [--user|--system]
  sido uninstall --user|--system
  sido upgrade                       upgrade an npm install and refresh its config
  sido status [--user|--system]
  sido run [--adapter <name>] -- <command> [args...]
                                     run a command with SUDO_ASKPASS set
  sido watch                         wait for and approve pending requests
  sido approve                       approve the most recent pending request
  sido --help
  sido --version

Adapters:
  auto       detect the best adapter (default)
  tmux       hidden prompt in a tmux popup
  herdr      hidden prompt in a temporary Herdr pane
  osascript  native macOS password dialog
  zenity     Zenity password dialog
  kdialog    KDE password dialog
  tty        hidden prompt on /dev/tty
  watch      approve from another terminal with "sido approve"

Env:
  SIDO_ADAPTER=<name>      select an adapter for askpass mode
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
    if (
      (values.user && values.system) ||
      (command === 'uninstall' && !values.user && !values.system)
    ) {
      const scope_usage =
        command === 'install' ? '[--user|--system]' : '--user|--system'
      console.error(`[sido] usage: ${self_path} ${command} ${scope_usage}`)
      process.exit(1)
    }
    const scope = values.user
      ? '--user'
      : values.system
        ? '--system'
        : undefined
    if (command === 'install') do_install(scope)
    else do_uninstall(scope!)
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
    const scope = values.user
      ? '--user'
      : values.system
        ? '--system'
        : undefined
    do_status(scope)
    process.exit(0)
  }

  if (command === 'upgrade') {
    do_upgrade()
  }

  if (command === 'run') {
    const run = parse_run_args(process.argv.slice(3))
    do_run(run.command, run.args, run.adapter)
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

  console.error(`[sido] unknown command: ${command ?? '(none)'}`)
  console.error(`[sido] run "${self_path} --help" for usage`)
  process.exit(1)
}

export function askpass_main(): void {
  if (command === '_inner_prompt_receiver') {
    const prompt_file = process.argv[3]
    const fifo = process.argv[4]
    if (!prompt_file || !fifo) process.exit(1)
    inner_prompt_receiver(prompt_file, fifo)
    return
  }
  prompt = (command || `[sudo] password for ${userInfo().username}: `)
    .replace(/%u/g, userInfo().username)
    .replace(/%h/g, hostname())
  requesting_command = get_requesting_command()
  display_prompt = requesting_command
    ? `Command: ${requesting_command}\n${prompt}`
    : prompt
  is_tmux = !!process.env.TMUX
  is_herdr = process.env.HERDR_ENV === '1'
  is_mac = process.platform === 'darwin'
  is_linux = process.platform === 'linux'
  has_display = !!(process.env.DISPLAY || process.env.WAYLAND_DISPLAY)
  can_gui = is_mac || (is_linux && has_display)
  selected_adapter = parse_adapter(process.env.SIDO_ADAPTER ?? 'auto')
  main()
}

let prompt = ''
let requesting_command: string | undefined
let display_prompt = ''
let is_tmux = false
let is_herdr = false
let is_mac = false
let is_linux = false
let has_display = false
let can_gui = false
let selected_adapter: Adapter = 'auto'

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

function read_stdout(result: {
  status: number | null
  stdout: Buffer | null
}): string {
  if (result.status !== 0 || !result.stdout || result.stdout.length === 0)
    process.exit(1)
  return result.stdout.toString().replace(/\r?\n$/, '')
}

function read_secret(prompt: string): string | null {
  const result = spawnSync(
    'bash',
    [
      '-c',
      'read -s -p "$1" pw < /dev/tty && printf \'%s\\n\' "$pw"',
      'sido',
      prompt,
    ],
    { stdio: ['ignore', 'pipe', 'inherit'] },
  )
  if (result.status !== 0 || !result.stdout || result.stdout.length === 0)
    return null
  return result.stdout.toString().replace(/\r?\n$/, '')
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
      const command_result = spawnSync(
        'ps',
        ['-o', 'command=', '-p', String(process.ppid)],
        { stdio: ['pipe', 'pipe', 'pipe'] },
      )
      if (command_result.status !== 0) return
      value = command_result.stdout?.toString() ?? ''
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
  if (selected_adapter !== 'auto') return void adapter_prompt(selected_adapter)
  if (is_tmux) return void tmux_prompt()
  if (is_herdr) return void herdr_prompt(true)
  if (can_gui) return void gui_prompt()
  if (interactive_shell_tty() && tty_prompt()) return
  watch_prompt()
}

function adapter_prompt(adapter: Exclude<Adapter, 'auto'>): void {
  if (adapter === 'tmux') return void tmux_prompt()
  if (adapter === 'herdr') return void herdr_prompt(false)
  if (adapter === 'osascript') {
    if (!is_mac) adapter_unavailable(adapter, 'requires macOS')
    return void mac_gui()
  }
  if (adapter === 'zenity')
    return void exact_gui_prompt('zenity', [
      '--password',
      '--title',
      display_prompt,
    ])
  if (adapter === 'kdialog')
    return void exact_gui_prompt('kdialog', ['--password', display_prompt])
  if (adapter === 'tty') {
    if (!tty_prompt()) adapter_unavailable(adapter, 'could not read /dev/tty')
    return
  }
  watch_prompt()
}

function adapter_unavailable(adapter: Adapter, detail: string): never {
  console.error(`[sido] ${adapter} adapter unavailable: ${detail}`)
  process.exit(1)
}

// Returns true when /dev/tty is our controlling terminal and it is in canonical
// mode (a normal interactive shell owns it) — i.e. safe to read a password
// inline. A raw-mode tty means a TUI/agent owns the screen, and no tty at all
// means headless; both fall through to watch mode. `stty -a` is read-only, so
// it never disturbs the owning application's terminal state.
function interactive_shell_tty(): boolean {
  const stty_result = spawnSync(
    'bash',
    ['-c', 'stty -a < /dev/tty 2>/dev/null'],
    {
      stdio: ['ignore', 'pipe', 'inherit'],
    },
  )
  if (stty_result.status !== 0) return false
  const out = stty_result.stdout?.toString() ?? ''
  // Linux prints `icanon`/`echo`; BSD/macOS prints `canon`/`echo`. Negated
  // flags are prefixed with `-`, which the lookbehind excludes.
  return /(?<!-)(?:i?canon)\b/.test(out) && /(?<!-)echo\b/.test(out)
}

function sudo_conf_path(): string {
  return '/etc/sudo.conf'
}

function user_profile_names(): string[] {
  return [
    '.profile',
    '.bash_profile',
    '.bash_login',
    '.bashrc',
    '.zprofile',
    '.zshenv',
    '.zshrc',
  ]
}

function user_profile_paths(): string[] {
  return user_profile_names().map((name) => join(homedir(), name))
}

function user_profile_path(): string {
  const shell = basename(process.env.SHELL ?? '')
  if (shell === 'zsh') return join(homedir(), '.zshrc')
  if (shell === 'bash') return join(homedir(), '.bashrc')
  return join(homedir(), '.profile')
}

function managed_user_block_pattern(): RegExp {
  return /^# sido start\n[\s\S]*?^# sido end\n?/m
}

function managed_user_install(path: string): string | undefined {
  if (!existsSync(path)) return
  const block = readFileSync(path, 'utf8').match(
    managed_user_block_pattern(),
  )?.[0]
  return block?.match(/^export SUDO_ASKPASS=(.*)$/m)?.[1]
}

function remove_managed_user_install(content: string): string {
  return content
    .replace(managed_user_block_pattern(), '')
    .replace(/\n{3,}/g, '\n\n')
    .trimEnd()
}

function parse_adapter(value: string): Adapter {
  if ((adapters as readonly string[]).includes(value)) return value as Adapter
  console.error(
    `[sido] unknown adapter "${value}"; expected one of: ${adapters.join(', ')}`,
  )
  process.exit(1)
}

function parse_run_args(args: string[]): {
  command: string
  args: string[]
  adapter?: Adapter
} {
  const separator = args.indexOf('--')
  if (separator === -1 || !args[separator + 1]) {
    console.error(
      `[sido] usage: ${self_path} run [--adapter <name>] -- <command> [args...]`,
    )
    process.exit(1)
  }

  let adapter_value: string | undefined
  try {
    adapter_value = parseArgs({
      args: args.slice(0, separator),
      options: { adapter: { type: 'string' } },
    }).values.adapter
  } catch (error) {
    console.error(`[sido] ${String(error)}`)
    process.exit(1)
  }

  return {
    command: args[separator + 1]!,
    args: args.slice(separator + 2),
    ...(adapter_value ? { adapter: parse_adapter(adapter_value) } : {}),
  }
}

function do_run(command: string, args: string[], adapter?: Adapter): never {
  const proc = spawnSync(command, args, {
    env: {
      ...process.env,
      SUDO_ASKPASS: askpass_path,
      ...(adapter ? { SIDO_ADAPTER: adapter } : {}),
    },
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

function compare_semver(left: string, right: string): number | undefined {
  const parse = (value: string) => {
    const match = value.match(
      /^v?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$/,
    )
    if (!match) return
    return {
      core: [Number(match[1]), Number(match[2]), Number(match[3])],
      prerelease: match[4]?.split('.'),
    }
  }
  const a = parse(left)
  const b = parse(right)
  if (!a || !b) return
  for (let i = 0; i < a.core.length; i++) {
    if (a.core[i]! !== b.core[i]!) return a.core[i]! < b.core[i]! ? -1 : 1
  }
  if (!a.prerelease && !b.prerelease) return 0
  if (!a.prerelease) return 1
  if (!b.prerelease) return -1
  const length = Math.max(a.prerelease.length, b.prerelease.length)
  for (let i = 0; i < length; i++) {
    const x = a.prerelease[i]
    const y = b.prerelease[i]
    if (x === undefined) return -1
    if (y === undefined) return 1
    if (x === y) continue
    const x_numeric = /^\d+$/.test(x)
    const y_numeric = /^\d+$/.test(y)
    if (x_numeric && y_numeric) return Number(x) < Number(y) ? -1 : 1
    if (x_numeric !== y_numeric) return x_numeric ? -1 : 1
    return x < y ? -1 : 1
  }
  return 0
}

function has_managed_user_install(): boolean {
  return user_profile_paths().some(
    (path) => managed_user_install(path) !== undefined,
  )
}

function has_managed_system_install(): boolean {
  const path = sudo_conf_path()
  if (!existsSync(path)) return false
  const result = spawnSync('cat', [path], { stdio: ['pipe', 'pipe', 'pipe'] })
  return /^# sido\nPath askpass .*$/m.test(result.stdout?.toString() ?? '')
}

function require_npm_install(): void {
  const root = spawnSync('npm', ['root', '--global'], {
    stdio: ['inherit', 'pipe', 'inherit'],
    encoding: 'utf8',
  })
  if (root.error || root.status !== 0) {
    console.error('[sido] upgrade requires npm and an npm global installation')
    process.exit(1)
  }

  try {
    const package_path = realpathSync(join(root.stdout.trim(), 'sido-askpass'))
    const executable_path = realpathSync(self_path)
    if (
      executable_path !== package_path &&
      !executable_path.startsWith(`${package_path}${sep}`)
    ) {
      throw new Error('outside npm package')
    }
  } catch {
    console.error(
      '[sido] upgrade supports npm global installations only; use the original package manager to upgrade',
    )
    process.exit(1)
  }
}

function latest_npm_version(): string {
  const latest_result = spawnSync(
    'npm',
    ['view', 'sido-askpass@latest', 'version', '--json'],
    { stdio: ['inherit', 'pipe', 'inherit'], encoding: 'utf8' },
  )
  if (latest_result.error) {
    console.error(
      `[sido] could not check npm for upgrades: ${latest_result.error.message}`,
    )
    process.exit(1)
  }
  if (latest_result.status !== 0) {
    console.error(
      `[sido] could not check npm for upgrades (status ${latest_result.status})`,
    )
    process.exit(latest_result.status ?? 1)
  }

  let latest: string
  try {
    const value: unknown = JSON.parse(latest_result.stdout)
    if (typeof value === 'string') {
      latest = value
    } else if (
      Array.isArray(value) &&
      value.length === 1 &&
      typeof value[0] === 'string'
    ) {
      latest = value[0]
    } else {
      throw new Error('not a version string')
    }
  } catch {
    console.error('[sido] npm returned an invalid latest version')
    process.exit(1)
  }
  return latest
}

function run_npm_upgrade(latest: string): void {
  console.error(
    `[sido] upgrading ${package_json.version} to ${latest} with npm`,
  )
  const upgrade = spawnSync(
    'npm',
    ['install', '--global', 'sido-askpass@latest'],
    { stdio: 'inherit' },
  )
  if (upgrade.error) {
    console.error(`[sido] npm upgrade failed: ${upgrade.error.message}`)
    process.exit(1)
  }
  if (upgrade.status !== 0) {
    console.error(`[sido] npm upgrade failed with status ${upgrade.status}`)
    process.exit(upgrade.status ?? 1)
  }
}

function refresh_managed_configuration(): never {
  console.error('[sido] refreshing managed configuration')
  const install = spawnSync(cli_path, ['install'], {
    stdio: 'inherit',
  })
  if (install.error) {
    console.error(
      `[sido] upgraded, but configuration refresh failed: ${install.error.message}`,
    )
    process.exit(1)
  }
  process.exit(install.status ?? 1)
}

function do_upgrade(): never {
  require_npm_install()
  const latest = latest_npm_version()
  const comparison = compare_semver(package_json.version, latest)
  if (comparison === undefined) {
    console.error(
      `[sido] cannot compare versions ${package_json.version} and ${latest}`,
    )
    process.exit(1)
  }
  if (comparison >= 0) {
    console.error(
      `[sido] already up to date (${package_json.version}; npm latest is ${latest})`,
    )
    process.exit(0)
  }

  run_npm_upgrade(latest)
  refresh_managed_configuration()
}

function do_install(scope?: string): void {
  if (!scope) {
    const scopes = [
      ...(has_managed_user_install() ? ['--user'] : []),
      ...(has_managed_system_install() ? ['--system'] : []),
    ]
    if (scopes.length === 0) {
      console.error(
        `[sido] no existing installation found; use: ${self_path} install --user|--system`,
      )
      process.exit(1)
    }
    for (const existing_scope of scopes) do_install(existing_scope)
    return
  }

  if (scope === '--user') install_user()
  else install_system()
}

function install_user(): void {
  const target_path = user_profile_path()
  const line = `export SUDO_ASKPASS="${askpass_path}"`
  const alias_line =
    basename(target_path) === '.zshrc' ? "\nalias sudo='sudo -A'" : ''
  const managed_block = `# sido start\n${line}${alias_line}\n# sido end\n`
  const migrated_paths: string[] = []

  for (const path of user_profile_paths()) {
    if (!existsSync(path)) continue
    const content = readFileSync(path, 'utf8')
    if (!managed_user_block_pattern().test(content)) continue
    if (path !== target_path) {
      writeFileSync(path, `${remove_managed_user_install(content)}\n`)
      migrated_paths.push(path)
    }
  }

  const target_content = existsSync(target_path)
    ? readFileSync(target_path, 'utf8')
    : ''
  const unmanaged_content = remove_managed_user_install(target_content)
  for (const match of unmanaged_content.matchAll(
    /^export SUDO_ASKPASS=(.*)$/gm,
  )) {
    console.error(
      `[sido] warning: preserving unmanaged SUDO_ASKPASS=${match[1]} in ${target_path}`,
    )
  }
  if (managed_user_block_pattern().test(target_content)) {
    writeFileSync(
      target_path,
      target_content.replace(managed_user_block_pattern(), managed_block),
    )
  } else {
    const normalized_content = target_content.trimEnd()
    const separator = normalized_content ? '\n\n' : ''
    writeFileSync(
      target_path,
      `${normalized_content}${separator}${managed_block}`,
    )
  }

  for (const path of migrated_paths) {
    console.error(`[sido] migrated user configuration from ${path}`)
  }
  console.error(`[sido] installed to ${target_path}`)
  console.error('[sido] restart your shell, or run:')
  console.error(`[sido] export SUDO_ASKPASS="${askpass_path}"`)
}

function install_system(): void {
  const path = sudo_conf_path()
  const line = `Path askpass ${askpass_path}`
  let content = ''
  if (existsSync(path)) {
    const read_result = spawnSync('cat', [path], {
      stdio: ['pipe', 'pipe', 'pipe'],
    })
    content = read_result.stdout?.toString() ?? ''
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
        `[sido] warning: replacing Path askpass ${old_value} with "${askpass_path}"`,
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

function normalize_uninstalled_content(content: string): string {
  return content.replace(/\n{3,}/g, '\n\n').trim() + '\n'
}

function do_uninstall(scope: string): void {
  if (scope === '--user') {
    const removed_paths: string[] = []
    for (const path of user_profile_paths()) {
      if (!existsSync(path)) continue
      const content = readFileSync(path, 'utf8')
      if (!managed_user_block_pattern().test(content)) continue
      writeFileSync(
        path,
        normalize_uninstalled_content(
          content.replace(managed_user_block_pattern(), ''),
        ),
      )
      removed_paths.push(path)
    }
    if (removed_paths.length === 0) {
      console.error('[sido] nothing to uninstall')
      process.exit(1)
    }
    for (const path of removed_paths) {
      console.error(`[sido] removed from ${path}`)
    }
  } else {
    const path = sudo_conf_path()
    if (!existsSync(path)) {
      console.error('[sido] nothing to uninstall')
      process.exit(1)
    }
    const read_result = spawnSync('cat', [path], {
      stdio: ['pipe', 'pipe', 'pipe'],
    })
    let content = read_result.stdout?.toString() ?? ''
    content = content.replace(/^# sido\n/gm, '')
    content = content.replace(/^Path askpass .*$/gm, '')
    const proc = spawnSync('sudo', ['tee', path], {
      input: normalize_uninstalled_content(content),
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
      const status_result = spawnSync('cat', [sys_path], {
        stdio: ['pipe', 'pipe', 'pipe'],
      })
      const match = status_result.stdout
        ?.toString()
        .match(/^Path askpass (.*)$/m)
      console.error(
        match
          ? `[sido] system askpass: ${match[1]}`
          : `[sido] no system askpass in ${sys_path}`,
      )
    } else {
      console.error(`[sido] ${sys_path} does not exist`)
    }
  }

  if (!scope || scope === '--user') {
    let found = false
    for (const path of user_profile_paths()) {
      const askpass = managed_user_install(path)
      if (askpass === undefined) continue
      found = true
      console.error(`[sido] user askpass in ${path}: ${askpass}`)
    }
    if (!found)
      console.error('[sido] no managed user askpass in shell startup files')
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

  let result
  try {
    result = spawnSync(
      'bash',
      ['-c', script, 'sido', resources.fifo, self_path, resources.prompt_file],
      { stdio: ['inherit', 'pipe', 'inherit'] },
    )
  } finally {
    remove_prompt_resources(resources)
  }
  process.stdout.write(read_stdout(result))
}

// ── Herdr ───────────────────────────────────────────────────────────────────
// Same bash spawnSync pattern as tmux: background cat on the FIFO + blocking
// herdr pane run. Shell job control (&, wait) keeps coordination in one sync
// call and reaps the background cat on cancel.

function herdr_split_direction(): 'right' | 'down' {
  let direction: 'right' | 'down' = 'right'
  try {
    const layout = spawnSync('herdr', ['pane', 'layout', '--current'], {
      stdio: ['inherit', 'pipe', 'pipe'],
    })
    if (layout.status === 0) {
      const info = JSON.parse(layout.stdout?.toString() ?? '{}')
      if ((info.result?.pane?.width ?? 0) <= 150) direction = 'down'
    }
  } catch {}
  return direction
}

function create_herdr_pane(direction: 'right' | 'down'): string | undefined {
  const split = spawnSync(
    'herdr',
    ['pane', 'split', '--current', '--direction', direction, '--cwd', '/'],
    { stdio: ['inherit', 'pipe', 'pipe'] },
  )
  if (split.status !== 0) return
  return JSON.parse(split.stdout?.toString() ?? '{}').result?.pane?.pane_id
}

function close_herdr_pane(pane_id: string): void {
  spawnSync('herdr', ['pane', 'close', pane_id], {
    stdio: ['pipe', 'pipe', 'pipe'],
  })
}

function herdr_receiver_script(): string {
  return [
    `"$1" _inner_prompt_receiver "$2" "$3" &`,
    `receiver=$!`,
    `(`,
    `  while kill -0 "$4" 2>/dev/null; do`,
    `    sleep 0.2`,
    `  done`,
    `  rm -rf -- "$5"`,
    `  herdr pane close "$6" >/dev/null 2>&1`,
    `) &`,
    `watchdog=$!`,
    `wait "$receiver"`,
    `receiver_status=$?`,
    `kill "$watchdog" 2>/dev/null`,
    `wait "$watchdog" 2>/dev/null`,
    `exit "$receiver_status"`,
  ].join('\n')
}

function herdr_prompt(allow_fallback: boolean): void {
  const direction = herdr_split_direction()
  console.error(
    `[sido] password requested — see new pane (${direction === 'right' ? 'right' : 'bottom'})`,
  )

  const pane_id = create_herdr_pane(direction)
  if (!pane_id) return void fallback_herdr(allow_fallback)

  let resources: PromptResources
  try {
    resources = create_prompt_resources()
  } catch (error) {
    close_herdr_pane(pane_id)
    console.error(`[sido] ${String(error)}`)
    process.exit(1)
  }

  const block_cmd =
    `herdr pane run "$1" bash -c "$7" sido ` + `"$3" "$4" "$2" "$5" "$6" "$1"`
  const runner_script = fifo_drain_script('$2', block_cmd, 'run_status')

  let result
  try {
    result = spawnSync(
      'bash',
      [
        '-c',
        runner_script,
        'sido',
        pane_id,
        resources.fifo,
        self_path,
        resources.prompt_file,
        String(process.pid),
        resources.dir,
        herdr_receiver_script(),
      ],
      { stdio: ['inherit', 'pipe', 'inherit'] },
    )
  } finally {
    remove_prompt_resources(resources)
    close_herdr_pane(pane_id)
  }
  process.stdout.write(read_stdout(result))
}

function fallback_herdr(allow_fallback: boolean): void {
  if (!allow_fallback) adapter_unavailable('herdr', 'herdr pane split failed')
  console.error('[sido] herdr pane split failed, falling back')
  if (can_gui) return void gui_prompt()
  if (tty_prompt()) return
  watch_prompt()
}

// ── GUI (macOS / Linux) ────────────────────────────────────────────────────

function gui_prompt(): void {
  if (is_mac) return void mac_gui()
  linux_gui()
}

function mac_gui(): void {
  const dialog_title = 'Administrator Authentication'
  const dialog_result = spawnSync(
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
  if (dialog_result.status !== 0) process.exit(1)
  process.stdout.write(
    dialog_result.stdout?.toString().replace(/\r?\n$/, '') ?? '',
  )
}

function linux_gui(): void {
  for (const prog of [
    ['zenity', '--password', '--title', display_prompt],
    ['kdialog', '--password', display_prompt],
  ]) {
    const [cmd, ...args] = prog as [string, ...string[]]
    if (gui_program_prompt(cmd, args)) return
  }
  console.error('[sido] no GUI dialog found')
  if (tty_prompt()) return
  watch_prompt()
}

function exact_gui_prompt(command: string, args: string[]): void {
  if (!gui_program_prompt(command, args))
    adapter_unavailable(
      command as Adapter,
      `${command} failed or was cancelled`,
    )
}

function gui_program_prompt(command: string, args: string[]): boolean {
  const result = spawnSync(command, args, {
    stdio: ['inherit', 'pipe', 'inherit'],
  })
  if (result.status !== 0) return false
  process.stdout.write(result.stdout?.toString().replace(/\n$/, '') ?? '')
  return true
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
    `[sido] from another terminal run: ${cli_path} approve  (waiting up to ${Math.round(timeout_ms / 1000)}s)`,
  )

  // External `cat` opens the FIFO read-only and blocks until a writer (the
  // approver) connects. spawnSync's timeout kills it if nobody approves in time
  // — avoiding libuv's uncancellable threadpool open() on a FIFO.
  let result
  try {
    result = spawnSync('bash', ['-c', 'cat "$1"', 'sido', resources.fifo], {
      stdio: ['ignore', 'pipe', 'inherit'],
      timeout: timeout_ms,
      killSignal: 'SIGTERM',
    })
  } finally {
    remove_prompt_resources(resources)
  }

  if (result!.signal === 'SIGTERM' || result!.status === null) {
    console.error('[sido] timed out waiting for approver')
    process.exit(1)
  }
  process.stdout.write(read_stdout(result!))
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
    const modified_time = statSync(path).mtimeMs
    if (modified_time > newest_mtime) {
      newest_mtime = modified_time
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
  const writer_result = spawnSync(
    'bash',
    ['-c', 'cat > "$1"', 'sido', join(req_dir, 'password')],
    {
      input: `${password}\n`,
      stdio: ['pipe', 'pipe', 'inherit'],
      timeout: 5_000,
    },
  )
  console.error(
    writer_result.status === 0
      ? '[sido] password sent'
      : '[sido] request expired',
  )
  rmSync(req_dir, { recursive: true, force: true })
}
