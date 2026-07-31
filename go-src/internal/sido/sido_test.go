package sido

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCompareSemver(t *testing.T) {
	tests := []struct {
		name  string
		left  string
		right string
		want  int
		ok    bool
	}{
		{"equal basic", "1.0.0", "1.0.0", 0, true},
		{"equal with v", "v1.0.0", "1.0.0", 0, true},
		{"greater major", "2.0.0", "1.0.0", 1, true},
		{"less minor", "1.0.0", "1.1.0", -1, true},
		{"greater patch", "1.0.1", "1.0.0", 1, true},
		{"prerelease < release", "1.0.0-alpha", "1.0.0", -1, true},
		{"release > prerelease", "1.0.0", "1.0.0-beta", 1, true},
		{"prerelease numeric compare", "1.0.0-2", "1.0.0-10", -1, true},
		{"prerelease numeric lower precedence", "1.0.0-alpha", "1.0.0-1", 1, true},
		{"prerelease numeric before alpha", "1.0.0-1", "1.0.0-alpha", -1, true},
		{"prerelease dots longer smaller", "1.0.0-alpha", "1.0.0-alpha.1", -1, true},
		{"prerelease dots longer larger", "1.0.0-alpha.1", "1.0.0-alpha.beta", -1, true},
		{"prerelease beta > alpha", "1.0.0-beta", "1.0.0-alpha", 1, true},
		{"prerelease beta.2 > beta.1", "1.0.0-beta.2", "1.0.0-beta.1", 1, true},
		{"prerelease with build", "1.0.0-alpha+build", "1.0.0-alpha", 0, true},
		{"build ignored", "1.0.0+001", "1.0.0+002", 0, true},
		{"invalid left", "not-a-version", "1.0.0", 0, false},
		{"invalid right", "1.0.0", "garbage", 0, false},
		{"both invalid", "x", "y", 0, false},
		{"prerelease same lexicographic", "1.0.0-alpha", "1.0.0-alpha", 0, true},
		{"prerelease lexicographic compare", "1.0.0-a", "1.0.0-b", -1, true},
		{"prerelease mixed numeric lexicographic", "1.0.0-1a", "1.0.0-1", 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := compareSemver(tt.left, tt.right)
			if ok != tt.ok {
				t.Errorf("compareSemver(%q, %q) ok=%v, want ok=%v", tt.left, tt.right, ok, tt.ok)
			}
			if ok && got != tt.want {
				t.Errorf("compareSemver(%q, %q)=%d, want %d", tt.left, tt.right, got, tt.want)
			}
		})
	}
}

func TestCompareSemverEqual(t *testing.T) {
	cases := [][]string{
		{"1.0.0", "1.0.0"},
		{"v2.3.4", "2.3.4"},
		{"0.0.0", "0.0.0"},
		{"1.2.3-alpha+build", "1.2.3-alpha"},
		{"1.2.3-alpha.0", "1.2.3-alpha.0"},
	}
	for _, c := range cases {
		got, ok := compareSemver(c[0], c[1])
		if !ok {
			t.Errorf("compareSemver(%q, %q) unexpected failure", c[0], c[1])
		}
		if got != 0 {
			t.Errorf("compareSemver(%q, %q)=%d, want 0", c[0], c[1], got)
		}
	}
}

func TestParseAdapter(t *testing.T) {
	// isAdapter is a pure check (does not exit)
	for _, valid := range adapters {
		if !isAdapter(valid) {
			t.Errorf("expected %q to be valid adapter", valid)
		}
	}
	if isAdapter("nope") {
		t.Error("expected 'nope' to be invalid adapter")
	}
	if isAdapter("") {
		t.Error("expected empty string to be invalid adapter")
	}
}

func TestManagedBlockPattern(t *testing.T) {
	content := "export FOO=bar\n\n# sido start\nexport SUDO_ASKPASS=\"/old/path\"\n# sido end\n\nexport AFTER=yes\n"

	block := managedBlockPattern.FindString(content)
	if block == "" {
		t.Fatal("expected managed block match")
	}

	extractRe := regexp.MustCompile(`(?m)^export SUDO_ASKPASS=(.*)$`)
	m := extractRe.FindStringSubmatch(block)
	if len(m) < 2 || m[1] != "\"/old/path\"" {
		t.Errorf("expected SUDO_ASKPASS extraction, got %v", m)
	}

	removed := managedBlockPattern.ReplaceAllString(content, "")
	if regexp.MustCompile(`# sido`).MatchString(removed) {
		t.Error("expected managed block removed")
	}

	zshContent := "eval \"$(fnm env)\"\nexport KEEP_ZSH=yes\n\n# sido start\nexport SUDO_ASKPASS=\"/old/path\"\nalias sudo='sudo -A'\n# sido end\n"
	if !managedBlockPattern.MatchString(zshContent) {
		t.Error("expected zsh-compatible block to match")
	}
}

func TestPromptTemplateSubstitution(t *testing.T) {
	prompt := "[sudo] password for %u: "
	u := "pm"
	h := "host"

	got := stringsReplaceAll(prompt, map[string]string{"%u": u, "%h": h})
	want := "[sudo] password for pm: "
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	prompt2 := "Password for %u@%h: "
	got2 := stringsReplaceAll(prompt2, map[string]string{"%u": u, "%h": h})
	want2 := "Password for pm@host: "
	if got2 != want2 {
		t.Errorf("got %q, want %q", got2, want2)
	}
}

// stringsReplaceAll is a helper that mirrors the Go strings.ReplaceAll chaining pattern
func stringsReplaceAll(s string, replacements map[string]string) string {
	for old, new := range replacements {
		s = strings.ReplaceAll(s, old, new)
	}
	return s
}

func TestRemoveManagedUserInstall(t *testing.T) {
	content := "export BEFORE=yes\n\n# sido start\nexport SUDO_ASKPASS=\"/old/path\"\n# sido end\n\nexport AFTER=yes\n"
	got := removeManagedUserInstall(content)
	want := "export BEFORE=yes\n\nexport AFTER=yes"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestNormalizeUninstalledContent(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "empty",
			input: "",
			want:  "\n",
		},
		{
			name:  "single line",
			input: "export FOO=bar",
			want:  "export FOO=bar\n",
		},
		{
			name:  "collapse newlines",
			input: "line1\n\n\n\nline2",
			want:  "line1\n\nline2\n",
		},
		{
			name:  "trailing spaces trimmed",
			input: "line1   \n",
			want:  "line1\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeUninstalledContent(tt.input)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFifoRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "password")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = writeToFifo(fifo, "s3cr3t", time.Second) }()
	got, cancelled, err := readFifoPassword(fifo, ctx)
	if err != nil {
		t.Fatalf("readFifoPassword: %v", err)
	}
	if cancelled {
		t.Fatalf("unexpected cancel")
	}
	if got != "s3cr3t" {
		t.Errorf("got %q, want %q", got, "s3cr3t")
	}
}

// An empty password (NOPASSWD) must round-trip distinctly from a timeout/error.
func TestFifoEmptyPasswordRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "password")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = writeToFifo(fifo, "", time.Second) }()
	got, cancelled, err := readFifoPassword(fifo, ctx)
	if err != nil {
		t.Fatalf("readFifoPassword should return nil err for an empty password, got: %v", err)
	}
	if cancelled {
		t.Fatalf("unexpected cancel for empty password")
	}
	if got != "" {
		t.Errorf("empty password: got %q, want \"\"", got)
	}
}

func TestFifoReadTimeout(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "password")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	got, _, err := readFifoPassword(fifo, ctx)
	if err == nil {
		t.Fatalf("expected timeout error, got nil (line=%q)", got)
	}
}

// A writer that connects then closes without writing a password must be
// detected as a cancel — this is the pane/popup-closed case.
func TestFifoDetectsCancelOnWriterClose(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "password")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		// open the write end, then close it without writing anything
		f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			t.Errorf("writer open: %v", err)
			return
		}
		time.Sleep(100 * time.Millisecond) // hold it open briefly so the reader sees "connected"
		f.Close()
	}()
	got, cancelled, err := readFifoPassword(fifo, ctx)
	if err != nil {
		t.Fatalf("readFifoPassword: %v", err)
	}
	if !cancelled {
		t.Fatalf("expected cancelled=true (writer closed without a password), got pw=%q", got)
	}
}

// Regression: herdr `pane run` is fire-and-forget — runUI returns immediately
// while the password arrives later. promptViaSurface must WAIT on the FIFO,
// not bail when runUI returns. (Previously a 1s timeout closed the pane while
// the user was still typing.)
func TestPromptViaSurfaceWaitsForDelayedPassword(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "password")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	pw, err := promptViaSurface(fifo, func() error {
		go func() {
			time.Sleep(300 * time.Millisecond)
			_ = writeToFifo(fifo, "late", time.Second)
		}()
		return nil // fire-and-forget: returns before the password is written
	})
	if err != nil {
		t.Fatalf("promptViaSurface: %v", err)
	}
	if pw != "late" {
		t.Errorf("got %q, want %q (should have waited for the delayed write)", pw, "late")
	}
}

func TestPromptViaSurfaceBailsOnUIError(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "password")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	if _, err := promptViaSurface(fifo, func() error { return errors.New("surface failed") }); err == nil {
		t.Fatal("expected promptViaSurface to return the error from a failed UI command")
	}
}

// readHiddenLine must distinguish a genuine EOF (fall through to another
// adapter) from an empty password (Enter with nothing typed). Verified with a
// pipe since the logic is identical to a canonical tty at the byte level.
func TestReadHiddenLinePassword(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() { w.Write([]byte("secret\n")); w.Close() }()
	line, err := readHiddenLine(r)
	r.Close()
	if err != nil {
		t.Fatalf("readHiddenLine: %v", err)
	}
	if line != "secret" {
		t.Errorf("got %q, want %q", line, "secret")
	}
}

func TestReadHiddenLineEmptyPassword(t *testing.T) {
	r, w, _ := os.Pipe()
	go func() { w.Write([]byte("\n")); w.Close() }()
	line, err := readHiddenLine(r)
	r.Close()
	if err != nil {
		t.Fatalf("empty password should be nil err, got %v", err)
	}
	if line != "" {
		t.Errorf("got %q, want empty", line)
	}
}

func TestReadHiddenLineEOF(t *testing.T) {
	r, w, _ := os.Pipe()
	w.Close() // EOF with no input
	line, err := readHiddenLine(r)
	r.Close()
	if err != io.EOF {
		t.Fatalf("EOF should return io.EOF, got %v (line=%q)", err, line)
	}
}

// Raw/cbreak ttys deliver '\r' (ICRNL off) on Enter — must terminate the line.
func TestReadHiddenLineCarriageReturn(t *testing.T) {
	r, w, _ := os.Pipe()
	go func() { w.Write([]byte("pw\r")); w.Close() }()
	line, err := readHiddenLine(r)
	r.Close()
	if err != nil {
		t.Fatalf("readHiddenLine: %v", err)
	}
	if line != "pw" {
		t.Errorf("got %q, want %q", line, "pw")
	}
}
