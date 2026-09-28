package sido

// End-to-end tests.
//
// These run the real thing: the host binary is built into a scratch bin
// directory, the real shell launchers are installed next to it, and both are
// driven through stub external tools — the fixture fakes in ../test-fakes
// (tmux, herdr) plus generated fakes for npm and sudo. A test therefore covers
// the launcher, argument parsing, adapter dispatch, the FIFO protocol, and the
// install/uninstall/upgrade file edits exactly as a user would exercise them.
//
// Every path a test can write to is redirected into a per-test scratch
// directory (HOME, TMPDIR, XDG_RUNTIME_DIR). The default npm and sudo stubs fail
// loudly, so a test can never reach the real package manager or /etc/sudo.conf.
//
// Adapter tests pin SIDO_ADAPTER rather than relying on auto-detection, because
// auto consults /dev/tty and DISPLAY/WAYLAND_DISPLAY and so behaves differently
// on a developer's terminal than in CI.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// ── shared build ─────────────────────────────────────────────────────────────

var (
	e2eOnce    sync.Once
	e2eErr     error
	e2eRoot    string // shared scratch root, removed by TestMain
	e2eBinDir  string // launchers + host binary + stub tools
	e2eVersion string // package.json version, baked into the binary
)

func TestMain(m *testing.M) {
	code := m.Run()
	if e2eRoot != "" {
		os.RemoveAll(e2eRoot)
	}
	os.Exit(code)
}

// e2eSourceDir is the package directory, i.e. <repo>/go-src/internal/sido.
func e2eSourceDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(file)
}

// e2eRepoPath joins path elements onto the repository root.
func e2eRepoPath(elem ...string) string {
	return filepath.Join(append([]string{e2eSourceDir(), "..", "..", ".."}, elem...)...)
}

func e2ePackageVersion() (string, error) {
	data, err := os.ReadFile(e2eRepoPath("package.json"))
	if err != nil {
		return "", err
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return "", err
	}
	if pkg.Version == "" {
		return "", errors.New("package.json has no version")
	}
	return pkg.Version, nil
}

func e2eSetup() error {
	version, err := e2ePackageVersion()
	if err != nil {
		return err
	}
	root, err := os.MkdirTemp("", "sido-e2e-")
	if err != nil {
		return err
	}
	// Publish the root before anything can fail, so TestMain always cleans it up.
	e2eRoot = root
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}

	// Build the host binary under the name the launcher looks for.
	exe := e2eHostBinary(binDir)
	build := exec.Command("go", "build", "-trimpath",
		"-ldflags", "-X sido-go/internal/sido.Version="+version,
		"-o", exe, "./cmd/sido")
	build.Dir = e2eRepoPath("go-src")
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("go build in %s: %v\n%s", build.Dir, err, out)
	}

	// The real launchers and the adapter fixture fakes.
	for _, cp := range []struct{ src, dst string }{
		{e2eRepoPath("scripts", "sido.sh"), filepath.Join(binDir, "sido")},
		{e2eRepoPath("scripts", "sido-askpass.sh"), filepath.Join(binDir, "sido-askpass")},
		{e2eRepoPath("go-src", "test-fakes", "tmux"), filepath.Join(binDir, "tmux")},
		{e2eRepoPath("go-src", "test-fakes", "herdr"), filepath.Join(binDir, "herdr")},
	} {
		if err := e2eCopyFile(cp.src, cp.dst); err != nil {
			return err
		}
	}

	// Safety stubs: fail loudly rather than touching the real npm or sudo.conf.
	if err := e2eWriteFile(filepath.Join(binDir, "npm"), 0o755,
		"#!/bin/sh\necho '[e2e] npm stub is not configured for this test' >&2\nexit 1\n"); err != nil {
		return err
	}
	if err := e2eWriteFile(filepath.Join(binDir, "sudo"), 0o755,
		"#!/bin/sh\necho '[e2e] sudo stub: refusing to run in tests' >&2\nexit 1\n"); err != nil {
		return err
	}

	e2eRoot, e2eBinDir, e2eVersion = root, binDir, version
	return nil
}

// e2eHostBinary is the per-platform binary name the launcher resolves to.
func e2eHostBinary(binDir string) string {
	return filepath.Join(binDir, "sido-"+runtime.GOOS+"-"+runtime.GOARCH)
}

func e2eWriteFile(path string, mode os.FileMode, content string) error {
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func e2eCopyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return e2eWriteFile(dst, 0o755, string(data))
}

// ── per-test harness ─────────────────────────────────────────────────────────

type e2eHarness struct {
	t         *testing.T
	dir       string // per-test scratch
	binDir    string // shared: launchers, host binary, stub tools
	promptTmp string // TMPDIR seen by the child
	home      string // default isolated HOME
	runtime   string // default isolated XDG_RUNTIME_DIR
	version   string
}

func newE2E(t *testing.T) *e2eHarness {
	t.Helper()
	if testing.Short() {
		t.Skip("e2e test skipped with -short")
	}
	e2eOnce.Do(func() { e2eErr = e2eSetup() })
	if e2eErr != nil {
		t.Fatalf("e2e setup failed: %v", e2eErr)
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("e2e tests need bash for the fixture stubs")
	}

	dir := t.TempDir()
	h := &e2eHarness{
		t:         t,
		dir:       dir,
		binDir:    e2eBinDir,
		promptTmp: filepath.Join(dir, "prompt-tmp"),
		home:      filepath.Join(dir, "home"),
		runtime:   filepath.Join(dir, "runtime"),
		version:   e2eVersion,
	}
	for _, d := range []string{h.promptTmp, h.home, h.runtime} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func (h *e2eHarness) cliPath() string     { return filepath.Join(h.binDir, "sido") }
func (h *e2eHarness) askpassPath() string { return filepath.Join(h.binDir, "sido-askpass") }

// newHome returns a fresh isolated HOME.
func (h *e2eHarness) newHome(name string) string {
	h.t.Helper()
	home := filepath.Join(h.dir, "homes", name)
	if err := os.MkdirAll(home, 0o755); err != nil {
		h.t.Fatal(err)
	}
	return home
}

// env builds the child environment: the host environment minus everything that
// would make the run host-dependent, plus per-test isolation. Later entries win
// in exec.Cmd, so extra overrides the defaults.
func (h *e2eHarness) env(extra ...string) []string {
	drop := map[string]bool{
		"TMUX": true, "HERDR_ENV": true, "DISPLAY": true, "WAYLAND_DISPLAY": true,
		"SUDO_ASKPASS": true, "XDG_RUNTIME_DIR": true, "HOME": true, "SHELL": true,
		"TMPDIR": true, "SIDO_ADAPTER": true, "SIDO_WATCH_TIMEOUT": true,
	}
	base := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 && drop[kv[:i]] {
			continue
		}
		base = append(base, kv)
	}
	base = append(base,
		"PATH="+h.binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+h.home,
		"TMPDIR="+h.promptTmp,
		"SHELL=/bin/sh",
		"XDG_RUNTIME_DIR="+h.runtime,
	)
	return append(base, extra...)
}

type e2eResult struct {
	stdout string
	stderr string
	status int
}

func (h *e2eHarness) runCli(env []string, args ...string) e2eResult {
	h.t.Helper()
	return h.run(env, 10*time.Second, h.cliPath(), args...)
}

func (h *e2eHarness) runAskpass(env []string, args ...string) e2eResult {
	h.t.Helper()
	return h.run(env, 10*time.Second, h.askpassPath(), args...)
}

func (h *e2eHarness) run(env []string, timeout time.Duration, exe string, args ...string) e2eResult {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Env = h.env(env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	err := cmd.Run()
	res := e2eResult{stdout: stdout.String(), stderr: stderr.String()}
	if ctx.Err() == context.DeadlineExceeded {
		h.t.Fatalf("%s %v timed out after %s\nstdout: %s\nstderr: %s",
			exe, args, timeout, res.stdout, res.stderr)
	}
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			h.t.Fatalf("running %s %v: %v", exe, args, err)
		}
		res.status = exitErr.ExitCode()
	}
	return res
}

// assertTmpClean fails if prompt artifacts leaked into the child's TMPDIR.
func (h *e2eHarness) assertTmpClean() {
	h.t.Helper()
	entries, err := os.ReadDir(h.promptTmp)
	if err != nil {
		h.t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		h.t.Errorf("prompt artifacts leaked into TMPDIR: %v", names)
	}
}

func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

func e2eReadFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

func processExists(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func mustMatch(t *testing.T, re, got, what string) {
	t.Helper()
	if !regexp.MustCompile(re).MatchString(got) {
		t.Errorf("%s: %q does not match %s", what, got, re)
	}
}

func mustNotMatch(t *testing.T, re, got, what string) {
	t.Helper()
	if regexp.MustCompile(re).MatchString(got) {
		t.Errorf("%s: %q unexpectedly matches %s", what, got, re)
	}
}

// ── npm fakes (upgrade tests) ────────────────────────────────────────────────

// fakeNpmRoot lays out a directory shaped like an npm global root, containing a
// sido-askpass entry that resolves to the bin directory holding the binary, so
// requireNpmInstall accepts it as a real npm install.
func (h *e2eHarness) fakeNpmRoot() string {
	h.t.Helper()
	root := filepath.Join(h.dir, "npm-root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		h.t.Fatal(err)
	}
	pkg := filepath.Join(root, "sido-askpass")
	if err := os.Symlink(h.binDir, pkg); err != nil {
		h.t.Fatal(err)
	}
	return root
}

func (h *e2eHarness) writeNpmStub(script string) {
	h.t.Helper()
	path := filepath.Join(h.binDir, "npm")
	if err := e2eWriteFile(path, 0o755, script); err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() {
		// Restore the safety stub so a later test cannot reach the real npm.
		e2eWriteFile(path, 0o755,
			"#!/bin/sh\necho '[e2e] npm stub is not configured for this test' >&2\nexit 1\n")
	})
}

// newerVersion bumps the minor of the package version, mirroring the npm update
// the upgrade tests pretend to publish.
func (h *e2eHarness) newerVersion() string {
	h.t.Helper()
	m := regexp.MustCompile(`^(\d+)\.(\d+)\.`).FindStringSubmatch(h.version)
	if m == nil {
		h.t.Fatalf("unparsable package version %q", h.version)
	}
	minor, err := strconv.Atoi(m[2])
	if err != nil {
		h.t.Fatal(err)
	}
	return fmt.Sprintf("%s.%d.0", m[1], minor+1)
}

// ── tmux ─────────────────────────────────────────────────────────────────────

func TestE2ETmuxReturnsPasswordWithPrivateArtifacts(t *testing.T) {
	h := newE2E(t)
	modeLog := filepath.Join(h.dir, "modes")

	res := h.runAskpass([]string{
		"TMUX=e2e",
		"SIDO_ADAPTER=tmux",
		"SIDO_E2E_MODE=success",
		"SIDO_E2E_MODE_LOG=" + modeLog,
	}, "Password: ")

	if res.status != 0 {
		t.Fatalf("status = %d, want 0\nstderr: %s", res.status, res.stderr)
	}
	if res.stdout != "fake-password" {
		t.Errorf("stdout = %q, want %q", res.stdout, "fake-password")
	}
	got := strings.Fields(e2eReadFile(t, modeLog))
	if want := []string{"700", "600", "600"}; !slices.Equal(got, want) {
		t.Errorf("prompt artifact modes = %v, want %v (dir, fifo, prompt)", got, want)
	}
	h.assertTmpClean()
}

func TestE2ETmuxCancellationExitsWithoutLeakingArtifacts(t *testing.T) {
	h := newE2E(t)

	res := h.runAskpass([]string{
		"TMUX=e2e",
		"SIDO_ADAPTER=tmux",
		"SIDO_E2E_MODE=fail",
	}, "Password: ")

	if res.status != 1 {
		t.Errorf("status = %d, want 1\nstderr: %s", res.status, res.stderr)
	}
	h.assertTmpClean()
}

// ── Herdr ────────────────────────────────────────────────────────────────────

func TestE2EHerdrReturnsPasswordAndClosesPane(t *testing.T) {
	h := newE2E(t)
	paneLog := filepath.Join(h.dir, "panes")

	res := h.runAskpass([]string{
		"HERDR_ENV=1",
		"SIDO_ADAPTER=herdr",
		"SIDO_E2E_MODE=success",
		"SIDO_E2E_PANE_LOG=" + paneLog,
	}, "Password: ")

	if res.status != 0 {
		t.Fatalf("status = %d, want 0\nstderr: %s", res.status, res.stderr)
	}
	if res.stdout != "fake-password" {
		t.Errorf("stdout = %q, want %q", res.stdout, "fake-password")
	}
	if got, want := e2eReadFile(t, paneLog), "fake-pane\n"; got != want {
		t.Errorf("pane log = %q, want %q", got, want)
	}
	h.assertTmpClean()
}

func TestE2EHerdrCommandFailureExitsAndClosesPane(t *testing.T) {
	h := newE2E(t)
	paneLog := filepath.Join(h.dir, "panes")

	res := h.runAskpass([]string{
		"HERDR_ENV=1",
		"SIDO_ADAPTER=herdr",
		"SIDO_E2E_MODE=fail",
		"SIDO_E2E_PANE_LOG=" + paneLog,
	}, "Password: ")

	if res.status != 1 {
		t.Errorf("status = %d, want 1\nstderr: %s", res.status, res.stderr)
	}
	if got, want := e2eReadFile(t, paneLog), "fake-pane\n"; got != want {
		t.Errorf("pane log = %q, want %q (pane must be closed on failure)", got, want)
	}
	h.assertTmpClean()
}

// TestE2EHerdrReceiverCleansUpWhenShimDies covers the orphan contract: the
// receiver runs inside the pane, survives the shim, and pid-polls it. When the
// shim dies the receiver must remove the prompt directory, close the pane, and
// exit — otherwise a closed surface leaves an unusable pane and a FIFO behind.
//
// It drives the real `_inner_prompt_receiver` with a stand-in shim process
// rather than the fake `herdr pane run`, because the fake substitutes a sleep
// loop for the receiver and so cannot exercise the real watchdog.
func TestE2EHerdrReceiverCleansUpWhenShimDies(t *testing.T) {
	h := newE2E(t)
	paneLog := filepath.Join(h.dir, "panes")

	shim := exec.Command("sleep", "60")
	if err := shim.Start(); err != nil {
		t.Fatal(err)
	}
	shimReaped := false
	reapShim := func() {
		if shimReaped {
			return
		}
		shimReaped = true
		shim.Process.Kill()
		shim.Wait()
	}
	defer reapShim()

	base := filepath.Join(h.dir, "watch")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	res, err := createPromptResources(base)
	if err != nil {
		t.Fatal(err)
	}

	receiver := exec.Command(e2eHostBinary(h.binDir), "_inner_prompt_receiver",
		res.promptFile, res.fifo, res.dir, strconv.Itoa(shim.Process.Pid), "fake-pane")
	receiver.Env = h.env("SIDO_E2E_PANE_LOG=" + paneLog)
	var receiverErr bytes.Buffer
	receiver.Stderr = &receiverErr

	if err := receiver.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		receiver.Wait()
		close(exited)
	}()

	// While the shim lives the receiver must stay up (it is parked on the FIFO).
	time.Sleep(500 * time.Millisecond)
	select {
	case <-exited:
		t.Fatalf("receiver exited while the shim was alive\nstderr: %s", receiverErr.String())
	default:
	}

	// The receiver polls the shim with kill(pid, 0), so the shim must be reaped
	// for its pid to disappear — otherwise it lingers as a zombie and the
	// receiver rightly keeps waiting.
	reapShim()

	waitUntil(t, 10*time.Second, "receiver to clean up after shim death", func() bool {
		if _, err := os.Stat(res.dir); !os.IsNotExist(err) {
			return false
		}
		if !strings.Contains(e2eReadFile(t, paneLog), "fake-pane") {
			return false
		}
		select {
		case <-exited:
			return true
		default:
			return false
		}
	})
}

func TestE2EHerdrForcedSplitFailureDoesNotFallBack(t *testing.T) {
	h := newE2E(t)
	paneLog := filepath.Join(h.dir, "panes")

	res := h.runAskpass([]string{
		"HERDR_ENV=1",
		"SIDO_ADAPTER=herdr",
		"SIDO_E2E_MODE=split-fail",
		"SIDO_E2E_PANE_LOG=" + paneLog,
	}, "Password: ")

	if res.status != 1 {
		t.Errorf("status = %d, want 1\nstderr: %s", res.status, res.stderr)
	}
	mustMatch(t, `herdr adapter unavailable`, res.stderr, "stderr")
	h.assertTmpClean()
}

// ── install / uninstall / status ─────────────────────────────────────────────

func TestE2EUserInstallPreservesUnmanagedConfig(t *testing.T) {
	h := newE2E(t)
	home := h.newHome("preserve-unmanaged")
	profile := filepath.Join(home, ".profile")
	const original = "export KEEP_ME=yes\nexport SUDO_ASKPASS=\"/opt/other-askpass\"\n"
	if err := os.WriteFile(profile, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + home}

	install := h.runCli(env, "install", "--user")
	if install.status != 0 {
		t.Fatalf("install status = %d\nstderr: %s", install.status, install.stderr)
	}
	mustMatch(t, `warning: preserving unmanaged SUDO_ASKPASS=`, install.stderr, "install stderr")
	mustMatch(t, `(?m)^# sido start\nexport SUDO_ASKPASS=.*\n# sido end$`,
		e2eReadFile(t, profile), "profile after install")

	reinstall := h.runCli(env, "install", "--user")
	if reinstall.status != 0 {
		t.Fatalf("reinstall status = %d\nstderr: %s", reinstall.status, reinstall.stderr)
	}
	wantInstalled := "export KEEP_ME=yes\nexport SUDO_ASKPASS=\"/opt/other-askpass\"\n\n" +
		"# sido start\nexport SUDO_ASKPASS=\"" + h.askpassPath() + "\"\n# sido end\n"
	if got := e2eReadFile(t, profile); got != wantInstalled {
		t.Errorf("profile after reinstall =\n%q\nwant\n%q", got, wantInstalled)
	}

	uninstall := h.runCli(env, "uninstall", "--user")
	if uninstall.status != 0 {
		t.Fatalf("uninstall status = %d\nstderr: %s", uninstall.status, uninstall.stderr)
	}
	if got := e2eReadFile(t, profile); got != original {
		t.Errorf("profile after uninstall =\n%q\nwant\n%q", got, original)
	}
}

func TestE2EZshInstallMigratesManagedEntry(t *testing.T) {
	h := newE2E(t)
	home := h.newHome("zsh-migration")
	profile := filepath.Join(home, ".profile")
	zshrc := filepath.Join(home, ".zshrc")
	const profileBefore = "export KEEP_PROFILE=yes\n\n" +
		"# sido start\nexport SUDO_ASKPASS=\"/old/path\"\n# sido end\n"
	if err := os.WriteFile(profile, []byte(profileBefore), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zshrc, []byte("eval \"$(fnm env)\"\nexport KEEP_ZSH=yes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + home, "SHELL=/bin/zsh"}

	install := h.runCli(env, "install", "--user")
	if install.status != 0 {
		t.Fatalf("install status = %d\nstderr: %s", install.status, install.stderr)
	}
	if got, want := e2eReadFile(t, profile), "export KEEP_PROFILE=yes\n"; got != want {
		t.Errorf("profile after migration = %q, want %q", got, want)
	}
	wantZshrc := "eval \"$(fnm env)\"\nexport KEEP_ZSH=yes\n\n" +
		"# sido start\nexport SUDO_ASKPASS=\"" + h.askpassPath() + "\"\nalias sudo='sudo -A'\n# sido end\n"
	if got := e2eReadFile(t, zshrc); got != wantZshrc {
		t.Errorf("zshrc after install =\n%q\nwant\n%q", got, wantZshrc)
	}
	mustMatch(t, `migrated user configuration from .*\.profile`, install.stderr, "install stderr")
	mustMatch(t, `restart your shell`, install.stderr, "install stderr")
	mustNotMatch(t, `source `, install.stderr, "install stderr")

	reinstall := h.runCli(env, "install", "--user")
	if reinstall.status != 0 {
		t.Fatalf("reinstall status = %d\nstderr: %s", reinstall.status, reinstall.stderr)
	}
	aliases := regexp.MustCompile(`(?m)^alias sudo='sudo -A'$`).FindAllString(e2eReadFile(t, zshrc), -1)
	if len(aliases) != 1 {
		t.Errorf("alias appears %d times in .zshrc, want 1", len(aliases))
	}
}

func TestE2EUserStatusAndUninstallScanEveryStartupFile(t *testing.T) {
	h := newE2E(t)
	home := h.newHome("user-scan")
	if err := os.WriteFile(filepath.Join(home, ".profile"),
		[]byte("# sido start\nexport SUDO_ASKPASS=\"/profile/path\"\n# sido end\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".zshrc"),
		[]byte("# sido start\nexport SUDO_ASKPASS=\"/zsh/path\"\nalias sudo='sudo -A'\n# sido end\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + home, "SHELL=/bin/zsh"}

	status := h.runCli(env, "status", "--user")
	if status.status != 0 {
		t.Fatalf("status = %d\nstderr: %s", status.status, status.stderr)
	}
	mustMatch(t, `\.profile: "/profile/path"`, status.stderr, "status stderr")
	mustMatch(t, `\.zshrc: "/zsh/path"`, status.stderr, "status stderr")

	uninstall := h.runCli(env, "uninstall", "--user")
	if uninstall.status != 0 {
		t.Fatalf("uninstall = %d\nstderr: %s", uninstall.status, uninstall.stderr)
	}
	for _, name := range []string{".profile", ".zshrc"} {
		mustNotMatch(t, `# sido (?:start|end)`, e2eReadFile(t, filepath.Join(home, name)), name)
		mustMatch(t, regexp.QuoteMeta(name), uninstall.stderr, "uninstall stderr")
	}
}

func TestE2EInstallWithoutScopeReappliesExistingInstall(t *testing.T) {
	h := newE2E(t)
	home := h.newHome("install-detect")
	profile := filepath.Join(home, ".profile")
	if err := os.WriteFile(profile, []byte("export BEFORE=yes\n\n"+
		"# sido start\nexport SUDO_ASKPASS=\"/old/sido-askpass\"\n# sido end\n\n"+
		"export AFTER=yes\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := h.runCli([]string{"HOME=" + home}, "install")
	if res.status != 0 {
		t.Fatalf("install status = %d\nstderr: %s", res.status, res.stderr)
	}
	want := "export BEFORE=yes\n\n" +
		"# sido start\nexport SUDO_ASKPASS=\"" + h.askpassPath() + "\"\n# sido end\n\n" +
		"export AFTER=yes\n"
	if got := e2eReadFile(t, profile); got != want {
		t.Errorf("profile =\n%q\nwant\n%q", got, want)
	}
}

func TestE2EInstallWithoutScopeRequiresOneForFirstSetup(t *testing.T) {
	h := newE2E(t)
	home := h.newHome("install-new")

	res := h.runCli([]string{"HOME=" + home}, "install")
	if res.status != 1 {
		t.Errorf("status = %d, want 1\nstderr: %s", res.status, res.stderr)
	}
	mustMatch(t, `no existing installation found`, res.stderr, "stderr")
	mustMatch(t, `install --user\|--system`, res.stderr, "stderr")
}

func TestE2EStatusRejectsMutuallyExclusiveScopes(t *testing.T) {
	h := newE2E(t)

	res := h.runCli(nil, "status", "--user", "--system")
	if res.status != 1 {
		t.Errorf("status = %d, want 1\nstderr: %s", res.status, res.stderr)
	}
	mustMatch(t, `status \[--user\|--system\]`, res.stderr, "stderr")
}

func TestE2EStatusUserReportsInstalledAskpass(t *testing.T) {
	h := newE2E(t)
	home := h.newHome("status-user")
	env := []string{"HOME=" + home}

	if res := h.runCli(env, "install", "--user"); res.status != 0 {
		t.Fatalf("install status = %d\nstderr: %s", res.status, res.stderr)
	}
	res := h.runCli(env, "status", "--user")
	if res.status != 0 {
		t.Errorf("status = %d\nstderr: %s", res.status, res.stderr)
	}
	mustMatch(t, `user askpass in`, res.stderr, "status stderr")
}

// ── CLI surface ──────────────────────────────────────────────────────────────

func TestE2EVersionPrintsPackageVersion(t *testing.T) {
	h := newE2E(t)

	res := h.runCli(nil, "--version")
	if res.status != 0 {
		t.Fatalf("status = %d\nstderr: %s", res.status, res.stderr)
	}
	if got := strings.TrimSpace(res.stdout); got != h.version {
		t.Errorf("--version = %q, want %q (package.json)", got, h.version)
	}
}

func TestE2EHelpPrintsUsage(t *testing.T) {
	h := newE2E(t)

	res := h.runCli(nil, "--help")
	if res.status != 0 {
		t.Fatalf("status = %d\nstderr: %s", res.status, res.stderr)
	}
	for _, want := range []string{`Usage:`, `askpass mode`, `sido upgrade`} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("--help output is missing %q", want)
		}
	}
	for _, adapter := range []string{
		"auto", "tmux", "herdr", "osascript", "zenity", "kdialog", "tty", "watch",
	} {
		mustMatch(t, `(?m)^  `+adapter+` `, res.stdout, "--help adapters")
	}
}

// TestE2EAskpassPromptIsNeverParsedAsCLICommand guards the entry-point split:
// sudo passes the prompt as argv[1], so a prompt that looks like a flag must not
// be dispatched as a management command.
func TestE2EAskpassPromptIsNeverParsedAsCLICommand(t *testing.T) {
	h := newE2E(t)
	modeLog := filepath.Join(h.dir, "modes")

	res := h.runAskpass([]string{
		"TMUX=e2e",
		"SIDO_ADAPTER=tmux",
		"SIDO_E2E_MODE=success",
		"SIDO_E2E_MODE_LOG=" + modeLog,
	}, "--version")

	if res.status != 0 {
		t.Fatalf("status = %d\nstderr: %s", res.status, res.stderr)
	}
	if res.stdout != "fake-password" {
		t.Errorf("stdout = %q, want %q (prompt treated as a password request)", res.stdout, "fake-password")
	}
}

func TestE2EUnknownAdapterExitsWithAllowedValues(t *testing.T) {
	h := newE2E(t)

	res := h.runAskpass([]string{"SIDO_ADAPTER=nope"}, "Password: ")
	if res.status != 1 {
		t.Errorf("status = %d, want 1\nstdout: %s", res.status, res.stdout)
	}
	mustMatch(t, `unknown adapter "nope"`, res.stderr, "stderr")
	mustMatch(t, `auto, tmux, herdr, osascript`, res.stderr, "stderr")
}

func TestE2ERunRequiresSeparatorAndSetsAskpassEnv(t *testing.T) {
	h := newE2E(t)
	out := filepath.Join(h.dir, "run-env")

	res := h.runCli(nil, "run", "--adapter", "watch", "--", "bash", "-c",
		`printf '%s\n%s' "$SUDO_ASKPASS" "$SIDO_ADAPTER" > "`+out+`"`)
	if res.status != 0 {
		t.Fatalf("status = %d\nstderr: %s", res.status, res.stderr)
	}
	if got, want := e2eReadFile(t, out), h.askpassPath()+"\nwatch"; got != want {
		t.Errorf("run environment = %q, want %q", got, want)
	}

	missing := h.runCli(nil, "run", "true")
	if missing.status != 1 {
		t.Errorf("status = %d, want 1\nstderr: %s", missing.status, missing.stderr)
	}
	mustMatch(t, `run .* -- <command>`, missing.stderr, "stderr")
}

// ── upgrade ──────────────────────────────────────────────────────────────────

func TestE2EUpgradeInstallsLatestAndRefreshesConfig(t *testing.T) {
	h := newE2E(t)
	npmRoot := h.fakeNpmRoot()
	npmLog := filepath.Join(h.dir, "npm-args")
	h.writeNpmStub("#!/bin/sh\n" +
		"if [ \"$1\" = root ]; then printf '%s\\n' '" + npmRoot + "'; exit 0; fi\n" +
		"if [ \"$1\" = view ]; then printf '[\"" + h.newerVersion() + "\"]\\n'; exit 0; fi\n" +
		"printf '%s\\n' \"$@\" > \"$SIDO_E2E_NPM_LOG\"\n")

	home := h.newHome("upgrade")
	env := []string{"HOME=" + home}
	if res := h.runCli(env, "install", "--user"); res.status != 0 {
		t.Fatalf("install status = %d\nstderr: %s", res.status, res.stderr)
	}

	res := h.runCli(append(env, "SIDO_E2E_NPM_LOG="+npmLog), "upgrade")
	if res.status != 0 {
		t.Fatalf("upgrade status = %d\nstderr: %s", res.status, res.stderr)
	}
	if got, want := e2eReadFile(t, npmLog), "install\n--global\nsido-askpass@latest\n"; got != want {
		t.Errorf("npm args = %q, want %q", got, want)
	}
	mustMatch(t, `(?m)^# sido start\nexport SUDO_ASKPASS=`, e2eReadFile(t, filepath.Join(home, ".profile")), "profile")
	mustMatch(t, `refreshing managed configuration`, res.stderr, "upgrade stderr")
}

func TestE2EUpgradeDoesNotChangeConfigWhenNpmFails(t *testing.T) {
	h := newE2E(t)
	npmRoot := h.fakeNpmRoot()
	h.writeNpmStub("#!/bin/sh\n" +
		"if [ \"$1\" = root ]; then printf '%s\\n' '" + npmRoot + "'; exit 0; fi\n" +
		"if [ \"$1\" = view ]; then printf '\"" + h.newerVersion() + "\"\\n'; exit 0; fi\n" +
		"exit 23\n")

	home := h.newHome("upgrade-failure")
	res := h.runCli([]string{"HOME=" + home}, "upgrade")
	if res.status != 23 {
		t.Errorf("status = %d, want 23\nstderr: %s", res.status, res.stderr)
	}
	mustMatch(t, `npm upgrade failed with status 23`, res.stderr, "stderr")
	if _, err := os.Stat(filepath.Join(home, ".profile")); !os.IsNotExist(err) {
		t.Errorf(".profile should not exist after a failed upgrade (err=%v)", err)
	}
}

func TestE2EUpgradeSkipsNpmWhenAlreadyCurrent(t *testing.T) {
	h := newE2E(t)
	npmRoot := h.fakeNpmRoot()
	npmLog := filepath.Join(h.dir, "npm-current-args")
	h.writeNpmStub("#!/bin/sh\n" +
		"if [ \"$1\" = root ]; then printf '%s\\n' '" + npmRoot + "'; exit 0; fi\n" +
		"printf '%s\\n' \"$@\" >> \"$SIDO_E2E_NPM_LOG\"\n" +
		"if [ \"$1\" = view ]; then printf '\"" + h.version + "\"\\n'; fi\n")

	home := h.newHome("upgrade-current")
	res := h.runCli([]string{"HOME=" + home, "SIDO_E2E_NPM_LOG=" + npmLog}, "upgrade")
	if res.status != 0 {
		t.Fatalf("status = %d\nstderr: %s", res.status, res.stderr)
	}
	mustMatch(t, `already up to date`, res.stderr, "stderr")
	if got, want := e2eReadFile(t, npmLog), "view\nsido-askpass@latest\nversion\n--json\n"; got != want {
		t.Errorf("npm args = %q, want %q (no install should run)", got, want)
	}
	if _, err := os.Stat(filepath.Join(home, ".profile")); !os.IsNotExist(err) {
		t.Errorf(".profile should not exist when already current (err=%v)", err)
	}
}

func TestE2EUpgradeRejectsNonNpmInstall(t *testing.T) {
	h := newE2E(t)
	otherRoot := filepath.Join(h.dir, "other-npm-root")
	if err := os.MkdirAll(otherRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	h.writeNpmStub("#!/bin/sh\nprintf '%s\\n' '" + otherRoot + "'\n")

	home := h.newHome("upgrade-non-npm")
	res := h.runCli([]string{"HOME=" + home}, "upgrade")
	if res.status != 1 {
		t.Errorf("status = %d, want 1\nstderr: %s", res.status, res.stderr)
	}
	mustMatch(t, `npm global installations only`, res.stderr, "stderr")
}

// ── watch mode ───────────────────────────────────────────────────────────────

func TestE2EWatchTimesOutAndHintsWhenNoApproverConnects(t *testing.T) {
	h := newE2E(t)
	runtime := filepath.Join(h.dir, "runtime-timeout")
	if err := os.MkdirAll(runtime, 0o700); err != nil {
		t.Fatal(err)
	}

	res := h.runAskpass([]string{
		"XDG_RUNTIME_DIR=" + runtime,
		"SIDO_ADAPTER=watch",
		"SIDO_WATCH_TIMEOUT=1",
	}, "Password: ")

	if res.status != 1 {
		t.Errorf("status = %d, want 1\nstdout: %s", res.status, res.stdout)
	}
	mustMatch(t, `from another terminal run: .* approve`, res.stderr, "stderr")
	mustMatch(t, `timed out waiting for approver`, res.stderr, "stderr")
	entries, err := os.ReadDir(filepath.Join(runtime, "sido"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("request directories left behind: %v", entries)
	}
}

func TestE2EWatchHandsPasswordThroughFifoToApprover(t *testing.T) {
	h := newE2E(t)
	runtime := filepath.Join(h.dir, "runtime-watch")
	if err := os.MkdirAll(runtime, 0o700); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(h.askpassPath(), "Password: ")
	cmd.Env = h.env("XDG_RUNTIME_DIR="+runtime, "SIDO_ADAPTER=watch", "SIDO_WATCH_TIMEOUT=10")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()

	var fifo string
	waitUntil(t, 10*time.Second, "request FIFO to appear", func() bool {
		matches, _ := filepath.Glob(filepath.Join(runtime, "sido", "sido-*", "password"))
		if len(matches) == 0 {
			return false
		}
		fifo = matches[0]
		return true
	})

	if err := writeFifoPassword(fifo, "e2e-secret"); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("askpass exited with error: %v\nstderr: %s", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("askpass did not exit after the password was written\nstderr: %s", stderr.String())
	}
	if got, want := stdout.String(), "e2e-secret"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	entries, err := os.ReadDir(filepath.Join(runtime, "sido"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("request directories left behind: %v", entries)
	}
}

// writeFifoPassword feeds a password to a parked request, mirroring what
// `sido approve` does after reading the secret from the terminal.
func writeFifoPassword(fifo, password string) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_, werr := f.WriteString(password + "\n")
			f.Close()
			return werr
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("opening FIFO %s: %w", fifo, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestE2EApproveReportsNothingWhenNoRequestsPending(t *testing.T) {
	h := newE2E(t)
	runtime := filepath.Join(h.dir, "runtime-empty")
	if err := os.MkdirAll(runtime, 0o700); err != nil {
		t.Fatal(err)
	}

	res := h.runCli([]string{"XDG_RUNTIME_DIR=" + runtime}, "approve")
	if res.status != 0 {
		t.Errorf("status = %d, want 0\nstderr: %s", res.status, res.stderr)
	}
	mustMatch(t, `no pending password requests`, res.stderr, "stderr")
}
