package sido

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"golang.org/x/sys/unix"
)

var Version = "0.0.0-dev"

// ── paths ─────────────────────────────────────────────────────────────────────

// selfPath is the real per-OS binary (sido-mac / sido-linux) — used to spawn
// the inner prompt receiver directly and for the npm-package path check.
func selfPath() string {
	exe, err := os.Executable()
	if err != nil {
		exe, _ = filepath.Abs(os.Args[0])
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved
	}
	return exe
}

// cliPath / askpassPath are the OS-detecting shell launchers that live next to
// the binary. cliPath is what the user invokes ("sido ..."); askpassPath is what
// sudo execs via SUDO_ASKPASS.
func cliPath() string {
	return filepath.Join(filepath.Dir(selfPath()), "sido")
}

func askpassPath() string {
	return filepath.Join(filepath.Dir(selfPath()), "sido-askpass")
}

// ── adapters ──────────────────────────────────────────────────────────────────

var adapters = []string{
	"auto", "tmux", "herdr", "osascript", "zenity", "kdialog", "tty", "watch",
}

func isAdapter(name string) bool {
	for _, a := range adapters {
		if a == name {
			return true
		}
	}
	return false
}

// ── spawn helpers ─────────────────────────────────────────────────────────────

type spawnCfg struct {
	stdin  string // "inherit", "ignore", "pipe"
	stdout string
	stderr string
	input  string
	env    []string
}

type spawnRes struct {
	stdout []byte
	stderr []byte
	status int
	err    error
	signum syscall.Signal
}

func spawn(command string, args []string, cfg spawnCfg) spawnRes {
	if cfg.stdin == "" {
		cfg.stdin = "pipe"
	}
	if cfg.stdout == "" {
		cfg.stdout = "pipe"
	}
	if cfg.stderr == "" {
		cfg.stderr = "pipe"
	}

	cmd := exec.Command(command, args...)

	switch cfg.stdin {
	case "inherit":
		cmd.Stdin = os.Stdin
	case "ignore":
		cmd.Stdin = bytes.NewReader(nil)
	case "pipe":
		if cfg.input != "" {
			cmd.Stdin = strings.NewReader(cfg.input)
		} else {
			cmd.Stdin = nil
		}
	}

	var outBuf, errBuf bytes.Buffer
	switch cfg.stdout {
	case "inherit":
		cmd.Stdout = os.Stdout
	case "ignore":
		cmd.Stdout = io.Discard
	case "pipe":
		cmd.Stdout = &outBuf
	}
	switch cfg.stderr {
	case "inherit":
		cmd.Stderr = os.Stderr
	case "ignore":
		cmd.Stderr = io.Discard
	case "pipe":
		cmd.Stderr = &errBuf
	}

	if cfg.env != nil {
		cmd.Env = cfg.env
	}

	runErr := cmd.Run()

	res := spawnRes{
		stdout: outBuf.Bytes(),
		stderr: errBuf.Bytes(),
		status: -1,
	}

	if cmd.ProcessState != nil {
		res.status = cmd.ProcessState.ExitCode()
		if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				res.signum = ws.Signal()
				res.status = -1
			}
		}
	}

	if runErr != nil && cmd.ProcessState == nil {
		res.err = runErr
	}

	return res
}

// stripTrailingNewline removes a single trailing "\r\n", "\n", or "\r".
func stripTrailingNewline(s string) string {
	if len(s) > 0 && s[len(s)-1] == '\n' {
		s = s[:len(s)-1]
	}
	if len(s) > 0 && s[len(s)-1] == '\r' {
		s = s[:len(s)-1]
	}
	return s
}

// trimEnd mirrors JS String.prototype.trimEnd (strips trailing whitespace).
func trimEnd(s string) string {
	return strings.TrimRightFunc(s, unicode.IsSpace)
}

// ── readSecret (native terminal via termios + x/sys) ─────────────────────────

func readSecret(prompt string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	defer tty.Close()

	restore, err := ttyEchoOff(int(tty.Fd()))
	if err != nil {
		return "", err
	}
	defer restore()

	// Restore echo if the user interrupts so the terminal isn't left hidden.
	// signal.Stop is essential: a leaked registration would keep suppressing
	// the default terminate disposition (breaking Ctrl-C in `sido watch`).
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigc)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case s := <-sigc:
			restore()
			os.Exit(128 + int(s.(syscall.Signal)))
		case <-done:
		}
	}()

	if _, err := tty.WriteString(prompt); err != nil {
		return "", err
	}
	pw, err := readHiddenLine(tty)
	tty.WriteString("\n")
	return pw, err
}

// readHiddenLine reads one line from a canonical-mode tty with echo disabled. A
// genuine EOF (Ctrl-D / closed tty) returns io.EOF so the caller can fall
// through to another adapter, while pressing Enter with nothing typed returns an
// empty password — matching what the old `bash read -s` did.
func readHiddenLine(tty *os.File) (string, error) {
	var line []byte
	var one [1]byte
	for {
		n, err := tty.Read(one[:])
		if n > 0 {
			// Canonical ttys deliver '\n' on Enter; raw/cbreak ttys deliver
			// '\r' (ICRNL off). Accept both as end-of-line.
			if one[0] == '\n' || one[0] == '\r' {
				return string(line), nil
			}
			line = append(line, one[0])
			continue
		}
		if err != nil {
			return "", err
		}
		return "", io.EOF // n == 0, no error → EOF
	}
}

// ── FIFO helpers ─────────────────────────────────────────────────────────────

var errSurfaceCancelled = errors.New("password surface closed without supplying a password")

type readFifoResult struct {
	pw        string
	cancelled bool
	err       error
}

// readFifoPassword reads one newline-terminated password line from the FIFO. The
// FIFO is opened O_RDONLY|O_NONBLOCK (open never blocks) and read with
// non-blocking probes so the kernel distinguishes the writer's state:
//   - EAGAIN       → a writer is connected but has no data yet (still typing)
//   - data (>0)    → accumulate until newline → password
//   - EOF (n==0)   → no writer; if we had seen the writer, it exited without
//     writing = the surface was closed/cancelled → cancelled=true.
//     EOF before the writer ever connects is "not started yet",
//     so the startup window is not mistaken for a cancel.
//
// ctx cancellation surfaces as ctx.Err(). This replaces the O_RDWR trick (which
// masked EOF because we held our own write end) — close is now always detected.
func readFifoPassword(fifo string, ctx context.Context) (string, bool, error) {
	f, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	fd := int(f.Fd())
	connected := false
	var buf bytes.Buffer
	b := make([]byte, 4096)
	for {
		if i := bytes.IndexByte(buf.Bytes(), '\n'); i >= 0 {
			return strings.TrimRight(string(buf.Bytes()[:i]), "\r"), false, nil
		}
		n, rerr := unix.Read(fd, b)
		if n > 0 {
			connected = true
			buf.Write(b[:n])
			continue
		}
		if rerr != nil {
			if errors.Is(rerr, syscall.EAGAIN) {
				connected = true // writer present, no data yet
			} else {
				return "", false, rerr
			}
		} else {
			// n == 0, no error → EOF. A writer that connected then left is a
			// cancel — unless it wrote something first, in which case hand back
			// what we got rather than discarding it.
			if connected && buf.Len() == 0 {
				return "", true, nil
			}
			if connected {
				return strings.TrimRight(buf.String(), "\r\n"), false, nil
			}
			// else: writer hasn't connected yet; keep waiting
		}
		select {
		case <-ctx.Done():
			return "", false, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// promptViaSurface runs runUI (which spawns the receiver inside the UI surface)
// while draining the FIFO. It returns the password, or errSurfaceCancelled if
// the surface (popup/pane) was closed without supplying one. runUI may block
// until the surface closes (tmux -E) or return immediately (herdr pane run is
// fire-and-forget); completion is detected via the FIFO itself, not runUI.
func promptViaSurface(fifo string, runUI func() error) (string, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // stop the FIFO reader if we bail via the runUI error path
	pwCh := make(chan readFifoResult, 1)
	go func() {
		pw, cancelled, err := readFifoPassword(fifo, ctx)
		pwCh <- readFifoResult{pw, cancelled, err}
	}()
	uiErr := make(chan error, 1)
	go func() { uiErr <- runUI() }()
	for {
		select {
		case r := <-pwCh:
			if r.err != nil {
				return "", r.err
			}
			if r.cancelled {
				return "", errSurfaceCancelled
			}
			return r.pw, nil
		case e := <-uiErr:
			if e != nil {
				return "", e // surface failed to spawn
			}
			uiErr = nil // surface reported success; keep waiting on the fifo
		}
	}
}

// writeToFifo writes password to a FIFO with O_NONBLOCK + retry loop, so it
// doesn't block forever if the shim already exited.
func writeToFifo(fifo, password string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			// Switch to blocking mode for the write so it completes atomically
			// (the runtime retries EAGAIN/EINTR internally) — no partial write.
			syscall.SetNonblock(int(f.Fd()), false)
			_, werr := fmt.Fprintf(f, "%s\n", password)
			f.Close()
			return werr
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ── inner prompt receiver ───────────────────────────────────────────────────

func innerPromptReceiver(promptFile, fifo, dir, shimPid, paneID string) {
	pid, _ := strconv.Atoi(shimPid)
	cleanup := func() {
		if dir != "" {
			os.RemoveAll(dir)
		}
		if paneID != "" {
			exec.Command("herdr", "pane", "close", paneID).Run()
		}
	}

	// Orphan watchdog: if the askpass shim dies, clean up and exit so the
	// popup/pane closes.
	if pid > 0 {
		go func() {
			for {
				time.Sleep(200 * time.Millisecond)
				if syscall.Kill(pid, 0) != nil {
					cleanup()
					os.Exit(0)
				}
			}
		}()
	}

	data, err := os.ReadFile(promptFile)
	if err != nil {
		cleanup()
		os.Exit(1)
	}

	// Open (and hold) the FIFO write end BEFORE prompting. If this process is
	// killed while the user is typing — pane/popup closed, SIGKILL — the kernel
	// closes the fd, so the shim sees EOF-after-connected and treats it as a
	// cancel instead of waiting forever. (O_WRONLY blocks only until the shim's
	// read end opens, which it does up front.)
	w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		cleanup()
		os.Exit(1)
	}
	defer w.Close()

	pw, err := readSecret(string(data))
	if err != nil {
		cleanup()
		os.Exit(1)
	}

	fmt.Fprintf(w, "%s\n", pw)
	cleanup()
}

func getRequestingCommand() string {
	ppid := os.Getppid()
	var value string
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", ppid))
		if err != nil {
			return ""
		}
		value = strings.ReplaceAll(string(data), "\x00", " ")
	} else {
		res := spawn("ps", []string{"-o", "command=", "-p", strconv.Itoa(ppid)}, spawnCfg{})
		if res.status != 0 {
			return ""
		}
		value = string(res.stdout)
	}

	re := regexp.MustCompile(`[\x00-\x1f\x7f-\x9f]`)
	value = re.ReplaceAllString(value, " ")
	re2 := regexp.MustCompile(`\s+`)
	value = re2.ReplaceAllString(value, " ")
	value = strings.TrimSpace(value)
	return value
}

// ── interactiveShellTty (native termios) ────────────────────────────────────

func interactiveShellTty() bool {
	tty, err := os.Open("/dev/tty")
	if err != nil {
		return false
	}
	defer tty.Close()
	echo, icanon, err := ttyFlags(int(tty.Fd()))
	if err != nil {
		return false
	}
	return echo && icanon
}

// ── prompt resources (FIFO) ──────────────────────────────────────────────────

type promptResources struct {
	dir        string
	fifo       string
	promptFile string
}

func createPromptResources(base string) (promptResources, error) {
	dir, err := os.MkdirTemp(base, "sido-")
	if err != nil {
		return promptResources{}, fmt.Errorf("mkdtemp failed: %w", err)
	}
	fifo := filepath.Join(dir, "password")

	if err := unix.Mkfifo(fifo, 0600); err != nil {
		os.RemoveAll(dir)
		return promptResources{}, fmt.Errorf("mkfifo failed: %s", err)
	}
	os.Chmod(fifo, 0600)

	resources := promptResources{
		dir:        dir,
		fifo:       fifo,
		promptFile: filepath.Join(dir, "prompt"),
	}

	if err := os.WriteFile(resources.promptFile, []byte(displayPrompt), 0600); err != nil {
		os.RemoveAll(dir)
		return promptResources{}, err
	}
	return resources, nil
}

func removePromptResources(r promptResources) {
	os.RemoveAll(r.dir)
}

// ── tmux ─────────────────────────────────────────────────────────────────────

func tmuxPrompt() {
	check := spawn("tmux", []string{"display-message", "-p", "#S"}, spawnCfg{})
	if check.status != 0 {
		detail := strings.TrimSpace(string(check.stderr))
		if detail != "" {
			fmt.Fprintln(os.Stderr, detail)
		}
		fmt.Fprintln(os.Stderr, "[sido] tmux access denied; retry the sudo command with escalated permissions")
		os.Exit(1)
	}

	resources, err := createPromptResources(os.TempDir())
	if err != nil {
		fmt.Fprintf(os.Stderr, "[sido] %s\n", err)
		os.Exit(1)
	}
	defer removePromptResources(resources)

	self := selfPath()
	cmdString := `"$SIDO_BIN" _inner_prompt_receiver "$SIDO_PROMPT" "$SIDO_FIFO" "$SIDO_DIR" "$SIDO_SHIM_PID" ""`

	pw, err := promptViaSurface(resources.fifo, func() error {
		return exec.Command("tmux", "display-popup", "-E", "-w", "60%", "-h", "5",
			"-e", "SIDO_BIN="+self,
			"-e", "SIDO_PROMPT="+resources.promptFile,
			"-e", "SIDO_FIFO="+resources.fifo,
			"-e", "SIDO_DIR="+resources.dir,
			"-e", "SIDO_SHIM_PID="+strconv.Itoa(os.Getpid()),
			cmdString,
		).Run()
	})
	removePromptResources(resources)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[sido] %s\n", err)
		os.Exit(1)
	}
	os.Stdout.WriteString(pw)
}

// ── Herdr ────────────────────────────────────────────────────────────────────

func herdrSplitDirection() string {
	res := spawn("herdr", []string{"pane", "layout", "--current"}, spawnCfg{
		stdin: "inherit", stdout: "pipe", stderr: "pipe",
	})
	if res.status == 0 {
		var info struct {
			Result struct {
				Pane struct {
					Width float64 `json:"width"`
				} `json:"pane"`
			} `json:"result"`
		}
		if err := json.Unmarshal(res.stdout, &info); err == nil {
			if info.Result.Pane.Width <= 150 {
				return "down"
			}
		}
	}
	return "right"
}

func createHerdrPane(direction string) string {
	res := spawn("herdr", []string{
		"pane", "split", "--current", "--direction", direction, "--cwd", "/",
	}, spawnCfg{stdin: "inherit", stdout: "pipe", stderr: "pipe"})
	if res.status != 0 {
		return ""
	}
	var info struct {
		Result struct {
			Pane struct {
				PaneID string `json:"pane_id"`
			} `json:"pane"`
		} `json:"result"`
	}
	if err := json.Unmarshal(res.stdout, &info); err != nil {
		return ""
	}
	return info.Result.Pane.PaneID
}

func closeHerdrPane(paneID string) {
	spawn("herdr", []string{"pane", "close", paneID}, spawnCfg{})
}

func herdrPrompt(allowFallback bool) {
	direction := herdrSplitDirection()
	label := "right"
	if direction == "down" {
		label = "bottom"
	}
	fmt.Fprintf(os.Stderr, "[sido] password requested — see new pane (%s)\n", label)

	paneID := createHerdrPane(direction)
	if paneID == "" {
		fallbackHerdr(allowFallback)
		return
	}

	resources, err := createPromptResources(os.TempDir())
	if err != nil {
		closeHerdrPane(paneID)
		fmt.Fprintf(os.Stderr, "[sido] %s\n", err)
		os.Exit(1)
	}

	args := []string{
		"pane", "run", paneID, selfPath(), "_inner_prompt_receiver",
		resources.promptFile, resources.fifo, resources.dir,
		strconv.Itoa(os.Getpid()), paneID,
	}
	pw, pwErr := promptViaSurface(resources.fifo, func() error {
		return exec.Command("herdr", args...).Run()
	})

	removePromptResources(resources)
	closeHerdrPane(paneID)

	if pwErr != nil {
		fmt.Fprintf(os.Stderr, "[sido] %s\n", pwErr)
		os.Exit(1)
	}
	os.Stdout.WriteString(pw)
}

func fallbackHerdr(allowFallback bool) {
	if !allowFallback {
		adapterUnavailable("herdr", "herdr pane split failed")
	}
	fmt.Fprintln(os.Stderr, "[sido] herdr pane split failed, falling back")
	if canGui {
		guiPrompt()
		return
	}
	if ttyPrompt() {
		return
	}
	watchPrompt()
}

// ── GUI ──────────────────────────────────────────────────────────────────────

func guiPrompt() {
	if isMac {
		macGui()
		return
	}
	linuxGui()
}

func macGui() {
	res := spawn("osascript", []string{
		"-e", "on run argv",
		"-e", `display dialog (item 1 of argv) with title (item 2 of argv) with icon caution with hidden answer default answer "" buttons {"Cancel", "Authenticate"} default button "Authenticate" cancel button "Cancel"`,
		"-e", "text returned of result",
		"-e", "end run",
		displayPrompt,
		"Administrator Authentication",
	}, spawnCfg{stdin: "inherit", stdout: "pipe", stderr: "inherit"})
	if res.status != 0 {
		os.Exit(1)
	}
	os.Stdout.WriteString(stripTrailingNewline(string(res.stdout)))
}

func linuxGui() {
	progs := [][]string{
		{"zenity", "--password", "--title", displayPrompt},
		{"kdialog", "--password", displayPrompt},
	}
	for _, prog := range progs {
		if guiProgramPrompt(prog[0], prog[1:]) {
			return
		}
	}
	fmt.Fprintln(os.Stderr, "[sido] no GUI dialog found")
	if ttyPrompt() {
		return
	}
	watchPrompt()
}

func exactGuiPrompt(command string, args []string) {
	if !guiProgramPrompt(command, args) {
		adapterUnavailable(command, command+" failed or was cancelled")
	}
}

func guiProgramPrompt(command string, args []string) bool {
	res := spawn(command, args, spawnCfg{
		stdin: "inherit", stdout: "pipe", stderr: "inherit",
	})
	if res.status != 0 {
		return false
	}
	out := string(res.stdout)
	out = strings.TrimSuffix(out, "\n")
	os.Stdout.WriteString(out)
	return true
}

// ── TTY ──────────────────────────────────────────────────────────────────────

func ttyPrompt() bool {
	pw, err := readSecret(displayPrompt)
	if err != nil {
		return false
	}
	os.Stdout.WriteString(pw)
	return true
}

// ── Watch mode ───────────────────────────────────────────────────────────────

func sidoDir() string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".cache")
	}
	dir := filepath.Join(base, "sido")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		os.MkdirAll(dir, 0700)
		os.Chmod(dir, 0700)
	}
	return dir
}

func watchTimeoutMs() time.Duration {
	raw := strings.TrimSpace(os.Getenv("SIDO_WATCH_TIMEOUT"))
	if raw != "" {
		val, err := strconv.ParseFloat(raw, 64)
		if err == nil && !math.IsNaN(val) && !math.IsInf(val, 0) && val > 0 {
			return time.Duration(val*1000) * time.Millisecond
		}
	}
	return 120000 * time.Millisecond
}

func watchPrompt() {
	resources, err := createPromptResources(sidoDir())
	if err != nil {
		fmt.Fprintf(os.Stderr, "[sido] %s\n", err)
		os.Exit(1)
	}

	timeout := watchTimeoutMs()
	reqMsg := "[sido] password requested"
	if requestingCommand != "" {
		reqMsg += fmt.Sprintf(" for: %s", requestingCommand)
	}
	fmt.Fprintln(os.Stderr, reqMsg)
	fmt.Fprintf(os.Stderr, "[sido] from another terminal run: %s approve  (waiting up to %.0fs)\n",
		cliPath(), timeout.Seconds())

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	pw, cancelled, err := readFifoPassword(resources.fifo, ctx)
	removePromptResources(resources)

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		fmt.Fprintln(os.Stderr, "[sido] timed out waiting for approver")
		os.Exit(1)
	case err != nil:
		fmt.Fprintf(os.Stderr, "[sido] could not read approval: %s\n", err)
		os.Exit(1)
	case cancelled:
		fmt.Fprintln(os.Stderr, "[sido] approver disconnected without a password")
		os.Exit(1)
	}
	os.Stdout.WriteString(pw)
}

func doWatch() {
	sd := sidoDir()
	fmt.Fprintf(os.Stderr, "[sido] watching %s for password requests (Ctrl-C to exit)\n", sd)
	for {
		req := newestRequest()
		if req != "" {
			handleRequest(req)
		} else {
			time.Sleep(1 * time.Second)
		}
	}
}

func doApprove() {
	req := newestRequest()
	if req == "" {
		fmt.Fprintln(os.Stderr, "[sido] no pending password requests")
		os.Exit(0)
	}
	handleRequest(req)
}

func newestRequest() string {
	entries, err := os.ReadDir(sidoDir())
	if err != nil {
		return ""
	}
	var newest string
	var newestMtime int64 = -1
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "sido-") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		mtime := info.ModTime().UnixMilli()
		if mtime > newestMtime {
			newestMtime = mtime
			newest = filepath.Join(sidoDir(), entry.Name())
		}
	}
	return newest
}

func handleRequest(reqDir string) {
	defer os.RemoveAll(reqDir) // always drop the request, success or not

	promptText := "[sudo] password: "
	if data, err := os.ReadFile(filepath.Join(reqDir, "prompt")); err == nil {
		promptText = string(data)
	}

	password, err := readSecret(promptText)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[sido] no password entered")
		os.RemoveAll(reqDir) // os.Exit skips the deferred cleanup
		os.Exit(1)
	}

	err = writeToFifo(filepath.Join(reqDir, "password"), password, 5*time.Second)
	if err == nil {
		fmt.Fprintln(os.Stderr, "[sido] password sent")
	} else {
		fmt.Fprintln(os.Stderr, "[sido] request expired")
	}
}

// ── install / uninstall / status ─────────────────────────────────────────────

func sudoConfPath() string {
	return "/etc/sudo.conf"
}

func userProfileNames() []string {
	return []string{".profile", ".bash_profile", ".bash_login", ".bashrc", ".zprofile", ".zshenv", ".zshrc"}
}

func userProfilePaths() []string {
	home, _ := os.UserHomeDir()
	paths := make([]string, 0, 7)
	for _, name := range userProfileNames() {
		paths = append(paths, filepath.Join(home, name))
	}
	return paths
}

func userProfilePath() string {
	home, _ := os.UserHomeDir()
	shell := filepath.Base(os.Getenv("SHELL"))
	if shell == "zsh" {
		return filepath.Join(home, ".zshrc")
	}
	if shell == "bash" {
		return filepath.Join(home, ".bashrc")
	}
	return filepath.Join(home, ".profile")
}

var managedBlockPattern = regexp.MustCompile(`(?ms)^# sido start\n.*?^# sido end\n?`)

func managedUserInstall(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	content := string(data)
	blockMatch := managedBlockPattern.FindString(content)
	if blockMatch == "" {
		return ""
	}
	re := regexp.MustCompile(`(?m)^export SUDO_ASKPASS=(.*)$`)
	sub := re.FindStringSubmatch(blockMatch)
	if len(sub) >= 2 {
		return sub[1]
	}
	return ""
}

func removeManagedUserInstall(content string) string {
	content = managedBlockPattern.ReplaceAllString(content, "")
	re3 := regexp.MustCompile(`\n{3,}`)
	content = re3.ReplaceAllString(content, "\n\n")
	return trimEnd(content)
}

func hasManagedUserInstall() bool {
	for _, p := range userProfilePaths() {
		if managedUserInstall(p) != "" {
			return true
		}
	}
	return false
}

func hasManagedSystemInstall() bool {
	data, err := os.ReadFile(sudoConfPath())
	if err != nil {
		return false
	}
	return regexp.MustCompile(`(?m)^# sido\nPath askpass .*$`).Match(data)
}

func doInstall(scope string) {
	if scope == "" {
		var scopes []string
		if hasManagedUserInstall() {
			scopes = append(scopes, "--user")
		}
		if hasManagedSystemInstall() {
			scopes = append(scopes, "--system")
		}
		if len(scopes) == 0 {
			fmt.Fprintf(os.Stderr, "[sido] no existing installation found; use: %s install --user|--system\n", cliPath())
			os.Exit(1)
		}
		for _, s := range scopes {
			doInstall(s)
		}
		return
	}

	if scope == "--user" {
		installUser()
	} else {
		installSystem()
	}
}

func installUser() {
	targetPath := userProfilePath()
	ap := askpassPath()
	line := fmt.Sprintf(`export SUDO_ASKPASS="%s"`, ap)
	aliasLine := ""
	if filepath.Base(targetPath) == ".zshrc" {
		aliasLine = "\nalias sudo='sudo -A'"
	}
	managedBlock := fmt.Sprintf("# sido start\n%s%s\n# sido end\n", line, aliasLine)

	var migratedPaths []string
	for _, p := range userProfilePaths() {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			continue
		}
		data, _ := os.ReadFile(p)
		content := string(data)
		if !managedBlockPattern.MatchString(content) {
			continue
		}
		if p != targetPath {
			os.WriteFile(p, []byte(removeManagedUserInstall(content)+"\n"), 0644)
			migratedPaths = append(migratedPaths, p)
		}
	}

	var targetContent string
	if data, err := os.ReadFile(targetPath); err == nil {
		targetContent = string(data)
	}
	unmanagedContent := removeManagedUserInstall(targetContent)

	exportRe := regexp.MustCompile(`(?m)^export SUDO_ASKPASS=(.*)$`)
	for _, m := range exportRe.FindAllStringSubmatch(unmanagedContent, -1) {
		fmt.Fprintf(os.Stderr, "[sido] warning: preserving unmanaged SUDO_ASKPASS=%s in %s\n", m[1], targetPath)
	}

	if managedBlockPattern.MatchString(targetContent) {
		newContent := managedBlockPattern.ReplaceAllString(targetContent, managedBlock)
		os.WriteFile(targetPath, []byte(newContent), 0644)
	} else {
		normalized := trimEnd(targetContent)
		separator := ""
		if normalized != "" {
			separator = "\n\n"
		}
		os.WriteFile(targetPath, []byte(normalized+separator+managedBlock), 0644)
	}

	for _, p := range migratedPaths {
		fmt.Fprintf(os.Stderr, "[sido] migrated user configuration from %s\n", p)
	}
	fmt.Fprintf(os.Stderr, "[sido] installed to %s\n", targetPath)
	fmt.Fprintln(os.Stderr, "[sido] restart your shell, or run:")
	fmt.Fprintf(os.Stderr, "[sido] export SUDO_ASKPASS=\"%s\"\n", ap)
}

func installSystem() {
	path := sudoConfPath()
	ap := askpassPath()
	line := fmt.Sprintf("Path askpass %s", ap)
	var content string
	if data, err := os.ReadFile(path); err == nil {
		content = string(data)
	}
	managedPattern := regexp.MustCompile(`(?m)^# sido\nPath askpass .*$`)
	if managedPattern.MatchString(content) {
		content = managedPattern.ReplaceAllString(content, "# sido\n"+line)
	} else if regexp.MustCompile(`(?m)^Path askpass `).MatchString(content) {
		oldRe := regexp.MustCompile(`(?m)^Path askpass (.*)$`)
		for _, m := range oldRe.FindAllStringSubmatch(content, -1) {
			fmt.Fprintf(os.Stderr, "[sido] warning: replacing Path askpass %s with \"%s\"\n", m[1], ap)
		}
		content = regexp.MustCompile(`(?m)^Path askpass .*$`).ReplaceAllString(content, "# sido\n"+line)
	} else {
		content += fmt.Sprintf("\n# sido\n%s\n", line)
	}
	res := spawn("sudo", []string{"tee", path}, spawnCfg{
		stdin: "pipe", input: content, stdout: "inherit", stderr: "inherit",
	})
	if res.status == 0 {
		fmt.Fprintf(os.Stderr, "[sido] installed to %s\n", path)
	} else {
		fmt.Fprintln(os.Stderr, "[sido] install failed — do you have sudo?")
		os.Exit(1)
	}
}

func normalizeUninstalledContent(content string) string {
	re := regexp.MustCompile(`\n{3,}`)
	return strings.TrimSpace(re.ReplaceAllString(content, "\n\n")) + "\n"
}

func doUninstall(scope string) {
	if scope == "--user" {
		var removedPaths []string
		for _, p := range userProfilePaths() {
			if _, err := os.Stat(p); os.IsNotExist(err) {
				continue
			}
			data, _ := os.ReadFile(p)
			content := string(data)
			if !managedBlockPattern.MatchString(content) {
				continue
			}
			cleaned := managedBlockPattern.ReplaceAllString(content, "")
			os.WriteFile(p, []byte(normalizeUninstalledContent(cleaned)), 0644)
			removedPaths = append(removedPaths, p)
		}
		if len(removedPaths) == 0 {
			fmt.Fprintln(os.Stderr, "[sido] nothing to uninstall")
			os.Exit(1)
		}
		for _, p := range removedPaths {
			fmt.Fprintf(os.Stderr, "[sido] removed from %s\n", p)
		}
	} else {
		path := sudoConfPath()
		if _, err := os.Stat(path); os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "[sido] nothing to uninstall")
			os.Exit(1)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "[sido] nothing to uninstall")
			os.Exit(1)
		}
		content := string(data)
		content = regexp.MustCompile(`(?m)^# sido\n`).ReplaceAllString(content, "")
		content = regexp.MustCompile(`(?m)^Path askpass .*$`).ReplaceAllString(content, "")
		teeRes := spawn("sudo", []string{"tee", path}, spawnCfg{
			stdin: "pipe", input: normalizeUninstalledContent(content), stdout: "inherit", stderr: "inherit",
		})
		if teeRes.status == 0 {
			fmt.Fprintf(os.Stderr, "[sido] removed from %s\n", path)
		} else {
			fmt.Fprintln(os.Stderr, "[sido] uninstall failed")
			os.Exit(1)
		}
	}
}

func doStatus(scope string) {
	if scope == "" || scope == "--system" {
		sysPath := sudoConfPath()
		if data, err := os.ReadFile(sysPath); err == nil {
			m := regexp.MustCompile(`(?m)^Path askpass (.*)$`).FindStringSubmatch(string(data))
			if len(m) >= 2 {
				fmt.Fprintf(os.Stderr, "[sido] system askpass: %s\n", m[1])
			} else {
				fmt.Fprintf(os.Stderr, "[sido] no system askpass in %s\n", sysPath)
			}
		} else if scope == "--system" {
			fmt.Fprintf(os.Stderr, "[sido] %s does not exist\n", sysPath)
		}
	}

	if scope == "" || scope == "--user" {
		found := false
		for _, p := range userProfilePaths() {
			ap := managedUserInstall(p)
			if ap == "" {
				continue
			}
			found = true
			fmt.Fprintf(os.Stderr, "[sido] user askpass in %s: %s\n", p, ap)
		}
		if !found {
			fmt.Fprintln(os.Stderr, "[sido] no managed user askpass in shell startup files")
		}
	}

	envActive := os.Getenv("SUDO_ASKPASS")
	if envActive != "" {
		fmt.Fprintf(os.Stderr, "[sido] active (env): %s\n", envActive)
	}
}

// ── upgrade ──────────────────────────────────────────────────────────────────

func requireNpmInstall() {
	rootRes := spawn("npm", []string{"root", "--global"}, spawnCfg{
		stdin: "inherit", stdout: "pipe", stderr: "inherit",
	})
	if rootRes.err != nil || rootRes.status != 0 {
		fmt.Fprintln(os.Stderr, "[sido] upgrade requires npm and an npm global installation")
		os.Exit(1)
	}

	pkgPath, err := filepath.EvalSymlinks(filepath.Join(strings.TrimSpace(string(rootRes.stdout)), "sido-askpass"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "[sido] upgrade supports npm global installations only; use the original package manager to upgrade")
		os.Exit(1)
	}
	exePath := selfPath()
	if exePath != pkgPath && !strings.HasPrefix(exePath, pkgPath+string(os.PathSeparator)) {
		fmt.Fprintln(os.Stderr, "[sido] upgrade supports npm global installations only; use the original package manager to upgrade")
		os.Exit(1)
	}
}

func latestNpmVersion() string {
	res := spawn("npm", []string{"view", "sido-askpass@latest", "version", "--json"}, spawnCfg{
		stdin: "inherit", stdout: "pipe", stderr: "inherit",
	})
	if res.err != nil {
		fmt.Fprintf(os.Stderr, "[sido] could not check npm for upgrades: %s\n", res.err)
		os.Exit(1)
	}
	if res.status != 0 {
		fmt.Fprintf(os.Stderr, "[sido] could not check npm for upgrades (status %d)\n", res.status)
		os.Exit(res.status)
	}

	var latest string
	out := strings.TrimSpace(string(res.stdout))
	if err := json.Unmarshal([]byte(out), &latest); err == nil && latest != "" {
		return latest
	}
	var arr []string
	if err := json.Unmarshal([]byte(out), &arr); err == nil && len(arr) == 1 {
		return arr[0]
	}
	fmt.Fprintln(os.Stderr, "[sido] npm returned an invalid latest version")
	os.Exit(1)
	return ""
}

func runNpmUpgrade(latest string) {
	fmt.Fprintf(os.Stderr, "[sido] upgrading %s to %s with npm\n", Version, latest)
	res := spawn("npm", []string{"install", "--global", "sido-askpass@latest"}, spawnCfg{
		stdin: "inherit", stdout: "inherit", stderr: "inherit",
	})
	if res.err != nil {
		fmt.Fprintf(os.Stderr, "[sido] npm upgrade failed: %s\n", res.err)
		os.Exit(1)
	}
	if res.status != 0 {
		fmt.Fprintf(os.Stderr, "[sido] npm upgrade failed with status %d\n", res.status)
		os.Exit(res.status)
	}
}

func refreshManagedConfiguration() {
	fmt.Fprintln(os.Stderr, "[sido] refreshing managed configuration")
	cp := cliPath()
	res := spawn(cp, []string{"install"}, spawnCfg{
		stdin: "inherit", stdout: "inherit", stderr: "inherit",
	})
	if res.err != nil {
		fmt.Fprintf(os.Stderr, "[sido] upgraded, but configuration refresh failed: %s\n", res.err)
		os.Exit(1)
	}
	os.Exit(res.status)
}

func doUpgrade() {
	requireNpmInstall()
	latest := latestNpmVersion()
	cmp, ok := compareSemver(Version, latest)
	if !ok {
		fmt.Fprintf(os.Stderr, "[sido] cannot compare versions %s and %s\n", Version, latest)
		os.Exit(1)
	}
	if cmp >= 0 {
		fmt.Fprintf(os.Stderr, "[sido] already up to date (%s; npm latest is %s)\n", Version, latest)
		os.Exit(0)
	}
	runNpmUpgrade(latest)
	refreshManagedConfiguration()
}

// ── semver comparison ────────────────────────────────────────────────────────

var semverRe = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$`)

func compareSemver(left, right string) (int, bool) {
	parse := func(value string) (core [3]int, prerelease []string, ok bool) {
		m := semverRe.FindStringSubmatch(value)
		if m == nil {
			return [3]int{}, nil, false
		}
		core[0], _ = strconv.Atoi(m[1])
		core[1], _ = strconv.Atoi(m[2])
		core[2], _ = strconv.Atoi(m[3])
		if m[4] != "" {
			prerelease = strings.Split(m[4], ".")
		}
		return core, prerelease, true
	}

	aCore, aPre, aOk := parse(left)
	bCore, bPre, bOk := parse(right)
	if !aOk || !bOk {
		return 0, false
	}

	for i := 0; i < 3; i++ {
		if aCore[i] != bCore[i] {
			if aCore[i] < bCore[i] {
				return -1, true
			}
			return 1, true
		}
	}

	if aPre == nil && bPre == nil {
		return 0, true
	}
	if aPre == nil {
		return 1, true
	}
	if bPre == nil {
		return -1, true
	}

	maxLen := len(aPre)
	if len(bPre) > maxLen {
		maxLen = len(bPre)
	}

	for i := 0; i < maxLen; i++ {
		var x, y string
		if i < len(aPre) {
			x = aPre[i]
		}
		if i < len(bPre) {
			y = bPre[i]
		}
		if x == "" {
			return -1, true
		}
		if y == "" {
			return 1, true
		}
		if x == y {
			continue
		}
		xNum := isNumeric(x)
		yNum := isNumeric(y)
		if xNum && yNum {
			xn, _ := strconv.Atoi(x)
			yn, _ := strconv.Atoi(y)
			if xn < yn {
				return -1, true
			}
			return 1, true
		}
		if xNum != yNum {
			if xNum {
				return -1, true
			}
			return 1, true
		}
		if x < y {
			return -1, true
		}
		return 1, true
	}
	return 0, true
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// ── parseAdapter ─────────────────────────────────────────────────────────────

func parseAdapter(value string) string {
	if isAdapter(value) {
		return value
	}
	fmt.Fprintf(os.Stderr, "[sido] unknown adapter \"%s\"; expected one of: %s\n", value, strings.Join(adapters, ", "))
	os.Exit(1)
	return ""
}

func parseRunArgs(args []string) (command string, cmdArgs []string, adapter string) {
	sepIdx := -1
	for i, a := range args {
		if a == "--" {
			sepIdx = i
			break
		}
	}
	if sepIdx == -1 || sepIdx+1 >= len(args) {
		fmt.Fprintf(os.Stderr, "[sido] usage: %s run [--adapter <name>] -- <command> [args...]\n", cliPath())
		os.Exit(1)
	}

	for i := 0; i < sepIdx; i++ {
		a := args[i]
		switch {
		case a == "--adapter":
			if i+1 >= sepIdx {
				fmt.Fprintln(os.Stderr, "[sido] option --adapter requires an argument")
				os.Exit(1)
			}
			i++
			adapter = parseAdapter(args[i])
		case strings.HasPrefix(a, "--adapter="):
			adapter = parseAdapter(strings.TrimPrefix(a, "--adapter="))
		case strings.HasPrefix(a, "-") && a != "-":
			fmt.Fprintf(os.Stderr, "[sido] unknown or unexpected option: %s\n", a)
			os.Exit(1)
		default:
			fmt.Fprintf(os.Stderr, "[sido] unexpected argument before --: %s\n", a)
			os.Exit(1)
		}
	}

	command = args[sepIdx+1]
	cmdArgs = args[sepIdx+2:]
	return
}

// ── doRun ────────────────────────────────────────────────────────────────────

func doRun(command string, args []string, adapter string) {
	envMap := make(map[string]string)
	for _, e := range os.Environ() {
		kv := strings.SplitN(e, "=", 2)
		if len(kv) == 2 {
			envMap[kv[0]] = kv[1]
		}
	}
	envMap["SUDO_ASKPASS"] = askpassPath()
	if adapter != "" {
		envMap["SIDO_ADAPTER"] = adapter
	}

	var envSlice []string
	for k, v := range envMap {
		envSlice = append(envSlice, k+"="+v)
	}

	res := spawn(command, args, spawnCfg{
		stdin: "inherit", stdout: "inherit", stderr: "inherit", env: envSlice,
	})
	if res.err != nil {
		fmt.Fprintf(os.Stderr, "[sido] failed to run %s: %s\n", command, res.err)
		os.Exit(1)
	}
	if res.signum != 0 {
		p, _ := os.FindProcess(os.Getpid())
		if p != nil {
			p.Signal(res.signum)
		}
		os.Exit(128 + int(res.signum))
	}
	status := res.status
	if status < 0 {
		status = 1
	}
	os.Exit(status)
}

// ── adapter dispatch ─────────────────────────────────────────────────────────

func adapterUnavailable(adapter, detail string) {
	fmt.Fprintf(os.Stderr, "[sido] %s adapter unavailable: %s\n", adapter, detail)
	os.Exit(1)
}

func adapterPrompt(adapter string) {
	switch adapter {
	case "tmux":
		tmuxPrompt()
	case "herdr":
		herdrPrompt(false)
	case "osascript":
		if !isMac {
			adapterUnavailable(adapter, "requires macOS")
		}
		macGui()
	case "zenity":
		exactGuiPrompt("zenity", []string{"--password", "--title", displayPrompt})
	case "kdialog":
		exactGuiPrompt("kdialog", []string{"--password", displayPrompt})
	case "tty":
		if !ttyPrompt() {
			adapterUnavailable(adapter, "could not read /dev/tty")
		}
	case "watch":
		watchPrompt()
	}
}

func dispatchAdapter() {
	if selectedAdapter != "auto" {
		adapterPrompt(selectedAdapter)
		return
	}
	if isTmux {
		tmuxPrompt()
		return
	}
	if isHerdr {
		herdrPrompt(true)
		return
	}
	if canGui {
		guiPrompt()
		return
	}
	if interactiveShellTty() && ttyPrompt() {
		return
	}
	watchPrompt()
}

// ── globals for askpass mode ─────────────────────────────────────────────────

var (
	prompt            string
	requestingCommand string
	displayPrompt     string
	isTmux            bool
	isHerdr           bool
	isMac             bool
	isLinux           bool
	hasDisplay        bool
	canGui            bool
	selectedAdapter   string = "auto"
)

// ── help ─────────────────────────────────────────────────────────────────────

const helpText = `sido-askpass — SUDO_ASKPASS shim for headless agent environments

Name:    sido-askpass
Version: %s
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
  sido askpass [prompt]              (internal) askpass mode, invoked via sudo
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
  SIDO_WATCH_TIMEOUT=<sec> approver wait timeout (default 120)`

// ── ManagerMain ──────────────────────────────────────────────────────────────

func ManagerMain() {
	command := ""
	if len(os.Args) > 1 {
		command = os.Args[1]
	}

	if command == "--help" || command == "-h" || command == "help" {
		fmt.Printf(helpText+"\n", Version)
		os.Exit(0)
	}

	if command == "--version" || command == "-V" || command == "version" {
		fmt.Println(Version)
		os.Exit(0)
	}

	if command == "install" || command == "uninstall" {
		user := false
		system := false
		for i := 2; i < len(os.Args); i++ {
			if os.Args[i] == "--user" {
				user = true
			}
			if os.Args[i] == "--system" {
				system = true
			}
		}
		if (user && system) || (command == "uninstall" && !user && !system) {
			scopeUsage := "[--user|--system]"
			if command == "install" {
				scopeUsage = "[--user|--system]"
			} else {
				scopeUsage = "--user|--system"
			}
			fmt.Fprintf(os.Stderr, "[sido] usage: %s %s %s\n", cliPath(), command, scopeUsage)
			os.Exit(1)
		}
		scope := ""
		if user {
			scope = "--user"
		} else if system {
			scope = "--system"
		}
		if command == "install" {
			doInstall(scope)
		} else {
			doUninstall(scope)
		}
		os.Exit(0)
	}

	if command == "status" {
		user := false
		system := false
		for i := 2; i < len(os.Args); i++ {
			if os.Args[i] == "--user" {
				user = true
			}
			if os.Args[i] == "--system" {
				system = true
			}
		}
		if user && system {
			fmt.Fprintf(os.Stderr, "[sido] usage: %s status [--user|--system]\n", cliPath())
			os.Exit(1)
		}
		scope := ""
		if user {
			scope = "--user"
		} else if system {
			scope = "--system"
		}
		doStatus(scope)
		os.Exit(0)
	}

	if command == "upgrade" {
		doUpgrade()
	}

	if command == "run" {
		cmd, args, adapter := parseRunArgs(os.Args[2:])
		doRun(cmd, args, adapter)
	}

	if command == "watch" {
		doWatch()
		os.Exit(0)
	}

	if command == "approve" {
		doApprove()
		os.Exit(0)
	}

	fmt.Fprintf(os.Stderr, "[sido] unknown command: %s\n", command)
	fmt.Fprintf(os.Stderr, "[sido] run \"%s --help\" for usage\n", cliPath())
	os.Exit(1)
}

// ── ReceiverMain ─────────────────────────────────────────────────────────────

// ReceiverMain is the top-level `sido _inner_prompt_receiver` entry: it runs
// inside a popup/pane, collects the password, writes it to the FIFO, and cleans
// up (including closing the pane and self-reaping when the askpass shim dies).
func ReceiverMain(args []string) {
	if len(args) < 4 {
		os.Exit(1)
	}
	paneID := ""
	if len(args) >= 5 {
		paneID = args[4]
	}
	innerPromptReceiver(args[0], args[1], args[2], args[3], paneID)
}

// ── AskpassMain ──────────────────────────────────────────────────────────────

func AskpassMain(args []string) {
	command := ""
	if len(args) > 0 {
		command = args[0]
	}

	u, err := user.Current()
	username := "root"
	if err == nil {
		username = u.Username
	}
	hostname, _ := os.Hostname()

	prompt = command
	if prompt == "" {
		prompt = fmt.Sprintf("[sudo] password for %s: ", username)
	}
	prompt = strings.ReplaceAll(prompt, "%u", username)
	prompt = strings.ReplaceAll(prompt, "%h", hostname)

	requestingCommand = getRequestingCommand()
	displayPrompt = prompt
	if requestingCommand != "" {
		displayPrompt = fmt.Sprintf("Command: %s\n%s", requestingCommand, prompt)
	}

	isTmux = os.Getenv("TMUX") != ""
	isHerdr = os.Getenv("HERDR_ENV") == "1"
	isMac = runtime.GOOS == "darwin"
	isLinux = runtime.GOOS == "linux"
	hasDisplay = os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
	canGui = isMac || (isLinux && hasDisplay)

	selectedAdapter = os.Getenv("SIDO_ADAPTER")
	if selectedAdapter == "" {
		selectedAdapter = "auto"
	}
	selectedAdapter = parseAdapter(selectedAdapter)

	dispatchAdapter()
}
