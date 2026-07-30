import assert from 'node:assert/strict'
import { after, before, describe, test } from 'node:test'
import { spawn, spawnSync } from 'node:child_process'
import {
  chmodSync,
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  rmSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import package_json from '../package.json' with { type: 'json' }

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const source = join(root, 'src', 'sido-askpass.ts')
const fixture_dir = join(root, 'test', 'fixtures')

function escaped_source_path(): string {
  return source.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}

let test_dir: string
let bin_dir: string
let prompt_tmp: string
let mode_log: string
let pane_log: string

interface RunOptions {
  args?: string[]
  env?: NodeJS.ProcessEnv
}

function run({ args = ['Password: '], env = {} }: RunOptions = {}) {
  const child_env: NodeJS.ProcessEnv = {
    ...process.env,
    PATH: `${bin_dir}:${process.env.PATH}`,
    TMPDIR: prompt_tmp,
    ...env,
  }
  delete child_env.TMUX
  delete child_env.HERDR_ENV
  delete child_env.DISPLAY
  delete child_env.WAYLAND_DISPLAY
  Object.assign(child_env, env)

  return spawnSync(process.execPath, [source, ...args], {
    encoding: 'utf8',
    env: child_env,
    timeout: 3_000,
  })
}

function assert_clean(): void {
  assert.deepEqual(readdirSync(prompt_tmp), [])
}

function install_fake_npm(home_name: string, script: string): string {
  const home = join(test_dir, home_name)
  const npm = join(bin_dir, 'npm')
  mkdirSync(home)
  writeFileSync(npm, script)
  chmodSync(npm, 0o755)
  return home
}

describe('e2e', { concurrency: 1 }, (): void => {
  before(() => {
    test_dir = mkdtempSync(join(tmpdir(), 'sido-e2e-'))
    bin_dir = join(test_dir, 'bin')
    prompt_tmp = join(test_dir, 'prompt-tmp')
    mode_log = join(test_dir, 'modes')
    pane_log = join(test_dir, 'panes')
    mkdirSync(bin_dir)
    mkdirSync(prompt_tmp)
    for (const command of ['tmux', 'herdr']) {
      const target = join(bin_dir, command)
      copyFileSync(join(fixture_dir, command), target)
      chmodSync(target, 0o755)
    }
  })

  after(() => {
    rmSync(test_dir, { recursive: true, force: true })
  })

  test('tmux returns the password with private prompt artifacts', () => {
    const result = run({
      env: {
        TMUX: 'e2e',
        SIDO_E2E_MODE: 'success',
        SIDO_E2E_MODE_LOG: mode_log,
      },
    })
    assert.equal(result.status, 0, result.stderr)
    assert.equal(result.stdout, 'fake-password')
    assert.deepEqual(readFileSync(mode_log, 'utf8').trim().split('\n'), [
      '700',
      '600',
      '600',
    ])
    assert_clean()
  })

  test('tmux cancellation exits without leaking prompt artifacts', () => {
    const result = run({
      env: { TMUX: 'e2e', SIDO_E2E_MODE: 'fail' },
    })
    assert.equal(result.status, 1)
    assert.equal(result.signal, null)
    assert_clean()
  })

  test('Herdr returns the password and closes its pane', () => {
    writeFileSync(pane_log, '')
    const result = run({
      env: {
        HERDR_ENV: '1',
        SIDO_E2E_MODE: 'success',
        SIDO_E2E_PANE_LOG: pane_log,
      },
    })
    assert.equal(result.status, 0, result.stderr)
    assert.equal(result.stdout, 'fake-password')
    assert.equal(readFileSync(pane_log, 'utf8'), 'fake-pane\n')
    assert_clean()
  })

  test('Herdr command failure exits, closes its pane, and does not hang', () => {
    writeFileSync(pane_log, '')
    const result = run({
      env: {
        HERDR_ENV: '1',
        SIDO_E2E_MODE: 'fail',
        SIDO_E2E_PANE_LOG: pane_log,
      },
    })
    assert.equal(result.status, 1)
    assert.equal(result.signal, null)
    assert.equal(readFileSync(pane_log, 'utf8'), 'fake-pane\n')
    assert_clean()
  })

  test('forced Herdr split failure exits without falling back', () => {
    const result = run({
      env: {
        SIDO_ADAPTER: 'herdr',
        SIDO_E2E_MODE: 'split-fail',
        SIDO_E2E_PANE_LOG: pane_log,
      },
    })
    assert.equal(result.status, 1)
    assert.match(result.stderr, /herdr adapter unavailable/)
    assert_clean()
  })

  test('user install warns, marks its export, and uninstalls only its entry', () => {
    const home = join(test_dir, 'home')
    const profile = join(home, '.profile')
    mkdirSync(home)
    writeFileSync(
      profile,
      'export KEEP_ME=yes\nexport SUDO_ASKPASS="/opt/other-askpass"\n',
    )

    const install = run({
      args: ['install', '--user'],
      env: { HOME: home },
    })
    assert.equal(install.status, 0, install.stderr)
    assert.match(install.stderr, /warning: replacing SUDO_ASKPASS=/)
    assert.match(
      readFileSync(profile, 'utf8'),
      /^# sido\nexport SUDO_ASKPASS=/m,
    )

    const reinstall = run({
      args: ['install', '--user'],
      env: { HOME: home },
    })
    assert.equal(reinstall.status, 0, reinstall.stderr)
    assert.doesNotMatch(reinstall.stderr, /warning:/)

    const uninstall = run({
      args: ['uninstall', '--user'],
      env: { HOME: home },
    })
    assert.equal(uninstall.status, 0, uninstall.stderr)
    assert.equal(readFileSync(profile, 'utf8'), 'export KEEP_ME=yes\n')
  })

  test('install without a scope reapplies an existing managed install', () => {
    const home = join(test_dir, 'home-install-detect')
    const profile = join(home, '.profile')
    mkdirSync(home)
    writeFileSync(profile, '# sido\nexport SUDO_ASKPASS="/old/sido-askpass"\n')

    const result = run({ args: ['install'], env: { HOME: home } })
    assert.equal(result.status, 0, result.stderr)
    assert.match(
      readFileSync(profile, 'utf8'),
      new RegExp(escaped_source_path()),
    )
  })

  test('install without a scope requires one for first setup', () => {
    const home = join(test_dir, 'home-install-new')
    mkdirSync(home)
    const result = run({ args: ['install'], env: { HOME: home } })
    assert.equal(result.status, 1)
    assert.match(result.stderr, /no existing installation found/)
    assert.match(result.stderr, /install --user\|--system/)
  })

  test('status rejects mutually exclusive scopes', () => {
    const result = run({ args: ['status', '--user', '--system'] })
    assert.equal(result.status, 1)
    assert.match(result.stderr, /status \[--user\|--system\]/)
  })

  test('--version prints the package version', () => {
    const result = run({ args: ['--version'] })
    assert.equal(result.status, 0, result.stderr)
    assert.equal(result.stdout.trim(), package_json.version)
  })

  test('--help prints usage', () => {
    const result = run({ args: ['--help'] })
    assert.equal(result.status, 0, result.stderr)
    assert.match(result.stdout, /Usage:/)
    assert.match(result.stdout, /askpass mode/)
    assert.match(result.stdout, /sido-askpass upgrade/)
    for (const adapter of [
      'auto',
      'tmux',
      'herdr',
      'osascript',
      'zenity',
      'kdialog',
      'tty',
      'watch',
    ]) {
      assert.match(result.stdout, new RegExp(`^  ${adapter} `, 'm'))
    }
  })

  test('upgrade installs the latest npm package and refreshes user config', () => {
    const npm_log = join(test_dir, 'npm-upgrade-args')
    const npm_script = [
      '#!/usr/bin/env bash',
      'if [ "$1" = root ]; then',
      `  printf '%s\\n' ${JSON.stringify(dirname(root))}`,
      'elif [ "$1" = view ]; then',
      `  printf '"0.8.0"\\n'`,
      'else',
      `  printf '%s\\n' "$@" > "$SIDO_E2E_NPM_LOG"`,
      'fi',
      '',
    ].join('\n')
    const home = install_fake_npm('home-upgrade', npm_script)
    const install = run({
      args: ['install', '--user'],
      env: { HOME: home },
    })
    assert.equal(install.status, 0, install.stderr)

    const result = run({
      args: ['upgrade'],
      env: { HOME: home, SIDO_E2E_NPM_LOG: npm_log },
    })
    assert.equal(result.status, 0, result.stderr)
    assert.equal(
      readFileSync(npm_log, 'utf8'),
      'install\n--global\nsido-askpass@latest\n',
    )
    assert.match(
      readFileSync(join(home, '.profile'), 'utf8'),
      /^# sido\nexport SUDO_ASKPASS=/m,
    )
    assert.match(result.stderr, /refreshing managed configuration/)
  })

  test('upgrade does not change user config when npm fails', () => {
    const npm_script = [
      '#!/usr/bin/env bash',
      'if [ "$1" = root ]; then',
      `  printf '%s\\n' ${JSON.stringify(dirname(root))}`,
      'elif [ "$1" = view ]; then',
      `  printf '"0.8.0"\\n'`,
      'else',
      '  exit 23',
      'fi',
      '',
    ].join('\n')
    const home = install_fake_npm('home-upgrade-failure', npm_script)

    const result = run({ args: ['upgrade'], env: { HOME: home } })
    assert.equal(result.status, 23)
    assert.match(result.stderr, /npm upgrade failed with status 23/)
    assert.equal(existsSync(join(home, '.profile')), false)
  })

  test('upgrade skips npm install when already current', () => {
    const npm_log = join(test_dir, 'npm-upgrade-current-args')
    const npm_script = [
      '#!/usr/bin/env bash',
      'if [ "$1" = root ]; then',
      `  printf '%s\\n' ${JSON.stringify(dirname(root))}`,
      'else',
      `  printf '%s\\n' "$@" >> "$SIDO_E2E_NPM_LOG"`,
      'fi',
      'if [ "$1" = view ]; then',
      `  printf '"${package_json.version}"\\n'`,
      'fi',
      '',
    ].join('\n')
    const home = install_fake_npm('home-upgrade-current', npm_script)

    const result = run({
      args: ['upgrade'],
      env: { HOME: home, SIDO_E2E_NPM_LOG: npm_log },
    })
    assert.equal(result.status, 0, result.stderr)
    assert.match(result.stderr, /already up to date/)
    assert.equal(
      readFileSync(npm_log, 'utf8'),
      'view\nsido-askpass@latest\nversion\n--json\n',
    )
    assert.equal(existsSync(join(home, '.profile')), false)
  })

  test('upgrade rejects a non-npm installation', () => {
    const npm_root = join(test_dir, 'other-npm-root')
    mkdirSync(npm_root)
    const npm_script =
      `#!/usr/bin/env bash\n` + `printf '%s\\n' ${JSON.stringify(npm_root)}\n`
    const home = install_fake_npm('home-upgrade-non-npm', npm_script)

    const result = run({ args: ['upgrade'], env: { HOME: home } })
    assert.equal(result.status, 1)
    assert.match(result.stderr, /npm global installations only/)
  })

  test('run requires -- and sets SUDO_ASKPASS and the adapter', () => {
    const out = join(test_dir, 'run-env')
    const result = run({
      args: [
        'run',
        '--adapter',
        'watch',
        '--',
        'bash',
        '-c',
        `printf '%s\\n%s' "$SUDO_ASKPASS" "$SIDO_ADAPTER" > "${out}"`,
      ],
    })
    assert.equal(result.status, 0, result.stderr)
    assert.equal(readFileSync(out, 'utf8'), `${source}\nwatch`)

    const missing_separator = run({ args: ['run', 'true'] })
    assert.equal(missing_separator.status, 1)
    assert.match(missing_separator.stderr, /run .* -- <command>/)
  })

  test('unknown adapter exits with the allowed values', () => {
    const result = run({ env: { SIDO_ADAPTER: 'nope' } })
    assert.equal(result.status, 1)
    assert.match(result.stderr, /unknown adapter "nope"/)
    assert.match(result.stderr, /auto, tmux, herdr, osascript/)
  })

  test('status --user reports the installed askpass', () => {
    const home = join(test_dir, 'home-status')
    mkdirSync(home)
    run({ args: ['install', '--user'], env: { HOME: home } })
    const result = run({ args: ['status', '--user'], env: { HOME: home } })
    assert.equal(result.status, 0, result.stderr)
    assert.match(result.stderr, /user askpass in/)
  })

  test('watch times out and hints when no approver connects', () => {
    const runtime = join(test_dir, 'runtime-timeout')
    mkdirSync(runtime, { recursive: true })
    const result = run({
      args: ['Password: '],
      env: {
        XDG_RUNTIME_DIR: runtime,
        SIDO_ADAPTER: 'watch',
        SIDO_WATCH_TIMEOUT: '1',
      },
    })
    assert.equal(result.status, 1, result.stdout)
    assert.match(result.stderr, /from another terminal run: .* approve/)
    assert.match(result.stderr, /timed out waiting for approver/)
    assert.deepEqual(readdirSync(join(runtime, 'sido')), [])
  })

  test('watch hands the password through the FIFO to an approver', async () => {
    const runtime = join(test_dir, 'runtime-watch')
    mkdirSync(runtime, { recursive: true })
    const env: NodeJS.ProcessEnv = {
      ...process.env,
      PATH: `${bin_dir}:${process.env.PATH}`,
      XDG_RUNTIME_DIR: runtime,
      SIDO_ADAPTER: 'watch',
      SIDO_WATCH_TIMEOUT: '10',
    }
    delete env.TMUX
    delete env.HERDR_ENV
    delete env.DISPLAY
    delete env.WAYLAND_DISPLAY
    const child = spawn(process.execPath, [source, 'Password: '], {
      env,
      stdio: ['ignore', 'pipe', 'pipe'],
    })
    let stdout = ''
    let stderr = ''
    child.stdout.on('data', (d: Buffer) => (stdout += d.toString()))
    child.stderr.on('data', (d: Buffer) => (stderr += d.toString()))

    let fifo: string | undefined
    for (let i = 0; i < 100; i++) {
      try {
        const entries = readdirSync(join(runtime, 'sido')).filter((e) =>
          e.startsWith('sido-'),
        )
        if (entries.length > 0) {
          fifo = join(runtime, 'sido', entries[0]!, 'password')
          break
        }
      } catch {}
      await new Promise((r) => setTimeout(r, 50))
    }
    assert.ok(fifo, 'request FIFO appeared')

    const w = spawnSync('bash', ['-c', 'cat > "$1"', 'sido', fifo], {
      input: 'e2e-secret\n',
      encoding: 'utf8',
      timeout: 3_000,
    })
    assert.equal(w.status, 0, w.stderr)

    const code = await new Promise<number>((resolve) =>
      child.on('close', resolve),
    )
    assert.equal(code, 0, `child exited ${code}: ${stderr}`)
    assert.equal(stdout, 'e2e-secret')
    assert.deepEqual(readdirSync(join(runtime, 'sido')), [])
  })

  test('approve reports nothing when no requests are pending', () => {
    const runtime = join(test_dir, 'runtime-empty')
    mkdirSync(runtime, { recursive: true })
    const result = run({ args: ['approve'], env: { XDG_RUNTIME_DIR: runtime } })
    assert.equal(result.status, 0, result.stderr)
    assert.match(result.stderr, /no pending password requests/)
  })
})
