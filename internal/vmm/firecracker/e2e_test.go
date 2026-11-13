package firecracker

// Real end-to-end test of the Firecracker driver and the guest agent, gated on
// FASTVM_E2E=1 plus a working KVM host and guest assets. It builds the static
// agent, injects it into an ext4 rootfs built from an unpacked Alpine rootfs
// (mke2fs -d), then:
//
//   - boots a real microVM through Driver.Boot,
//   - waits for the guest agent Ready handshake over virtio-vsock,
//   - execs commands and checks stdout/exit codes,
//   - pauses and resumes the vCPUs and checks that a guest counter froze,
//   - snapshots the paused VM, stops the VMM, restores the memory file into a
//     fresh Firecracker process paused (dormant), resumes, and verifies that the
//     guest filesystem and agent still work.
//
// Environment:
//
//	FASTVM_E2E=1                enable (otherwise skipped)
//	FASTVM_KERNEL               kernel image (default /tmp/fastvm-derisk/assets/vmlinux.bin)
//	FASTVM_ROOTFS               unpacked guest rootfs directory, or an ext4 image that
//	                            is unpacked read-only with debugfs (default
//	                            /tmp/fastvm-derisk/assets/rootfs)
//	FASTVM_ROOTFS_DIR           unpacked guest rootfs directory (takes precedence)
//	FASTVM_FIRECRACKER          firecracker binary (default "firecracker" on PATH)
//
// No TAP/network and no root are required.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"fastvm/internal/agent"
	"fastvm/internal/vmm"
)

const (
	e2eKernelDefault  = "/tmp/fastvm-derisk/assets/vmlinux.bin"
	e2eRootfsDefault  = "/tmp/fastvm-derisk/assets/rootfs"
	e2eDiskSize       = 256 << 20
	e2eBootTimeout    = 30 * time.Second
	e2eCommandTimeout = 20 * time.Second
)

func TestE2EFirecracker(t *testing.T) {
	if os.Getenv("FASTVM_E2E") != "1" {
		t.Skip("real Firecracker e2e: set FASTVM_E2E=1 (requires /dev/kvm and guest assets)")
	}
	kernel := envOr("FASTVM_KERNEL", e2eKernelDefault)
	fcBinary := envOr("FASTVM_FIRECRACKER", "firecracker")

	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm not available: %v", err)
	}
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
		t.Skipf("/dev/kvm not openable by this user: %v", err)
	} else {
		_ = f.Close()
	}
	if _, err := os.Stat(kernel); err != nil {
		t.Skipf("kernel image %s: %v (set FASTVM_KERNEL)", kernel, err)
	}
	rootfsDir := resolveRootfsE2E(t)
	if _, err := exec.LookPath("mke2fs"); err != nil {
		t.Skipf("mke2fs not available: %v", err)
	}
	fcPath, err := exec.LookPath(fcBinary)
	if err != nil {
		t.Skipf("firecracker binary %q: %v", fcBinary, err)
	}

	agentBin := buildAgentBinaryE2E(t)
	baseDisk := buildBaseDiskE2E(t, rootfsDir, agentBin)
	d, err := NewDriver(Options{
		Binary:         fcPath,
		BootTimeout:    e2eBootTimeout,
		RequestTimeout: 10 * time.Second,
		StopTimeout:    10 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}

	t.Run("BootExecPauseResume", func(t *testing.T) {
		testE2EBootExecPauseResume(t, d, kernel, baseDisk)
	})
	t.Run("SnapshotRestorePaused", func(t *testing.T) {
		testE2ESnapshotRestore(t, d, kernel, baseDisk)
	})
}

func testE2EBootExecPauseResume(t *testing.T, d vmm.Driver, kernel, baseDisk string) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.ext4")
	copySparseFile(t, baseDisk, disk)
	cfg := e2eBootConfig(dir, kernel, disk)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	vm, cl := e2eBootAndDial(t, ctx, d, cfg, cfg.VsockUDPath)

	// Exec captures stdout/stderr and exit codes.
	resp := e2eExec(t, ctx, cl, "/bin/echo", "hello")
	if resp.ExitCode != 0 || resp.Stdout != "hello\n" {
		t.Fatalf("echo hello: %+v", resp)
	}
	resp = e2eExec(t, ctx, cl, "/bin/sh", "-c", "echo out; echo err >&2; exit 7")
	if resp.ExitCode != 7 || resp.Stdout != "out\n" || resp.Stderr != "err\n" {
		t.Fatalf("exec exit codes: %+v", resp)
	}

	// The serial console was configured by the driver and shows the init marker.
	waitForFileContains(t, cfg.SerialPath, "FASTVM_BOOT_OK", 10*time.Second)

	// Start a CPU-busy counter workload and measure its baseline rate.
	resp = e2eExecDetach(t, ctx, cl, true, "/bin/sh", "-c",
		"i=0; while :; do i=$((i+1)); echo $i > /tmp/counter; done")
	if resp.Error != "" {
		t.Fatalf("detach counter workload: %+v", resp)
	}
	before := waitForCounter(t, ctx, cl)
	time.Sleep(500 * time.Millisecond)
	baseline := waitForCounter(t, ctx, cl) - before
	if baseline <= 0 {
		t.Fatalf("baseline counter did not advance (before=%d)", before)
	}

	// Pause: the guest is frozen, so the counter must not advance during 2s.
	before = waitForCounter(t, ctx, cl)
	if err := vm.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	time.Sleep(2 * time.Second)
	if err := vm.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	after := waitForCounter(t, ctx, cl)
	frozenDelta := after - before
	if frozenDelta < 0 {
		t.Fatalf("counter went backwards across pause: %d -> %d", before, after)
	}
	// While paused for 2s the counter should advance by less than the baseline
	// rate measured over 500ms; if vCPUs kept running it would advance ~4x that.
	if frozenDelta > baseline {
		t.Fatalf("counter advanced %d across a 2s pause (baseline 500ms = %d): vCPUs did not stop", frozenDelta, baseline)
	}
	resp = e2eExec(t, ctx, cl, "/bin/echo", "after-resume")
	if resp.Stdout != "after-resume\n" {
		t.Fatalf("exec after resume: %+v", resp)
	}

	// Stats work against the live VMM process.
	if _, err := vm.Stats(ctx); err != nil {
		t.Fatalf("Stats: %v", err)
	}
}

func testE2ESnapshotRestore(t *testing.T, d vmm.Driver, kernel, baseDisk string) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.ext4")
	copySparseFile(t, baseDisk, disk)
	// Snapshot restore requires the disk at the same path, so keep one copy.
	cfg := e2eBootConfig(dir, kernel, disk)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	vm, cl := e2eBootAndDial(t, ctx, d, cfg, cfg.VsockUDPath)
	if resp := e2eExec(t, ctx, cl, "/bin/sh", "-c", "echo persisted > /persist.txt && sync"); resp.ExitCode != 0 {
		t.Fatalf("write persist file: %+v", resp)
	}
	if resp := e2eExec(t, ctx, cl, "/bin/cat", "/persist.txt"); resp.Stdout != "persisted\n" {
		t.Fatalf("read persist file before snapshot: %+v", resp)
	}

	snapDir := filepath.Join(dir, "memory")
	snap, err := vm.Snapshot(ctx, snapDir)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	for _, p := range []string{snap.StatePath, snap.MemPath} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("snapshot file %s: %v", p, err)
		}
		if st.Size() == 0 {
			t.Fatalf("snapshot file %s is empty", p)
		}
	}
	if err := vm.Stop(ctx); err != nil {
		t.Fatalf("Stop original VM: %v", err)
	}
	_ = cl.Close()
	// The original process must be gone before the same disk/snapshot is reused.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer waitCancel()
	_ = vm.Wait(waitCtx)

	// Reload the paused snapshot into a fresh Firecracker process, dormant.
	restoreCfg := vmm.RestoreConfig{
		SandboxID:   cfg.SandboxID,
		Snapshot:    snap,
		VsockUDPath: filepath.Join(dir, "vsock-restore.sock"),
		APISocket:   filepath.Join(dir, "api-restore.sock"),
		SerialPath:  filepath.Join(dir, "serial-restore.log"),
		Resume:      false, // dormant: device state loaded, vCPUs halted
	}
	restored, err := d.Restore(ctx, restoreCfg)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer stopCancel()
		_ = restored.Stop(stopCtx)
	})
	if restored.APISocket() != restoreCfg.APISocket {
		t.Fatalf("restored APISocket = %q", restored.APISocket())
	}

	// The restored VM is paused; resume, then reconnect the agent channel (vsock
	// resets across restore) and verify both the filesystem state and exec.
	if err := restored.Resume(ctx); err != nil {
		t.Fatalf("Resume restored VM: %v", err)
	}
	cl2 := e2eDial(t, ctx, restoreCfg.VsockUDPath)
	t.Cleanup(func() { _ = cl2.Close() })
	if resp := e2eExec(t, ctx, cl2, "/bin/cat", "/persist.txt"); resp.Stdout != "persisted\n" {
		t.Fatalf("filesystem state lost across snapshot+restore: %+v", resp)
	}
	if resp := e2eExec(t, ctx, cl2, "/bin/echo", "after-restore"); resp.Stdout != "after-restore\n" {
		t.Fatalf("exec after restore: %+v", resp)
	}
}

// e2eBootConfig builds a boot config in dir.
func e2eBootConfig(dir, kernel, disk string) vmm.BootConfig {
	return vmm.BootConfig{
		SandboxID:   "e2e",
		KernelPath:  kernel,
		DiskPath:    disk,
		VCPUs:       1,
		MemoryMB:    128,
		GuestCID:    3,
		VsockUDPath: filepath.Join(dir, "vsock.sock"),
		APISocket:   filepath.Join(dir, "api.sock"),
		SerialPath:  filepath.Join(dir, "serial.log"),
		Cmdline:     DefaultCmdline,
	}
}

// e2eBootAndDial boots vm, waits for the guest agent and returns a client. The
// VM is stopped on test cleanup.
func e2eBootAndDial(t *testing.T, ctx context.Context, d vmm.Driver, cfg vmm.BootConfig, vsockPath string) (vmm.VM, agent.Client) {
	t.Helper()
	vm, err := d.Boot(ctx, cfg)
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer stopCancel()
		_ = vm.Stop(stopCtx)
	})
	cl := e2eDial(t, ctx, vsockPath)
	t.Cleanup(func() { _ = cl.Close() })
	return vm, cl
}

func e2eDial(t *testing.T, ctx context.Context, vsockPath string) agent.Client {
	t.Helper()
	dialCtx, cancel := context.WithTimeout(ctx, e2eBootTimeout)
	defer cancel()
	cl, err := agent.DialFirecracker(dialCtx, vsockPath, 3, e2eBootTimeout)
	if err != nil {
		t.Fatalf("DialFirecracker: %v", err)
	}
	readyCtx, readyCancel := context.WithTimeout(ctx, e2eBootTimeout)
	defer readyCancel()
	if err := cl.WaitReady(readyCtx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	return cl
}

func e2eExec(t *testing.T, ctx context.Context, cl agent.Client, argv ...string) agent.Response {
	t.Helper()
	return e2eExecDetach(t, ctx, cl, false, argv...)
}

func e2eExecDetach(t *testing.T, ctx context.Context, cl agent.Client, detach bool, argv ...string) agent.Response {
	t.Helper()
	execCtx, cancel := context.WithTimeout(ctx, e2eCommandTimeout)
	defer cancel()
	resp, err := cl.Exec(execCtx, agent.Request{
		Op:        agent.OpExec,
		Argv:      argv,
		Detach:    detach,
		TimeoutMS: int(e2eCommandTimeout / time.Millisecond),
	})
	if err != nil {
		t.Fatalf("Exec %v: %v", argv, err)
	}
	return resp
}

// waitForCounter reads the guest counter file until it holds a number.
func waitForCounter(t *testing.T, ctx context.Context, cl agent.Client) int64 {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last string
	for {
		resp := e2eExec(t, ctx, cl, "/bin/cat", "/tmp/counter")
		text := strings.TrimSpace(resp.Stdout)
		if n, err := strconv.ParseInt(text, 10, 64); err == nil {
			return n
		}
		last = text
		if time.Now().After(deadline) {
			t.Fatalf("counter file never contained a number (last output %q, response %+v)", last, resp)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// buildAgentBinaryE2E builds the guest agent statically (CGO_ENABLED=0).
func buildAgentBinaryE2E(t *testing.T) string {
	t.Helper()
	root := moduleRoot(t)
	out := filepath.Join(t.TempDir(), "fastvm-agent")
	cmd := exec.Command("go", "build", "-trimpath", "-o", out, "./cmd/fastvm-agent")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build guest agent: %v\n%s", err, output)
	}
	return out
}

// buildBaseDiskE2E builds a bootable ext4 image containing rootfsDir plus the
// agent and init script. Ext4 features are pinned to what the pinned 4.14 guest
// kernel supports (modern mke2fs enables orphan_file, which 4.14 cannot mount).
func buildBaseDiskE2E(t *testing.T, rootfsDir, agentBin string) string {
	t.Helper()
	dir := t.TempDir()
	staged := filepath.Join(dir, "rootfs")
	copyTree(t, rootfsDir, staged)

	agentDst := filepath.Join(staged, "usr", "local", "bin", "fastvm-agent")
	if err := os.MkdirAll(filepath.Dir(agentDst), 0o755); err != nil {
		t.Fatalf("staged agent dir: %v", err)
	}
	copyFileMode(t, agentBin, agentDst, 0o755)

	initSrc := filepath.Join(moduleRoot(t), "guest", "init.sh")
	copyFileMode(t, initSrc, filepath.Join(staged, "init"), 0o755)

	disk := filepath.Join(dir, "base.ext4")
	f, err := os.Create(disk)
	if err != nil {
		t.Fatalf("create disk: %v", err)
	}
	if err := f.Truncate(e2eDiskSize); err != nil {
		_ = f.Close()
		t.Fatalf("size disk: %v", err)
	}
	_ = f.Close()

	cmd := exec.Command("mke2fs", "-q", "-F", "-t", "ext4", "-O", "^orphan_file", "-d", staged, disk)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mke2fs: %v\n%s", err, output)
	}
	return disk
}

// resolveRootfsE2E locates an unpacked guest rootfs, accepting either a
// directory or an ext4 image (unpacked read-only with debugfs) via
// FASTVM_ROOTFS.
func resolveRootfsE2E(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv("FASTVM_ROOTFS_DIR"); dir != "" {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			t.Skipf("FASTVM_ROOTFS_DIR %s is not a directory", dir)
		}
		return dir
	}
	source := os.Getenv("FASTVM_ROOTFS")
	if source == "" {
		source = e2eRootfsDefault
	}
	st, err := os.Stat(source)
	if err != nil {
		t.Skipf("guest rootfs %s: %v (set FASTVM_ROOTFS)", source, err)
	}
	if st.IsDir() {
		return source
	}
	if _, err := exec.LookPath("debugfs"); err != nil {
		t.Skipf("rootfs %s is an image and debugfs is unavailable: %v", source, err)
	}
	dir := filepath.Join(t.TempDir(), "rootfs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create unpacked rootfs dir: %v", err)
	}
	cmd := exec.Command("debugfs", "-R", fmt.Sprintf("rdump / %s", dir), source)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("unpack rootfs %s with debugfs: %v\n%s", source, err, output)
	}
	if _, err := os.Stat(filepath.Join(dir, "bin", "busybox")); err != nil {
		t.Fatalf("unpacked rootfs %s does not contain /bin/busybox: %v", source, err)
	}
	return dir
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above working directory")
		}
		dir = parent
	}
}

// copyTree copies a rootfs directory preserving modes and symlinks.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, de os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := de.Info()
		if err != nil {
			return err
		}
		switch {
		case de.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			return copyFileModeErr(path, target, info.Mode().Perm())
		default:
			// Device nodes/FIFOs do not occur in the fastvm rootfs images; skip.
			return nil
		}
	})
	if err != nil {
		t.Fatalf("copy rootfs %s -> %s: %v", src, dst, err)
	}
}

func copyFileMode(t *testing.T, src, dst string, mode os.FileMode) {
	t.Helper()
	if err := copyFileModeErr(src, dst, mode); err != nil {
		t.Fatalf("copy %s -> %s: %v", src, dst, err)
	}
}

func copyFileModeErr(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// copySparseFile copies src to dst, preserving holes and the total size.
func copySparseFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("open %s: %v", src, err)
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		t.Fatalf("stat %s: %v", src, err)
	}
	out, err := os.Create(dst)
	if err != nil {
		t.Fatalf("create %s: %v", dst, err)
	}
	buf := make([]byte, 1<<20)
	var offset int64
	for {
		n, readErr := in.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if isZero(chunk) {
				if _, err := out.Seek(int64(n), io.SeekCurrent); err != nil {
					_ = out.Close()
					t.Fatalf("seek %s: %v", dst, err)
				}
			} else if _, err := out.Write(chunk); err != nil {
				_ = out.Close()
				t.Fatalf("write %s: %v", dst, err)
			}
			offset += int64(n)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = out.Close()
			t.Fatalf("read %s: %v", src, readErr)
		}
	}
	if err := out.Truncate(offset); err != nil {
		_ = out.Close()
		t.Fatalf("truncate %s: %v", dst, err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close %s: %v", dst, err)
	}
	if st.Size() != offset {
		t.Fatalf("copied %d bytes, want %d", offset, st.Size())
	}
}

func isZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

func waitForFileContains(t *testing.T, path, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("file %s did not contain %q within %s (last error %v)", path, want, timeout, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
