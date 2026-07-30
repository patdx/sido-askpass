import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import {
  chmodSync,
  copyFileSync,
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
const fixture_dir = join(root, 'e2e', 'fixtures')
const test_dir = mkdtempSync(join(tmpdir(), 'sido-e2e-'))
const bin_dir = join(test_dir, 'bin')
const prompt_tmp = join(test_dir, 'prompt-tmp')
const mode_log = join(test_dir, 'modes')
const pane_log = join(test_dir, 'panes')

mkdirSync(bin_dir)
mkdirSync(prompt_tmp)
for (const command of ['tmux', 'herdr']) {
  const target = join(bin_dir, command)
  copyFileSync(join(fixture_dir, command), target)
  chmodSync(target, 0o755)
}

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

function test(name: string, fn: () => void): void {
  fn()
  console.log(`✓ ${name}`)
}

try {
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

  test('Herdr split failure falls back without allocating artifacts', () => {
    const result = run({
      env: {
        HERDR_ENV: '1',
        SIDO_E2E_MODE: 'split-fail',
        SIDO_E2E_PANE_LOG: pane_log,
      },
    })
    assert.equal(result.status, 1)
    assert.match(result.stderr, /herdr pane split failed/)
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
  })

  test('run sets SUDO_ASKPASS for the child command', () => {
    const out = join(test_dir, 'run-env')
    const result = run({
      args: ['run', 'bash', '-c', `printf '%s' "$SUDO_ASKPASS" > "${out}"`],
    })
    assert.equal(result.status, 0, result.stderr)
    assert.equal(readFileSync(out, 'utf8'), source)
  })

  test('status --user reports the installed askpass', () => {
    const home = join(test_dir, 'home-status')
    mkdirSync(home)
    run({ args: ['install', '--user'], env: { HOME: home } })
    const result = run({ args: ['status', '--user'], env: { HOME: home } })
    assert.equal(result.status, 0, result.stderr)
    assert.match(result.stderr, /user askpass in/)
  })
} finally {
  rmSync(test_dir, { recursive: true, force: true })
}
