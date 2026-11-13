// Package e2e contains gated end-to-end tests against real Firecracker VMs.
//
// Run with:
//
//	make assets
//	FASTVM_E2E=1 go test -count=1 -timeout 10m ./test/e2e/...
//
// The tests skip cleanly when /dev/kvm, firecracker, or the downloaded assets
// are missing.
package e2e

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fastvm/internal/agent"
	"fastvm/internal/config"
	"fastvm/internal/daemon"
	"fastvm/internal/image"
	"fastvm/internal/manager"
	"fastvm/internal/network"
	"fastvm/internal/store"
	"fastvm/internal/types"
	"fastvm/internal/vmm/firecracker"
	"fastvm/internal/volume"
)

// fakeNetwork is a network.Manager with no TAP. It lets the e2e test exercise
// the full VMM/image/volume/daemon stack without CAP_NET_ADMIN.
type fakeNetwork struct {
	wake chan network.WakeEvent
}

func newFakeNetwork() *fakeNetwork {
	return &fakeNetwork{wake: make(chan network.WakeEvent, 4)}
}

func (f *fakeNetwork) Create(_ context.Context, req network.CreateRequest) (network.Port, error) {
	return network.Port{SandboxID: req.SandboxID, MAC: "02:00:00:00:00:01", IP: "10.77.0.2", Gateway: "10.77.0.1"}, nil
}
func (f *fakeNetwork) Delete(context.Context, string) error          { return nil }
func (f *fakeNetwork) SetAsleep(context.Context, string, bool) error { return nil }
func (f *fakeNetwork) Stats(context.Context, string) (types.NetStats, error) {
	return types.NetStats{}, nil
}
func (f *fakeNetwork) WakeEvents() <-chan network.WakeEvent { return f.wake }
func (f *fakeNetwork) Close() error                         { return nil }

func requireE2E(t *testing.T) (kernel, rootfs string) {
	t.Helper()
	if os.Getenv("FASTVM_E2E") == "" {
		t.Skip("set FASTVM_E2E=1 and run `make assets` to enable real-VM e2e tests")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("/dev/kvm not available")
	}
	root := repoRoot(t)
	kernel = envOr("FASTVM_KERNEL", filepath.Join(root, "test", "e2e", "assets", "vmlinux.bin"))
	rootfs = envOr("FASTVM_ROOTFS", filepath.Join(root, "test", "e2e", "assets", "rootfs"))
	if _, err := os.Stat(kernel); err != nil {
		t.Skipf("kernel missing (%s): run make assets", kernel)
	}
	if st, err := os.Stat(rootfs); err != nil || !st.IsDir() {
		t.Skipf("rootfs missing (%s): run make assets", rootfs)
	}
	if _, err := exec.LookPath("firecracker"); err != nil {
		t.Skip("firecracker binary not found")
	}
	return kernel, rootfs
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
			return wd
		}
		parent := filepath.Dir(wd)
		if parent == wd {
			t.Fatalf("cannot find repo root (go.mod) from %q", wd)
		}
		wd = parent
	}
}

func buildAgent(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "fastvm-agent")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/fastvm-agent")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build guest agent: %v\n%s", err, b)
	}
	return out
}

type stack struct {
	d      *daemon.Daemon
	images *image.FSBuilder
}

func newStack(t *testing.T, kernel string) *stack {
	t.Helper()
	root := repoRoot(t)
	tmp := t.TempDir()
	agentBin := buildAgent(t)

	cfg := config.Default()
	cfg.DataDir = filepath.Join(tmp, "data")
	cfg.KernelPath = kernel
	cfg.AgentPath = agentBin
	cfg.Wake.CoalesceWindow = config.Duration(10 * time.Millisecond)

	st, err := store.Open(filepath.Join(tmp, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	imgs, err := image.NewBuilder(image.Options{
		BaseDir:   filepath.Join(tmp, "images"),
		AgentPath: agentBin,
		WorkDir:   root,
	})
	if err != nil {
		t.Fatal(err)
	}
	vols, err := volume.NewManager(volume.Options{
		VolDir:     filepath.Join(tmp, "volumes"),
		DefaultCOW: "copy", // /home is ext4 (no reflink)
	})
	if err != nil {
		t.Fatal(err)
	}
	drv, err := firecracker.NewDriver(firecracker.Options{
		Binary:      envOr("FASTVM_FIRECRACKER", "firecracker"),
		RunDir:      filepath.Join(tmp, "run"),
		BootTimeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	dial := func(ctx context.Context, uds string, cid uint32) (agent.Client, error) {
		return agent.DialFirecracker(ctx, uds, cid, 30*time.Second)
	}
	d, err := daemon.New(daemon.Deps{
		Config: cfg, Store: st, Images: imgs, Volumes: vols,
		Network: newFakeNetwork(), VMM: drv, DialAgent: dial,
		Log: slog.New(slog.NewTextHandler(testWriter{t}, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return &stack{d: d, images: imgs}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func TestDaemonLifecycleRealVM(t *testing.T) {
	kernel, rootfs := requireE2E(t)
	s := newStack(t, kernel)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	base, err := s.images.Ingest(ctx, image.IngestSpec{RootfsDir: rootfs, Tag: "alpine:e2e"})
	if err != nil {
		t.Fatalf("ingest base disk: %v", err)
	}
	t.Logf("base disk: %s %d MiB", base.Path, base.SizeBytes>>20)

	sb, err := s.d.Create(ctx, manager.CreateOptions{
		Name: "e2e", Image: base.Identity(), NoAutoSleep: true,
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if sb.State != types.StateRunning {
		t.Fatalf("state = %s", sb.State)
	}
	defer func() { _ = s.d.Delete(context.Background(), sb.ID) }()

	// Exec in the booted VM.
	resp, err := s.d.Exec(ctx, sb.ID, []string{"/bin/echo", "hello-fastvm"}, nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(resp.Stdout, "hello-fastvm") {
		t.Fatalf("exec stdout = %q", resp.Stdout)
	}

	// Docker-style per-boot identity files are written by the guest agent.
	resp, err = s.d.Exec(ctx, sb.ID, []string{"/bin/sh", "-c",
		"cat /etc/hostname; echo ---; cat /etc/resolv.conf; echo ---; cat /etc/hosts; echo ---; hostname"}, nil)
	if err != nil {
		t.Fatalf("exec identity: %v", err)
	}
	hostname := sb.ID
	if len(hostname) > 12 {
		hostname = hostname[:12]
	}
	if !strings.Contains(resp.Stdout, hostname+"\n") {
		t.Errorf("hostname file missing %q:\n%s", hostname, resp.Stdout)
	}
	if !strings.Contains(resp.Stdout, "nameserver ") {
		t.Errorf("resolv.conf has no nameserver:\n%s", resp.Stdout)
	}
	if !strings.Contains(resp.Stdout, "10.77.0.2\t"+hostname) {
		t.Errorf("hosts file missing guest entry:\n%s", resp.Stdout)
	}
	if !strings.HasSuffix(strings.TrimSpace(resp.Stdout), hostname) {
		t.Errorf("kernel hostname not set to %q:\n%s", hostname, resp.Stdout)
	}

	// Pause, then exec auto-wakes.
	if err := s.d.Sleep(ctx, sb.ID, false); err != nil {
		t.Fatalf("pause: %v", err)
	}
	got, err := s.d.Get(ctx, sb.ID)
	if err != nil || got.State != types.StatePaused {
		t.Fatalf("after pause: %v state=%s", err, got.State)
	}
	resp, err = s.d.Exec(ctx, sb.ID, []string{"/bin/echo", "after-pause"}, nil)
	if err != nil {
		t.Fatalf("exec after pause: %v", err)
	}
	if !strings.Contains(resp.Stdout, "after-pause") {
		t.Fatalf("after-pause stdout = %q", resp.Stdout)
	}

	// Dormant: snapshot guest memory to disk, restore paused, then wake.
	if err := s.d.Sleep(ctx, sb.ID, true); err != nil {
		t.Fatalf("offload: %v", err)
	}
	got, _ = s.d.Get(ctx, sb.ID)
	if got.State != types.StateDormant {
		t.Fatalf("state after offload = %s", got.State)
	}
	if _, err := os.Stat(filepath.Join(got.SnapshotDir, "mem")); err != nil {
		t.Fatalf("memory image missing: %v", err)
	}
	if err := s.d.Wake(ctx, sb.ID); err != nil {
		t.Fatalf("wake: %v", err)
	}
	got, _ = s.d.Get(ctx, sb.ID)
	if got.State != types.StateRunning || got.WakeCount == 0 {
		t.Fatalf("after wake: state=%s wakes=%d", got.State, got.WakeCount)
	}
	resp, err = s.d.Exec(ctx, sb.ID, []string{"/bin/sh", "-c", "echo done; uname -s"}, nil)
	if err != nil {
		t.Fatalf("exec after wake: %v", err)
	}
	if !strings.Contains(resp.Stdout, "done") {
		t.Fatalf("post-wake stdout = %q", resp.Stdout)
	}

	if err := s.d.Delete(ctx, sb.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.d.Get(ctx, sb.ID); !errors.Is(err, manager.ErrNotFound) {
		t.Fatalf("sandbox should be gone, err=%v", err)
	}
}
