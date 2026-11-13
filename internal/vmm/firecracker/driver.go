// Package firecracker implements the fastvm VMM driver on top of Firecracker's
// HTTP/JSON API over a Unix socket (docs/DESIGN.md §6.2, §6.4, M2/M5).
//
// One Driver spawns Firecracker VMM processes. Boot configures and starts a
// cold VM; Restore loads a paused memory-file snapshot into a fresh process.
// Snapshot/restore keeps all host paths stable: the disk path recorded in the
// snapshot must still exist at the same path, and the vsock host UDS is
// re-attached through vsock_override.
package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"fastvm/internal/vmm"
)

const (
	// DefaultBinary is Firecracker's executable name looked up on PATH.
	DefaultBinary = "firecracker"

	// DefaultBootTimeout bounds process spawn + API readiness.
	DefaultBootTimeout = 15 * time.Second
	// DefaultStopTimeout bounds SIGTERM -> SIGKILL escalation.
	DefaultStopTimeout = 5 * time.Second

	// DefaultCmdline boots the injected rootfs with the agent as init.
	DefaultCmdline = "console=ttyS0 root=/dev/vda rw init=/init"

	// RootDriveID is the drive id used for the root volume.
	RootDriveID = "rootfs"
	// VsockID is the vsock device id used for the guest agent channel.
	VsockID = "vsock0"
	// NetIfaceID is the virtio-net interface id.
	NetIfaceID = "eth0"

	// MinGuestCID is Firecracker's lowest usable guest CID.
	MinGuestCID = 3

	defaultVCPUs    = 1
	defaultMemoryMB = 128
)

// Options configures a Driver.
type Options struct {
	// Binary is the Firecracker executable (default "firecracker", looked up on
	// PATH and resolved once).
	Binary string
	// RunDir is the per-sandbox runtime directory. When BootConfig/RestoreConfig
	// leave paths empty, the driver uses <RunDir>/api.sock, <RunDir>/serial.log
	// and <RunDir>/vmm.log.
	RunDir string
	// BootTimeout overrides DefaultBootTimeout.
	BootTimeout time.Duration
	// RequestTimeout overrides the per-request API timeout (default 10s).
	RequestTimeout time.Duration
	// StopTimeout overrides DefaultStopTimeout.
	StopTimeout time.Duration
}

// Driver launches and controls Firecracker VMM processes.
type Driver struct {
	binary         string
	runDir         string
	bootTimeout    time.Duration
	requestTimeout time.Duration
	stopTimeout    time.Duration

	// args are extra command-line arguments placed before --api-sock. Used by
	// tests to substitute a fake VMM for the real binary.
	args []string
}

// NewDriver returns a vmm.Driver backed by Firecracker. It fails if the
// Firecracker binary cannot be found.
func NewDriver(opts Options) (vmm.Driver, error) {
	binary := opts.Binary
	if binary == "" {
		binary = DefaultBinary
	}
	resolved, err := exec.LookPath(binary)
	if err != nil {
		return nil, fmt.Errorf("firecracker: binary %q: %w", binary, err)
	}
	d := &Driver{
		binary:         resolved,
		runDir:         opts.RunDir,
		bootTimeout:    opts.BootTimeout,
		requestTimeout: opts.RequestTimeout,
		stopTimeout:    opts.StopTimeout,
	}
	if d.bootTimeout <= 0 {
		d.bootTimeout = DefaultBootTimeout
	}
	if d.requestTimeout <= 0 {
		d.requestTimeout = defaultRequestTimeout
	}
	if d.stopTimeout <= 0 {
		d.stopTimeout = DefaultStopTimeout
	}
	return d, nil
}

// vmPaths are the host-side paths for one VM instance.
type vmPaths struct {
	apiSock string
	serial  string
	vmmLog  string
}

// resolvePaths fills in paths from the driver's RunDir and validates the API
// socket path.
func (d *Driver) resolvePaths(apiSock, serial string) (vmPaths, error) {
	p := vmPaths{apiSock: apiSock, serial: serial}
	if d.runDir != "" {
		if p.apiSock == "" {
			p.apiSock = filepath.Join(d.runDir, "api.sock")
		}
		if p.serial == "" {
			p.serial = filepath.Join(d.runDir, "serial.log")
		}
		p.vmmLog = filepath.Join(d.runDir, "vmm.log")
	}
	if p.apiSock == "" {
		return vmPaths{}, errors.New("firecracker: no API socket path: set BootConfig/RestoreConfig APISocket or Options.RunDir")
	}
	if p.vmmLog == "" {
		p.vmmLog = filepath.Join(filepath.Dir(p.apiSock), "vmm.log")
	}
	return p, nil
}

// Boot configures and starts a cold microVM. It returns only after
// InstanceStart succeeds and the VMM process is alive.
func (d *Driver) Boot(ctx context.Context, cfg vmm.BootConfig) (vmm.VM, error) {
	if cfg.KernelPath == "" {
		return nil, errors.New("firecracker: BootConfig.KernelPath is required")
	}
	if cfg.DiskPath == "" {
		return nil, errors.New("firecracker: BootConfig.DiskPath is required")
	}
	paths, err := d.resolvePaths(cfg.APISocket, cfg.SerialPath)
	if err != nil {
		return nil, err
	}
	cmdline := cfg.Cmdline
	if cmdline == "" {
		cmdline = DefaultCmdline
	}

	proc, err := d.spawn(paths.apiSock, paths.vmmLog, cfg.VsockUDPath)
	if err != nil {
		return nil, err
	}
	v := &vm{
		driver: d,
		proc:   proc,
		paths:  paths,
		api:    newAPIClient(paths.apiSock, d.requestTimeout),
	}
	if err := d.waitForAPI(ctx, proc, v.api); err != nil {
		return nil, v.fail(err)
	}
	if err := d.configure(ctx, v, cfg, cmdline); err != nil {
		return nil, v.fail(err)
	}
	return v, nil
}

// configStep is one ordered Firecracker configuration request.
type configStep struct {
	name string
	run  func() error
}

// configure issues the cold-boot configuration in Firecracker's required order:
// devices first, InstanceStart last.
func (d *Driver) configure(ctx context.Context, v *vm, cfg vmm.BootConfig, cmdline string) error {
	vcpus := cfg.VCPUs
	if vcpus <= 0 {
		vcpus = defaultVCPUs
	}
	memMB := cfg.MemoryMB
	if memMB <= 0 {
		memMB = defaultMemoryMB
	}

	steps := []configStep{
		{"boot-source", func() error { return v.api.bootSource(ctx, cfg.KernelPath, cmdline) }},
		{"root drive", func() error { return v.api.rootDrive(ctx, RootDriveID, cfg.DiskPath, false) }},
		{"machine-config", func() error { return v.api.machineConfig(ctx, vcpus, memMB) }},
	}
	if cfg.TAPName != "" {
		steps = append(steps, configStep{"network-interface", func() error {
			return v.api.networkInterface(ctx, NetIfaceID, cfg.TAPName, cfg.MAC)
		}})
	}
	if v.paths.serial != "" {
		steps = append(steps, configStep{"serial", func() error {
			return v.api.serial(ctx, v.paths.serial)
		}})
	}
	if cfg.VsockUDPath != "" {
		cid := cfg.GuestCID
		if cid == 0 {
			cid = MinGuestCID
		}
		if cid < MinGuestCID {
			return fmt.Errorf("firecracker: guest CID %d is below Firecracker's minimum %d", cid, MinGuestCID)
		}
		steps = append(steps, configStep{"vsock", func() error {
			return v.api.vsock(ctx, VsockID, cid, cfg.VsockUDPath)
		}})
	}
	steps = append(steps, configStep{"InstanceStart", func() error { return v.api.start(ctx) }})

	for _, step := range steps {
		if err := step.run(); err != nil {
			return fmt.Errorf("firecracker: %s: %w", step.name, err)
		}
	}
	return nil
}

// Restore spawns a fresh Firecracker process and loads a full paused snapshot
// with the File memory backend. The disk path embedded in the snapshot must
// still exist at the same path. When Resume is false the returned VM is paused
// (dormant) and the caller can Resume it later; when true vCPUs are started
// immediately.
func (d *Driver) Restore(ctx context.Context, cfg vmm.RestoreConfig) (vmm.VM, error) {
	if cfg.Snapshot.StatePath == "" || cfg.Snapshot.MemPath == "" {
		return nil, errors.New("firecracker: RestoreConfig.Snapshot requires StatePath and MemPath")
	}
	for _, p := range []string{cfg.Snapshot.StatePath, cfg.Snapshot.MemPath} {
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("firecracker: snapshot file %s: %w", p, err)
		}
	}
	paths, err := d.resolvePaths(cfg.APISocket, cfg.SerialPath)
	if err != nil {
		return nil, err
	}

	proc, err := d.spawn(paths.apiSock, paths.vmmLog, cfg.VsockUDPath)
	if err != nil {
		return nil, err
	}
	v := &vm{
		driver: d,
		proc:   proc,
		paths:  paths,
		api:    newAPIClient(paths.apiSock, d.requestTimeout),
		paused: !cfg.Resume,
	}
	if err := d.waitForAPI(ctx, proc, v.api); err != nil {
		return nil, v.fail(err)
	}

	req := snapshotLoadRequest{
		SnapshotPath: cfg.Snapshot.StatePath,
		MemBackend: memBackend{
			BackendType: "File",
			BackendPath: cfg.Snapshot.MemPath,
		},
		ResumeVM: cfg.Resume,
	}
	if cfg.VsockUDPath != "" {
		req.VsockOverride = &vsockOverride{VsockID: VsockID, UDSPath: cfg.VsockUDPath}
	}
	if cfg.TAPName != "" {
		req.NetworkOverrides = []networkOverride{{IfaceID: NetIfaceID, HostDevName: cfg.TAPName}}
	}
	if err := v.api.snapshotLoad(ctx, req); err != nil {
		return nil, v.fail(fmt.Errorf("firecracker: snapshot load: %w", err))
	}
	return v, nil
}

// waitForAPI polls the API socket until the VMM answers, the process exits, the
// boot timeout elapses, or ctx is done.
func (d *Driver) waitForAPI(ctx context.Context, proc *process, api *apiClient) error {
	timeout := d.bootTimeout
	deadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
		timeout = time.Until(deadline)
	}
	var lastErr error
	for {
		if proc.exited() {
			return fmt.Errorf("firecracker: VMM exited before the API became ready: %v", proc.waitErr())
		}
		pingCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		err := api.ping(pingCtx)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return fmt.Errorf("firecracker: API socket %s not ready within %s: %w", api.sock, timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("firecracker: waiting for API socket: %w", ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// vm is one Firecracker process controlled through vmm.VM.
type vm struct {
	driver *Driver
	proc   *process
	paths  vmPaths
	api    *apiClient

	mu      sync.Mutex
	paused  bool
	stopped bool
}

// Pause halts vCPUs. Repeated calls are no-ops.
func (v *vm) Pause(ctx context.Context) error {
	v.mu.Lock()
	paused := v.paused
	v.mu.Unlock()
	if paused {
		return nil
	}
	if err := v.api.setVMState(ctx, "Paused"); err != nil {
		return fmt.Errorf("firecracker: pause: %w", err)
	}
	v.mu.Lock()
	v.paused = true
	v.mu.Unlock()
	return nil
}

// Resume restarts vCPUs. Repeated calls are no-ops.
func (v *vm) Resume(ctx context.Context) error {
	v.mu.Lock()
	paused := v.paused
	stopped := v.stopped
	v.mu.Unlock()
	if stopped {
		return errors.New("firecracker: resume: VM is stopped")
	}
	if !paused {
		return nil
	}
	if err := v.api.setVMState(ctx, "Resumed"); err != nil {
		return fmt.Errorf("firecracker: resume: %w", err)
	}
	v.mu.Lock()
	v.paused = false
	v.mu.Unlock()
	return nil
}

// Snapshot pauses the VM if needed and writes a full memory+device snapshot
// into dir (vmstate + sparse mem file). It fsyncs the files and directory so a
// crash after return does not lose the snapshot; snapshot metadata/checksums
// and GC are the snapshot manager's job.
func (v *vm) Snapshot(ctx context.Context, dir string) (vmm.Snapshot, error) {
	if dir == "" {
		return vmm.Snapshot{}, errors.New("firecracker: Snapshot requires a directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return vmm.Snapshot{}, fmt.Errorf("firecracker: create snapshot dir: %w", err)
	}
	snap := vmm.Snapshot{
		Dir:       dir,
		MemPath:   filepath.Join(dir, "mem"),
		StatePath: filepath.Join(dir, "vmstate"),
	}
	if err := v.Pause(ctx); err != nil {
		return vmm.Snapshot{}, err
	}
	if err := v.api.snapshotCreate(ctx, snap.StatePath, snap.MemPath); err != nil {
		return vmm.Snapshot{}, fmt.Errorf("firecracker: snapshot create: %w", err)
	}
	if err := syncPath(snap.StatePath); err != nil {
		return vmm.Snapshot{}, err
	}
	if err := syncPath(snap.MemPath); err != nil {
		return vmm.Snapshot{}, err
	}
	if err := syncDir(dir); err != nil {
		return vmm.Snapshot{}, err
	}
	return snap, nil
}

// Stop terminates the VMM process and closes the API client.
func (v *vm) Stop(ctx context.Context) error {
	v.mu.Lock()
	if v.stopped {
		v.mu.Unlock()
		return nil
	}
	v.stopped = true
	v.mu.Unlock()
	err := v.proc.stop(ctx, v.driver.stopTimeout)
	v.api.close()
	return err
}

// Wait blocks until the VMM process exits.
func (v *vm) Wait(ctx context.Context) error { return v.proc.wait(ctx) }

// Stats returns host-side counters for the VMM process (see stats.go for
// limitations).
func (v *vm) Stats(ctx context.Context) (vmm.Stats, error) {
	if err := ctx.Err(); err != nil {
		return vmm.Stats{}, err
	}
	if v.proc.exited() {
		return vmm.Stats{}, errors.New("firecracker: stats: VMM process has exited")
	}
	st, err := readVMStats(v.proc.pid())
	if err != nil {
		return vmm.Stats{}, err
	}
	return st, nil
}

// APISocket reports the VMM API socket path.
func (v *vm) APISocket() string { return v.paths.apiSock }

// fail kills the VMM after a boot/restore failure and appends the VMM log tail
// so the caller sees Firecracker's own diagnostics.
func (v *vm) fail(err error) error {
	stopCtx, cancel := context.WithTimeout(context.Background(), v.driver.stopTimeout+time.Second)
	defer cancel()
	_ = v.proc.stop(stopCtx, v.driver.stopTimeout)
	v.api.close()
	if tail := logTail(v.paths.vmmLog, 4096); tail != "" {
		return fmt.Errorf("%w\n--- VMM log tail (%s) ---\n%s", err, v.paths.vmmLog, tail)
	}
	return err
}

func syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("firecracker: fsync %s: %w", path, err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("firecracker: fsync %s: %w", path, err)
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("firecracker: fsync dir %s: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("firecracker: fsync dir %s: %w", dir, err)
	}
	return nil
}
