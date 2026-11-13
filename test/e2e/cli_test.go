package e2e

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fastvm/internal/api"
	"fastvm/internal/image"
)

// TestCLIOverUnixSocket drives the real daemon through its HTTP API with the
// compiled fastvm CLI: build-less image ingest, run, ls, exec, pause, resume, rm.
// The VM is real; only the network plane is faked in this test.
func TestCLIOverUnixSocket(t *testing.T) {
	kernel, rootfs := requireE2E(t)
	s := newStack(t, kernel)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	base, err := s.images.Ingest(ctx, image.IngestSpec{RootfsDir: rootfs, Tag: "alpine:cli"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	socket := filepath.Join(t.TempDir(), "fastvm.sock")
	srv := api.New(s.d, s.d, slog.New(slog.NewTextHandler(testWriter{t}, nil)))
	go func() { _ = srv.Serve(ctx, socket) }()
	waitFor(t, 5*time.Second, func() bool { _, err := os.Stat(socket); return err == nil })

	cli := buildCLI(t)
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, cli, args...)
		cmd.Env = append(os.Environ(), "FASTVM_SOCKET="+socket)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fastvm %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	if out := run("version"); !strings.Contains(out, "0.1.0") {
		t.Fatalf("version output = %q", out)
	}

	out := run("run", "--image", base.Identity(), "--name", "cli-e2e", "--no-auto-sleep", "--", "/bin/echo", "boot")
	fields := strings.Fields(out)
	if len(fields) < 2 {
		t.Fatalf("run output = %q", out)
	}
	id := fields[0]

	if out := run("ls"); !strings.Contains(out, "cli-e2e") ||
		(!strings.Contains(out, "Running") && !strings.Contains(out, "Idle")) {
		t.Fatalf("ls output = %q", out)
	}
	if out := run("get", "cli-e2e"); !strings.Contains(out, id) {
		t.Fatalf("get by name = %q", out)
	}
	prefix := id[:8]
	if out := run("get", prefix); !strings.Contains(out, id) {
		t.Fatalf("get by ID prefix = %q", out)
	}
	if out := run("exec", prefix, "--", "/bin/echo", "cli-hello"); !strings.Contains(out, "cli-hello") {
		t.Fatalf("exec by ID prefix output = %q", out)
	}
	if out := run("shell", id, "--", "/bin/sh", "-c", "echo shell-e2e"); !strings.Contains(out, "shell-e2e") {
		t.Fatalf("shell output = %q", out)
	}
	if out := run("exec", "-it", id, "--", "/bin/sh", "-c", "echo exec-it-e2e"); !strings.Contains(out, "exec-it-e2e") {
		t.Fatalf("exec -it output = %q", out)
	}

	run("pause", id, "--deep")
	got := run("get", id)
	if !strings.Contains(got, "Dormant") {
		t.Fatalf("get after deep pause = %q", got)
	}
	run("resume", id)
	got = run("get", id)
	if !strings.Contains(got, "Running") && !strings.Contains(got, "Idle") {
		t.Fatalf("get after resume = %q", got)
	}
	if out := run("events", "-n", "20"); !strings.Contains(out, "wake") && !strings.Contains(out, "state") {
		t.Fatalf("events output = %q", out)
	}

	run("rm", id)
	if out := run("ls"); strings.Contains(out, "cli-e2e") {
		t.Fatalf("sandbox still listed: %q", out)
	}

	if out := run("image", "ls"); !strings.Contains(out, "alpine:cli") {
		t.Fatalf("image ls output = %q", out)
	}
	run("image", "rm", base.Identity())
	if out := run("image", "ls"); strings.Contains(out, "alpine:cli") {
		t.Fatalf("image still listed after rm: %q", out)
	}
}

func buildCLI(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "fastvm")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/fastvm")
	cmd.Dir = repoRoot(t)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, b)
	}
	return out
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}
