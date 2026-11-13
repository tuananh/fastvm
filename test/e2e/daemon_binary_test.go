package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestDaemonBinarySmoke boots the real fastvmd binary under a user+net
// namespace, waits for its unix-socket API, drives it with the real CLI, and
// shuts it down cleanly with SIGTERM.
func TestDaemonBinarySmoke(t *testing.T) {
	requireE2E(t)
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip("unshare not available")
	}
	root := repoRoot(t)
	tmp := t.TempDir()
	fastvmd := buildGoBinary(t, "./cmd/fastvmd", "fastvmd")
	fastvm := buildGoBinary(t, "./cmd/fastvm", "fastvm")
	agentBin := buildGoBinary(t, "./cmd/fastvm-agent", "fastvm-agent")
	kernel, _ := requireE2E(t)
	socket := filepath.Join(tmp, "fastvm.sock")

	cmd := exec.Command("unshare", "-Urn", "-r", fastvmd,
		"-socket", socket,
		"-data-dir", filepath.Join(tmp, "data"),
		"-kernel", kernel,
		"-agent", agentBin,
		"-log-level", "debug",
	)
	cmd.Dir = root
	cmd.Stdout = testWriter{t}
	cmd.Stderr = testWriter{t}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGKILL)
		}
	})

	waitFor(t, 10*time.Second, func() bool { _, err := os.Stat(socket); return err == nil })

	runCLI := func(args ...string) string {
		t.Helper()
		c := exec.Command(fastvm, args...)
		c.Env = append(os.Environ(), "FASTVM_SOCKET="+socket)
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("fastvm %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	if out := runCLI("version"); !strings.Contains(out, "fastvm 0.1.0") {
		t.Fatalf("version = %q", out)
	}
	if out := runCLI("image", "ls"); !strings.Contains(out, "TAG") {
		t.Fatalf("images = %q", out)
	}
	if out := runCLI("ls"); !strings.Contains(out, "ID") {
		t.Fatalf("ls = %q", out)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("fastvmd exited with error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("fastvmd did not exit after SIGTERM")
	}
}
