package firecracker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fastvm/internal/vmm"
)

// testDriver returns a Driver whose VMM binary is this test binary re-executed
// as the fake Firecracker helper.
func testDriver(t *testing.T, env map[string]string) (*Driver, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	logPath := filepath.Join(t.TempDir(), "vmm-requests.jsonl")
	t.Setenv("FASTVM_FAKE_VMM", "1")
	t.Setenv("FASTVM_FAKE_VMM_LOG", logPath)
	for k, v := range env {
		t.Setenv(k, v)
	}
	d := &Driver{
		binary:         exe,
		bootTimeout:    5 * time.Second,
		requestTimeout: 2 * time.Second,
		stopTimeout:    2 * time.Second,
		args:           []string{"-test.run=TestFakeVMMProcess", "--"},
	}
	return d, logPath
}

func testBootConfig(t *testing.T) vmm.BootConfig {
	t.Helper()
	dir := t.TempDir()
	return vmm.BootConfig{
		SandboxID:   "sbx-test",
		KernelPath:  filepath.Join(dir, "vmlinux.bin"),
		DiskPath:    filepath.Join(dir, "disk.ext4"),
		VCPUs:       2,
		MemoryMB:    256,
		GuestCID:    42,
		VsockUDPath: filepath.Join(dir, "vsock.sock"),
		APISocket:   filepath.Join(dir, "api.sock"),
		SerialPath:  filepath.Join(dir, "serial.log"),
	}
}

func stopVM(t *testing.T, vm vmm.VM) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = vm.Stop(ctx)
}

func bodyMap(t *testing.T, r recordedRequest) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.Body), &m); err != nil {
		t.Fatalf("decode body %q: %v", r.Body, err)
	}
	return m
}

func TestDriverBootAndLifecycle(t *testing.T) {
	d, logPath := testDriver(t, nil)
	cfg := testBootConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	vm, err := d.Boot(ctx, cfg)
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { stopVM(t, vm) })
	if vm.APISocket() != cfg.APISocket {
		t.Fatalf("APISocket = %q, want %q", vm.APISocket(), cfg.APISocket)
	}

	// Configuration order: devices before InstanceStart.
	reqs := nonPingRequests(readRequests(t, logPath))
	wantOrder := []string{"/boot-source", "/drives/rootfs", "/machine-config", "/serial", "/vsock", "/actions"}
	if len(reqs) != len(wantOrder) {
		t.Fatalf("boot made %d requests, want %d: %+v", len(reqs), len(wantOrder), reqs)
	}
	for i, want := range wantOrder {
		if reqs[i].Path != want {
			t.Fatalf("request %d path = %q, want %q", i, reqs[i].Path, want)
		}
	}
	boot := bodyMap(t, reqs[0])
	if boot["kernel_image_path"] != cfg.KernelPath {
		t.Fatalf("boot kernel = %v", boot["kernel_image_path"])
	}
	if boot["boot_args"] != DefaultCmdline {
		t.Fatalf("boot args = %v, want default %q", boot["boot_args"], DefaultCmdline)
	}
	machine := bodyMap(t, reqs[2])
	if machine["vcpu_count"] != float64(2) || machine["mem_size_mib"] != float64(256) {
		t.Fatalf("machine config = %v", machine)
	}
	if serialBody := bodyMap(t, reqs[3]); serialBody["serial_out_path"] != cfg.SerialPath {
		t.Fatalf("serial config = %v", serialBody)
	}
	vsockBody := bodyMap(t, reqs[4])
	if vsockBody["guest_cid"] != float64(42) {
		t.Fatalf("vsock cid = %v", vsockBody["guest_cid"])
	}
	if uds, _ := vsockBody["uds_path"].(string); uds != cfg.VsockUDPath {
		t.Fatalf("vsock uds = %v", vsockBody["uds_path"])
	}
	start := bodyMap(t, reqs[5])
	if start["action_type"] != "InstanceStart" {
		t.Fatalf("start action = %v", start)
	}

	// Pause is idempotent and issues exactly one PATCH /vm.
	if err := vm.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	reqs = nonPingRequests(readRequests(t, logPath))
	last := reqs[len(reqs)-1]
	if last.Method != "PATCH" || last.Path != "/vm" || bodyMap(t, last)["state"] != "Paused" {
		t.Fatalf("pause request = %+v", last)
	}
	countAfterPause := len(reqs)
	if err := vm.Pause(ctx); err != nil {
		t.Fatalf("second Pause: %v", err)
	}
	if got := len(nonPingRequests(readRequests(t, logPath))); got != countAfterPause {
		t.Fatalf("repeated Pause issued extra requests (%d -> %d)", countAfterPause, got)
	}

	if err := vm.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	reqs = nonPingRequests(readRequests(t, logPath))
	last = reqs[len(reqs)-1]
	if last.Method != "PATCH" || bodyMap(t, last)["state"] != "Resumed" {
		t.Fatalf("resume request = %+v", last)
	}

	// Stats come from /proc and must work while the process is alive (exact
	// CPU/IO values are environment dependent; see stats_test.go).
	if _, err := vm.Stats(ctx); err != nil {
		t.Fatalf("Stats: %v", err)
	}

	// Snapshot pauses and creates vmstate+mem via the API.
	snapDir := filepath.Join(t.TempDir(), "memory")
	snap, err := vm.Snapshot(ctx, snapDir)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Dir != snapDir {
		t.Fatalf("snapshot dir = %q", snap.Dir)
	}
	for _, p := range []string{snap.StatePath, snap.MemPath} {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("snapshot file %s: %v", p, err)
		}
		if string(data) != "fake-snapshot" {
			t.Fatalf("snapshot file %s = %q", p, data)
		}
	}
	reqs = nonPingRequests(readRequests(t, logPath))
	last = reqs[len(reqs)-1]
	if last.Path != "/snapshot/create" || bodyMap(t, last)["snapshot_type"] != "Full" {
		t.Fatalf("snapshot request = %+v", last)
	}
	if bodyMap(t, last)["snapshot_path"] != snap.StatePath || bodyMap(t, last)["mem_file_path"] != snap.MemPath {
		t.Fatalf("snapshot paths = %v", last.Body)
	}

	// Stop ends the process; stats then fail.
	if err := vm.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := vm.Stats(ctx); err == nil {
		t.Fatal("Stats after Stop should fail")
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer waitCancel()
	_ = vm.Wait(waitCtx) // returns the exit error for a signaled process
}

func TestDriverRunDirDefaults(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "requests.jsonl")
	t.Setenv("FASTVM_FAKE_VMM", "1")
	t.Setenv("FASTVM_FAKE_VMM_LOG", logPath)
	runDir := t.TempDir()
	d := &Driver{
		binary:         exe,
		runDir:         runDir,
		bootTimeout:    5 * time.Second,
		requestTimeout: 2 * time.Second,
		stopTimeout:    2 * time.Second,
		args:           []string{"-test.run=TestFakeVMMProcess", "--"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg := testBootConfig(t)
	cfg.APISocket = ""
	cfg.SerialPath = ""
	cfg.Cmdline = ""
	vm, err := d.Boot(ctx, cfg)
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { stopVM(t, vm) })

	if want := filepath.Join(runDir, "api.sock"); vm.APISocket() != want {
		t.Fatalf("APISocket = %q, want %q", vm.APISocket(), want)
	}
	reqs := nonPingRequests(readRequests(t, logPath))
	serial := bodyMap(t, reqs[3])
	if serial["serial_out_path"] != filepath.Join(runDir, "serial.log") {
		t.Fatalf("serial path = %v", serial["serial_out_path"])
	}
	boot := bodyMap(t, reqs[0])
	if boot["boot_args"] != DefaultCmdline {
		t.Fatalf("boot args = %v", boot["boot_args"])
	}
	if _, err := os.Stat(filepath.Join(runDir, "vmm.log")); err != nil {
		t.Fatalf("vmm.log missing: %v", err)
	}
}

func TestDriverBootNetworkInterface(t *testing.T) {
	d, logPath := testDriver(t, nil)
	cfg := testBootConfig(t)
	cfg.TAPName = "tap-sbx"
	cfg.MAC = "06:00:ac:10:00:02"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	vm, err := d.Boot(ctx, cfg)
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { stopVM(t, vm) })

	var netReq *recordedRequest
	reqs := nonPingRequests(readRequests(t, logPath))
	for i := range reqs {
		if reqs[i].Path == "/network-interfaces/eth0" {
			netReq = &reqs[i]
		}
	}
	if netReq == nil {
		t.Fatalf("no network-interface request in %+v", reqs)
	}
	body := bodyMap(t, *netReq)
	if body["host_dev_name"] != "tap-sbx" || body["guest_mac"] != cfg.MAC {
		t.Fatalf("network config = %v", body)
	}
	if reqs[len(reqs)-1].Path != "/actions" {
		t.Fatalf("InstanceStart is not last: %+v", reqs)
	}
}

func TestDriverBootProcessExitIncludesStderrTail(t *testing.T) {
	d, _ := testDriver(t, map[string]string{"FASTVM_FAKE_VMM_MODE": "exit"})
	d.bootTimeout = 3 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := d.Boot(ctx, testBootConfig(t))
	if err == nil {
		t.Fatal("Boot should fail when the VMM exits")
	}
	msg := err.Error()
	if !strings.Contains(msg, "VMM exited before the API became ready") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(msg, "kernel rejected by fake VMM") {
		t.Fatalf("error does not include VMM stderr tail: %v", err)
	}
}

func TestDriverBootAPIFailure(t *testing.T) {
	d, _ := testDriver(t, map[string]string{"FASTVM_FAKE_VMM_FAIL_PATH": "/boot-source"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := d.Boot(ctx, testBootConfig(t))
	if err == nil {
		t.Fatal("Boot should fail when boot-source is rejected")
	}
	msg := err.Error()
	if !strings.Contains(msg, "boot-source") || !strings.Contains(msg, "injected failure") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDriverBootValidation(t *testing.T) {
	d, _ := testDriver(t, nil)
	ctx := context.Background()
	if _, err := d.Boot(ctx, vmm.BootConfig{DiskPath: "/disk"}); err == nil {
		t.Fatal("missing kernel should fail")
	}
	if _, err := d.Boot(ctx, vmm.BootConfig{KernelPath: "/kernel"}); err == nil {
		t.Fatal("missing disk should fail")
	}
	cfg := testBootConfig(t)
	cfg.APISocket = ""
	d.runDir = ""
	if _, err := d.Boot(ctx, cfg); err == nil {
		t.Fatal("missing API socket should fail")
	}
}

func TestDriverRestoreRunning(t *testing.T) {
	d, logPath := testDriver(t, nil)
	snap := fakeSnapshot(t)
	restoreCfg := testRestoreConfig(t, snap)
	restoreCfg.Resume = true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	vm, err := d.Restore(ctx, restoreCfg)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	t.Cleanup(func() { stopVM(t, vm) })

	reqs := nonPingRequests(readRequests(t, logPath))
	if len(reqs) != 1 || reqs[0].Path != "/snapshot/load" {
		t.Fatalf("restore requests = %+v", reqs)
	}
	body := bodyMap(t, reqs[0])
	if body["snapshot_path"] != snap.StatePath || body["resume_vm"] != true {
		t.Fatalf("snapshot load body = %v", body)
	}
	mem, ok := body["mem_backend"].(map[string]any)
	if !ok || mem["backend_type"] != "File" || mem["backend_path"] != snap.MemPath {
		t.Fatalf("mem_backend = %v", body["mem_backend"])
	}
	vsock, ok := body["vsock_override"].(map[string]any)
	if !ok || vsock["uds_path"] != restoreCfg.VsockUDPath || vsock["vsock_id"] != VsockID {
		t.Fatalf("vsock_override = %v", body["vsock_override"])
	}

	// The VM was restored running: Resume is a no-op.
	if err := vm.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := len(nonPingRequests(readRequests(t, logPath))); got != 1 {
		t.Fatalf("resume after running restore issued requests (%d)", got)
	}
}

func TestDriverRestorePausedThenResume(t *testing.T) {
	d, logPath := testDriver(t, nil)
	snap := fakeSnapshot(t)
	restoreCfg := testRestoreConfig(t, snap)
	restoreCfg.Resume = false
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	vm, err := d.Restore(ctx, restoreCfg)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	t.Cleanup(func() { stopVM(t, vm) })
	if body := bodyMap(t, nonPingRequests(readRequests(t, logPath))[0]); body["resume_vm"] != false {
		t.Fatalf("resume_vm = %v, want false", body["resume_vm"])
	}
	if err := vm.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	reqs := nonPingRequests(readRequests(t, logPath))
	last := reqs[len(reqs)-1]
	if last.Method != "PATCH" || bodyMap(t, last)["state"] != "Resumed" {
		t.Fatalf("resume request = %+v", last)
	}
}

func TestDriverRestoreValidation(t *testing.T) {
	d, _ := testDriver(t, nil)
	ctx := context.Background()
	if _, err := d.Restore(ctx, vmm.RestoreConfig{}); err == nil {
		t.Fatal("empty snapshot should fail")
	}
	if _, err := d.Restore(ctx, vmm.RestoreConfig{Snapshot: vmm.Snapshot{StatePath: "/missing/vmstate", MemPath: "/missing/mem"}}); err == nil {
		t.Fatal("missing snapshot files should fail")
	}
}

func TestDriverRestoreNetworkOverride(t *testing.T) {
	d, logPath := testDriver(t, nil)
	restoreCfg := testRestoreConfig(t, fakeSnapshot(t))
	restoreCfg.Resume = true
	restoreCfg.TAPName = "tap-sbx"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	vm, err := d.Restore(ctx, restoreCfg)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	t.Cleanup(func() { stopVM(t, vm) })
	body := bodyMap(t, nonPingRequests(readRequests(t, logPath))[0])
	overrides, ok := body["network_overrides"].([]any)
	if !ok || len(overrides) != 1 {
		t.Fatalf("network_overrides = %v", body["network_overrides"])
	}
	entry, _ := overrides[0].(map[string]any)
	if entry["iface_id"] != NetIfaceID || entry["host_dev_name"] != "tap-sbx" {
		t.Fatalf("network override = %v", entry)
	}
}

func TestNewDriver(t *testing.T) {
	if _, err := NewDriver(Options{Binary: filepath.Join(t.TempDir(), "no-such-firecracker")}); err == nil {
		t.Fatal("missing binary should fail")
	}
	d, err := NewDriver(Options{Binary: os.Args[0], RunDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	if d == nil {
		t.Fatal("nil driver")
	}
}

func TestLogTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vmm.log")
	if got := logTail(path, 10); got != "" {
		t.Fatalf("missing file tail = %q", got)
	}
	content := strings.Repeat("ab", 100) + "TAIL"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got := logTail(path, 8)
	if got != content[len(content)-8:] {
		t.Fatalf("logTail = %q, want %q", got, content[len(content)-8:])
	}
}

func fakeSnapshot(t *testing.T) vmm.Snapshot {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "memory")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	snap := vmm.Snapshot{
		Dir:       dir,
		StatePath: filepath.Join(dir, "vmstate"),
		MemPath:   filepath.Join(dir, "mem"),
	}
	for _, p := range []string{snap.StatePath, snap.MemPath} {
		if err := os.WriteFile(p, []byte("snapshot"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return snap
}

func testRestoreConfig(t *testing.T, snap vmm.Snapshot) vmm.RestoreConfig {
	t.Helper()
	dir := t.TempDir()
	return vmm.RestoreConfig{
		SandboxID:   "sbx-test",
		Snapshot:    snap,
		VsockUDPath: filepath.Join(dir, "vsock.sock"),
		APISocket:   filepath.Join(dir, "api.sock"),
		SerialPath:  filepath.Join(dir, "serial.log"),
	}
}
