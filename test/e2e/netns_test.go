package e2e

import (
	"bufio"
	"context"
	"log/slog"
	"net"
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

// TestDaemonNetworkWakeRealVM proves the queue-live wake path with a real TAP,
// bridge, guest NIC, and Firecracker VM.
//
// The current user has no CAP_NET_ADMIN in the initial namespaces, so the test
// re-executes itself under `unshare -Urn -r` (user+net namespace) where TAP,
// bridge and nftables are available. The child process runs the real daemon and
// dials the guest directly from the same network namespace.
func TestDaemonNetworkWakeRealVM(t *testing.T) {
	if os.Getenv("FASTVM_NS_CHILD") == "1" {
		runNetworkWakeChild(t)
		return
	}
	requireE2E(t)
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip("unshare not available")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "unshare", "-Urn", "-r", exe,
		"-test.run", "^TestDaemonNetworkWakeRealVM$", "-test.v", "-test.timeout", "5m")
	cmd.Env = append(os.Environ(), "FASTVM_NS_CHILD=1", "FASTVM_E2E=1")
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("network e2e child failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "--- PASS: TestDaemonNetworkWakeRealVM") {
		t.Fatalf("network e2e child did not pass:\n%s", out)
	}
	t.Logf("network e2e child output:\n%s", out)
}

func runNetworkWakeChild(t *testing.T) {
	if os.Getenv("FASTVM_E2E") == "" {
		t.Skip("FASTVM_E2E not set")
	}
	kernel := envOr("FASTVM_KERNEL", filepath.Join(repoRoot(t), "test", "e2e", "assets", "vmlinux.bin"))
	rootfs := envOr("FASTVM_ROOTFS", filepath.Join(repoRoot(t), "test", "e2e", "assets", "rootfs"))
	if _, err := os.Stat(kernel); err != nil {
		t.Skipf("kernel missing: %v", err)
	}
	if _, err := os.Stat(rootfs); err != nil {
		t.Skipf("rootfs missing: %v", err)
	}

	tmp := t.TempDir()
	agentBin := buildAgent(t)
	echoBin := buildGoBinary(t, "./test/e2e/echo", "netecho")
	rootfsCopy := filepath.Join(tmp, "rootfs")
	if out, err := exec.Command("cp", "-a", rootfs+"/.", rootfsCopy).CombinedOutput(); err != nil {
		t.Fatalf("copy rootfs: %v\n%s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(rootfsCopy, "usr", "local", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(echoBin, filepath.Join(rootfsCopy, "usr", "local", "bin", "netecho"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.DataDir = filepath.Join(tmp, "data")
	cfg.KernelPath = kernel
	cfg.AgentPath = agentBin
	cfg.Lifecycle.IdleWindow = config.Duration(30 * time.Second)
	cfg.Wake.CoalesceWindow = config.Duration(10 * time.Millisecond)
	cfg.Switch.Mode = "bridge"

	st, err := store.Open(filepath.Join(tmp, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	imgs, err := image.NewBuilder(image.Options{BaseDir: filepath.Join(tmp, "images"), AgentPath: agentBin})
	if err != nil {
		t.Fatal(err)
	}
	vols, err := volume.NewManager(volume.Options{VolDir: filepath.Join(tmp, "volumes"), DefaultCOW: "copy"})
	if err != nil {
		t.Fatal(err)
	}
	netMgr, err := network.NewManager(network.DefaultOptions()) // bridge mode, 10.77.0.0/16
	if err != nil {
		t.Fatalf("network manager (must run under unshare -Urn): %v", err)
	}
	t.Cleanup(func() { _ = netMgr.Close() })
	drv, err := firecracker.NewDriver(firecracker.Options{Binary: envOr("FASTVM_FIRECRACKER", "firecracker"), RunDir: filepath.Join(tmp, "run"), BootTimeout: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	dial := func(ctx context.Context, uds string, cid uint32) (agent.Client, error) {
		return agent.DialFirecracker(ctx, uds, cid, 30*time.Second)
	}
	d, err := daemon.New(daemon.Deps{
		Config: cfg, Store: st, Images: imgs, Volumes: vols, Network: netMgr,
		VMM: drv, DialAgent: dial,
		Log: slog.New(slog.NewTextHandler(testWriter{t}, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	base, err := imgs.Ingest(ctx, image.IngestSpec{RootfsDir: rootfsCopy, Tag: "alpine:net"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	sb, err := d.Create(ctx, manager.CreateOptions{
		Name: "net-e2e", Image: base.Identity(), NoAutoSleep: true,
		Cmd: []string{"/usr/local/bin/netecho", "--port", "8080"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Logf("sandbox %s ip=%s tap=%s gateway=%s", sb.ID, sb.IP, sb.TAPName, sb.Gateway)
	if sb.TAPName == "" || sb.IP == "" {
		t.Fatal("sandbox has no network attachment")
	}
	defer func() { _ = d.Delete(context.Background(), sb.ID) }()

	addr := net.JoinHostPort(sb.IP, "8080")
	waitFor(t, 30*time.Second, func() bool {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			return false
		}
		defer conn.Close()
		line, _ := bufio.NewReader(conn).ReadString('\n')
		return strings.TrimSpace(line) == "wake-ok"
	})

	// Pause: TAP and the guest device queue stay live. Traffic must wake it.
	if err := d.Sleep(ctx, sb.ID, false); err != nil {
		t.Fatalf("pause: %v", err)
	}
	got, _ := d.Get(ctx, sb.ID)
	if got.State != types.StatePaused {
		t.Fatalf("state = %s, want Paused", got.State)
	}

	// A connection attempt at a paused sandbox must be queued and serviced after
	// wake; a generous timeout keeps wake latency distinguishable from failure.
	t0 := time.Now()
	conn, err := net.DialTimeout("tcp", addr, 20*time.Second)
	if err != nil {
		t.Fatalf("connect to paused sandbox did not wake it: %v", err)
	}
	connected := time.Since(t0)
	line, _ := bufio.NewReader(conn).ReadString('\n')
	_ = conn.Close()
	if strings.TrimSpace(line) != "wake-ok" {
		t.Fatalf("paused-sandbox response = %q", line)
	}
	t.Logf("wake-on-network latency: connect=%s response=%s total=%s", connected, time.Since(t0), time.Since(t0))

	waitFor(t, 5*time.Second, func() bool {
		cur, err := d.Get(ctx, sb.ID)
		return err == nil && (cur.State == types.StateRunning || cur.State == types.StateIdle) && cur.WakeCount > 0
	})
	t.Logf("wake-on-network succeeded; wake count = %d", mustGet(t, d, sb.ID).WakeCount)
}

func mustGet(t *testing.T, d *daemon.Daemon, id string) types.Sandbox {
	t.Helper()
	sb, err := d.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return sb
}

func buildGoBinary(t *testing.T, pkg, name string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, b)
	}
	return out
}

func copyFile(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, mode)
}
