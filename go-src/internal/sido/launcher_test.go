package sido

// Tests for the platform-detecting shell launchers.
//
// These need no Go binary: stub programs named sido-<os>-<arch> report which one
// the launcher exec'd, and a fake `uname` on PATH impersonates each supported
// platform. That exercises all four OS/arch mappings from any single host, plus
// the unsupported-platform and missing-binary error paths.
//
// The launchers running the real host binary are covered implicitly by every
// test in e2e_test.go, which invokes them through h.cliPath()/h.askpassPath().

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// launcherBinaries is the full set the build script produces.
var launcherBinaries = []string{
	"sido-linux-amd64",
	"sido-linux-arm64",
	"sido-darwin-amd64",
	"sido-darwin-arm64",
}

type launcherHarness struct {
	t        *testing.T
	launcher string // dir with sido, sido-askpass, and stub binaries
	fakeBin  string // dir with the fake uname
}

func newLauncherHarness(t *testing.T, binaries ...string) *launcherHarness {
	t.Helper()
	dir := t.TempDir()
	lh := &launcherHarness{
		t:        t,
		launcher: filepath.Join(dir, "dist"),
		fakeBin:  filepath.Join(dir, "fakebin"),
	}
	for _, d := range []string{lh.launcher, lh.fakeBin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	for _, name := range []string{"sido", "sido-askpass"} {
		if err := e2eCopyFile(e2eRepoPath("scripts", name+".sh"), filepath.Join(lh.launcher, name)); err != nil {
			t.Fatal(err)
		}
	}

	// Each stub prints its own name followed by its arguments.
	for _, name := range binaries {
		stub := "#!/bin/sh\nprintf '%s' '" + name + "'\n" +
			"for a in \"$@\"; do printf ' %s' \"$a\"; done\nprintf '\\n'\n"
		if err := e2eWriteFile(filepath.Join(lh.launcher, name), 0o755, stub); err != nil {
			t.Fatal(err)
		}
	}

	uname := "#!/bin/sh\ncase \"$1\" in\n" +
		"  -s) printf '%s\\n' \"$FAKE_UNAME_S\" ;;\n" +
		"  -m) printf '%s\\n' \"$FAKE_UNAME_M\" ;;\n" +
		"  *)  printf '%s\\n' \"$FAKE_UNAME_S\" ;;\n" +
		"esac\n"
	if err := e2eWriteFile(filepath.Join(lh.fakeBin, "uname"), 0o755, uname); err != nil {
		t.Fatal(err)
	}
	return lh
}

func (lh *launcherHarness) run(unameS, unameM, launcher string, args ...string) e2eResult {
	lh.t.Helper()
	cmd := exec.Command(filepath.Join(lh.launcher, launcher), args...)
	cmd.Env = []string{
		"PATH=" + lh.fakeBin + string(os.PathListSeparator) + lh.launcher +
			string(os.PathListSeparator) + os.Getenv("PATH"),
		"FAKE_UNAME_S=" + unameS,
		"FAKE_UNAME_M=" + unameM,
		"HOME=" + filepath.Join(lh.launcher, "home"),
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := e2eResult{stdout: stdout.String(), stderr: stderr.String()}
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			lh.t.Fatalf("running %s: %v", launcher, err)
		}
		res.status = exitErr.ExitCode()
	}
	return res
}

func TestLauncherSelectsPlatformBinary(t *testing.T) {
	lh := newLauncherHarness(t, launcherBinaries...)

	tests := []struct {
		unameS, unameM string
		want           string
	}{
		{"Linux", "x86_64", "sido-linux-amd64"},
		{"Linux", "amd64", "sido-linux-amd64"},
		{"Linux", "aarch64", "sido-linux-arm64"},
		{"Linux", "arm64", "sido-linux-arm64"},
		{"Darwin", "x86_64", "sido-darwin-amd64"},
		{"Darwin", "amd64", "sido-darwin-amd64"},
		{"Darwin", "arm64", "sido-darwin-arm64"},
		{"Darwin", "aarch64", "sido-darwin-arm64"},
	}

	for _, tt := range tests {
		t.Run(tt.unameS+"_"+tt.unameM, func(t *testing.T) {
			res := lh.run(tt.unameS, tt.unameM, "sido", "--version")
			if res.status != 0 {
				t.Fatalf("status = %d\nstderr: %s", res.status, res.stderr)
			}
			if got, want := strings.TrimSpace(res.stdout), tt.want+" --version"; got != want {
				t.Errorf("launcher executed %q, want %q", got, want)
			}
		})
	}
}

// TestLauncherAskpassForwardsToSido checks that the askpass launcher reuses the
// sibling `sido` launcher, so platform detection lives in one place.
func TestLauncherAskpassForwardsToSido(t *testing.T) {
	lh := newLauncherHarness(t, launcherBinaries...)

	res := lh.run("Darwin", "arm64", "sido-askpass", "prompt", "extra")
	if res.status != 0 {
		t.Fatalf("status = %d\nstderr: %s", res.status, res.stderr)
	}
	if got, want := strings.TrimSpace(res.stdout), "sido-darwin-arm64 askpass prompt extra"; got != want {
		t.Errorf("launcher executed %q, want %q", got, want)
	}
}

func TestLauncherRejectsUnsupportedPlatforms(t *testing.T) {
	lh := newLauncherHarness(t, launcherBinaries...)

	t.Run("architecture", func(t *testing.T) {
		res := lh.run("Linux", "riscv64", "sido")
		if res.status != 1 {
			t.Errorf("status = %d, want 1", res.status)
		}
		mustMatch(t, `unsupported architecture`, res.stderr, "stderr")
	})

	t.Run("os", func(t *testing.T) {
		res := lh.run("FreeBSD", "x86_64", "sido")
		if res.status != 1 {
			t.Errorf("status = %d, want 1", res.status)
		}
		mustMatch(t, `unsupported OS`, res.stderr, "stderr")
	})
}

// TestLauncherReportsMissingBinary covers the case where the platform is
// supported but the build did not ship that variant: the launcher must say so
// rather than failing with a bare exec error.
func TestLauncherReportsMissingBinary(t *testing.T) {
	lh := newLauncherHarness(t, "sido-linux-amd64")

	res := lh.run("Darwin", "arm64", "sido")
	if res.status != 1 {
		t.Errorf("status = %d, want 1", res.status)
	}
	mustMatch(t, `missing binary for darwin/arm64`, res.stderr, "stderr")
}
