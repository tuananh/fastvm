// Package cloudhypervisor implements the fastvm VMM driver on top of Cloud
// Hypervisor's HTTP/JSON API over a Unix socket (docs/CLOUD_HYPERVISOR_MIGRATION.md).
//
// One Driver spawns Cloud Hypervisor VMM processes. Boot launches the VMM with
// all devices configured on the command line; Pause/Resume/Snapshot/Shutdown go
// through the REST API. Host directories are shared with virtio-fs: one
// virtiofsd process per volume is started before the VMM and restarted before a
// snapshot restore (virtiofsd exits when the VMM disconnects).
package cloudhypervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"fastvm/internal/types"
	"fastvm/internal/vmm"
)

const (
	// DefaultBinary is the primary Cloud Hypervisor executable name.
	DefaultBinary = "cloud-hypervisor"
	// AltBinary is the static release binary name.
	AltBinary = "cloud-hypervisor-static"
	// DefaultVirtiofsd is the virtiofsd executable name; distro packages also
	// install it as /usr/lib/virtiofsd.
	DefaultVirtiofsd = "virtiofsd"
	altVirtiofsd     = "/usr/lib/virtiofsd"

	// DefaultBootTimeout bounds process spawn + API readiness.
	DefaultBootTimeout = 20 * time.Second
	// DefaultStopTimeout bounds SIGTERM -> SIGKILL escalation.
	DefaultStopTimeout = 10 * time.Second
	// defaultRequestTimeout is the per-API-request timeout.
	defaultRequestTimeout = 10 * time.Second
	// fsConnectTimeout bounds waiting for a virtiofsd socket.
	fsConnectTimeout = 10 * time.Second

	defaultVCPUs    = 2
	defaultMemoryMB = 512
	defaultCmdline  = "console=ttyS0 root=/dev/vda rw init=/init"
)

// Options configures a Driver.
type Options struct {
	// Binary is the Cloud Hypervisor executable. Empty searches
	// "cloud-hypervisor" then "cloud-hypervisor-static" on PATH.
	Binary string
	// Virtiofsd is the virtiofsd executable. Empty searches "virtiofsd" then
	// /usr/lib/virtiofsd. It is only required when a sandbox has volumes.
	Virtiofsd string
	// RunDir is the per-sandbox runtime directory. When BootConfig/RestoreConfig
	// leave paths empty, the driver uses <RunDir>/api.sock, <RunDir>/serial.log
	// and <RunDir>/vmm.log; virtiofsd sockets/logs live there too.
	RunDir string
	// BootTimeout overrides DefaultBootTimeout.
	BootTimeout time.Duration
	// RequestTimeout overrides the per-request API timeout (default 10s).
	RequestTimeout time.Duration
	// StopTimeout overrides DefaultStopTimeout.
	StopTimeout time.Duration
}

// Driver launches and controls Cloud Hypervisor VMM processes.
type Driver struct {
	binary         string
	virtiofsd      string // preferred path/name; resolved lazily
	runDir         string
	bootTimeout    time.Duration
	requestTimeout time.Duration
	stopTimeout    time.Duration
}

// NewDriver resolves the Cloud Hypervisor executable and applies defaults.
func NewDriver(opts Options) (*Driver, error) {
	// The config default is "cloud-hypervisor"; fall back to the static
	// release binary when only that one is installed.
	preferredBinary := opts.Binary
	if preferredBinary == DefaultBinary {
		preferredBinary = ""
	}
	binary, err := resolveExecutable(preferredBinary, DefaultBinary, AltBinary)
	if err != nil {
		return nil, err
	}
	d := &Driver{
		binary:         binary,
		virtiofsd:      opts.Virtiofsd,
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

// resolveExecutable returns an executable path for preferred, falling back to
// candidate names on PATH (or absolute candidates).
func resolveExecutable(preferred string, candidates ...string) (string, error) {
	try := func(name string) (string, bool) {
		if name == "" {
			return "", false
		}
		if filepath.IsAbs(name) || strings.ContainsRune(name, filepath.Separator) {
			if st, err := os.Stat(name); err == nil && !st.IsDir() {
				return name, true
			}
			return "", false
		}
		p, err := exec.LookPath(name)
		return p, err == nil
	}
	if preferred != "" {
		if p, ok := try(preferred); ok {
			return p, nil
		}
		return "", fmt.Errorf("cloudhypervisor: executable %q not found", preferred)
	}
	for _, name := range candidates {
		if p, ok := try(name); ok {
			return p, nil
		}
	}
	return "", fmt.Errorf("cloudhypervisor: no executable found (tried %s)", strings.Join(candidates, ", "))
}

// resolveVirtiofsd resolves the virtiofsd binary on first use.
func (d *Driver) resolveVirtiofsd() (string, error) {
	preferred := d.virtiofsd
	// Config default is "virtiofsd"; distro packages also install
	// /usr/lib/virtiofsd, so fall through to it if the name is not on PATH.
	if preferred == DefaultVirtiofsd {
		preferred = ""
	}
	return resolveExecutable(preferred, DefaultVirtiofsd, altVirtiofsd)
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
		return vmPaths{}, errors.New("cloudhypervisor: no API socket path: set BootConfig/RestoreConfig APISocket or Options.RunDir")
	}
	if p.vmmLog == "" {
		p.vmmLog = filepath.Join(filepath.Dir(p.apiSock), "vmm.log")
	}
	return p, nil
}

func effectiveVCPUs(n int) int {
	if n <= 0 {
		return defaultVCPUs
	}
	return n
}

func effectiveMemoryMB(n int) int {
	if n <= 0 {
		return defaultMemoryMB
	}
	return n
}

func memoryParam(memMB int, withVolumes bool) string {
	p := fmt.Sprintf("size=%dM", memMB)
	if withVolumes {
		// virtio-fs requires shared guest memory (vhost-user).
		p += ",shared=on"
	}
	return p
}

// bootArgs builds the Cloud Hypervisor command line for a cold boot. fsSocks is
// aligned with cfg.Volumes; each entry is the virtiofsd socket for that volume.
func bootArgs(cfg vmm.BootConfig, paths vmPaths, fsSocks []string) []string {
	cmdline := cfg.Cmdline
	if cmdline == "" {
		cmdline = defaultCmdline
	}
	args := []string{
		"--api-socket", paths.apiSock,
		"--kernel", cfg.KernelPath,
		"--cmdline", cmdline,
		"--cpus", fmt.Sprintf("boot=%d", effectiveVCPUs(cfg.VCPUs)),
		"--memory", memoryParam(effectiveMemoryMB(cfg.MemoryMB), len(cfg.Volumes) > 0),
		"--disk", fmt.Sprintf("path=%s,image_type=raw", cfg.DiskPath),
		"--serial", "file=" + paths.serial,
		"--console", "off",
	}
	for _, path := range cfg.DataDisks {
		args = append(args, "--disk", fmt.Sprintf("path=%s,image_type=raw", path))
	}
	if cfg.TAPName != "" {
		args = append(args, "--net", fmt.Sprintf("tap=%s,mac=%s", cfg.TAPName, cfg.MAC))
	}
	if cfg.VsockUDPath != "" {
		cid := cfg.GuestCID
		if cid == 0 {
			cid = 3
		}
		args = append(args, "--vsock", fmt.Sprintf("cid=%d,socket=%s", cid, cfg.VsockUDPath))
	}
	for i, sock := range fsSocks {
		args = append(args, "--fs", fmt.Sprintf("tag=fastvm%d,socket=%s", i, sock))
	}
	return args
}

// Boot starts a cold microVM with all devices configured on the command line.
// It returns once the Cloud Hypervisor API socket is serving; the caller still
// waits for the guest agent to become ready.
func (d *Driver) Boot(ctx context.Context, cfg vmm.BootConfig) (vmm.VM, error) {
	if cfg.KernelPath == "" {
		return nil, errors.New("cloudhypervisor: BootConfig.KernelPath is required")
	}
	if cfg.DiskPath == "" {
		return nil, errors.New("cloudhypervisor: BootConfig.DiskPath is required")
	}
	if _, err := os.Stat(cfg.KernelPath); err != nil {
		return nil, fmt.Errorf("cloudhypervisor: kernel image %s: %w (check fastvmctl kernel ls/download/use)", cfg.KernelPath, err)
	}
	paths, err := d.resolvePaths(cfg.APISocket, cfg.SerialPath)
	if err != nil {
		return nil, err
	}
	fss, socks, err := d.startFSBackends(paths, cfg.Volumes)
	if err != nil {
		return nil, err
	}
	if err := prepareSockets(paths.apiSock, cfg.VsockUDPath); err != nil {
		stopFSBackends(fss)
		return nil, err
	}
	proc, err := startProcess(d.binary, bootArgs(cfg, paths, socks), paths.vmmLog)
	if err != nil {
		stopFSBackends(fss)
		return nil, err
	}
	v := &vm{
		driver: d,
		proc:   proc,
		paths:  paths,
		api:    newAPIClient(paths.apiSock, d.requestTimeout),
		fss:    fss,
	}
	if err := d.waitForAPI(ctx, v.proc, v.api); err != nil {
		return nil, v.fail(err)
	}
	return v, nil
}

// Restore starts the virtio-fs backends and a fresh Cloud Hypervisor process
// that loads the snapshot. The VM starts paused; cfg.Resume resumes it. The
// virtiofsd processes from the snapshot's VMM have exited, so they are
// re-spawned at the same socket paths before the restore.
func (d *Driver) Restore(ctx context.Context, cfg vmm.RestoreConfig) (vmm.VM, error) {
	if cfg.Snapshot.Dir == "" {
		return nil, errors.New("cloudhypervisor: RestoreConfig.Snapshot.Dir is required")
	}
	if st, err := os.Stat(cfg.Snapshot.Dir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("cloudhypervisor: snapshot dir %s: %w", cfg.Snapshot.Dir, err)
	}
	paths, err := d.resolvePaths(cfg.APISocket, cfg.SerialPath)
	if err != nil {
		return nil, err
	}
	fss, _, err := d.startFSBackends(paths, cfg.Volumes)
	if err != nil {
		return nil, err
	}
	if err := prepareSockets(paths.apiSock, cfg.VsockUDPath); err != nil {
		stopFSBackends(fss)
		return nil, err
	}
	args := []string{
		"--api-socket", paths.apiSock,
		"--restore", "source_url=file://" + cfg.Snapshot.Dir,
	}
	proc, err := startProcess(d.binary, args, paths.vmmLog)
	if err != nil {
		stopFSBackends(fss)
		return nil, err
	}
	v := &vm{
		driver: d,
		proc:   proc,
		paths:  paths,
		api:    newAPIClient(paths.apiSock, d.requestTimeout),
		fss:    fss,
		paused: !cfg.Resume,
	}
	if err := d.waitForAPI(ctx, v.proc, v.api); err != nil {
		return nil, v.fail(err)
	}
	if cfg.Resume {
		if err := v.api.resume(ctx); err != nil {
			return nil, v.fail(fmt.Errorf("cloudhypervisor: resume restored VM: %w", err))
		}
		v.paused = false
	}
	return v, nil
}

// waitForAPI polls GET /vm.info until the VMM is serving.
func (d *Driver) waitForAPI(ctx context.Context, proc *process, api *apiClient) error {
	deadline := time.Now().Add(d.bootTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	var lastErr error
	for {
		if proc.exited() {
			return fmt.Errorf("cloudhypervisor: VMM exited before the API became ready: %v", proc.waitErr())
		}
		pingCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		err := api.info(pingCtx)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return fmt.Errorf("cloudhypervisor: API socket %s not ready within %s: %w", api.sock, d.bootTimeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("cloudhypervisor: waiting for API socket: %w", ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func prepareSockets(apiSock, vsockUDSPath string) error {
	if dir := filepath.Dir(apiSock); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("cloudhypervisor: create API socket dir: %w", err)
		}
	}
	if err := os.Remove(apiSock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cloudhypervisor: remove stale API socket %s: %w", apiSock, err)
	}
	if vsockUDSPath != "" {
		if err := os.Remove(vsockUDSPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("cloudhypervisor: remove stale vsock socket %s: %w", vsockUDSPath, err)
		}
	}
	return nil
}

// fsBackend is one virtiofsd process sharing a host directory.
type fsBackend struct {
	socketPath string
	proc       *process
}

// startFSBackends starts one virtiofsd per volume and waits for its socket.
func (d *Driver) startFSBackends(paths vmPaths, vols []types.Volume) ([]*fsBackend, []string, error) {
	if len(vols) == 0 {
		return nil, nil, nil
	}
	virtiofsd, err := d.resolveVirtiofsd()
	if err != nil {
		return nil, nil, fmt.Errorf("cloudhypervisor: volumes require virtiofsd: %w", err)
	}
	runDir := filepath.Dir(paths.apiSock)
	var (
		backends []*fsBackend
		socks    []string
	)
	fail := func(err error) ([]*fsBackend, []string, error) {
		stopFSBackends(backends)
		return nil, nil, err
	}
	for i, vol := range vols {
		sock := filepath.Join(runDir, fmt.Sprintf("fs-%d.sock", i))
		logPath := filepath.Join(runDir, fmt.Sprintf("virtiofsd-%d.log", i))
		if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fail(fmt.Errorf("cloudhypervisor: remove stale virtiofsd socket: %w", err))
		}
		args := []string{
			"--shared-dir=" + vol.HostPath,
			"--socket-path=" + sock,
			"--sandbox=namespace",
			"--cache=never",
			"--log-level=warn",
		}
		if vol.ReadOnly {
			args = append(args, "--readonly")
		}
		proc, err := startProcess(virtiofsd, args, logPath)
		if err != nil {
			return fail(err)
		}
		backends = append(backends, &fsBackend{socketPath: sock, proc: proc})
		socks = append(socks, sock)
		if err := waitForSocket(sock, proc, fsConnectTimeout); err != nil {
			return fail(fmt.Errorf("cloudhypervisor: virtiofsd %s: %w", vol.HostPath, err))
		}
	}
	return backends, socks, nil
}

func waitForSocket(path string, proc *process, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if st, err := os.Stat(path); err == nil && st.Mode()&os.ModeSocket != 0 {
			return nil
		}
		if proc.exited() {
			return fmt.Errorf("virtiofsd exited before its socket was ready: %v", proc.waitErr())
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("socket %s not ready within %s", path, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func stopFSBackends(backends []*fsBackend) {
	for _, b := range backends {
		if b.proc != nil {
			_ = b.proc.stop(context.Background(), DefaultStopTimeout)
		}
	}
}

// vm is one Cloud Hypervisor process controlled through vmm.VM.
type vm struct {
	driver *Driver
	proc   *process
	paths  vmPaths
	api    *apiClient
	fss    []*fsBackend
	paused bool

	mu      sync.Mutex
	stopped bool
}

// Pause halts vCPUs while keeping memory and devices resident.
func (v *vm) Pause(ctx context.Context) error {
	if err := v.api.pause(ctx); err != nil {
		return fmt.Errorf("cloudhypervisor: pause: %w", err)
	}
	v.paused = true
	return nil
}

// Resume restarts vCPUs.
func (v *vm) Resume(ctx context.Context) error {
	if err := v.api.resume(ctx); err != nil {
		return fmt.Errorf("cloudhypervisor: resume: %w", err)
	}
	v.paused = false
	return nil
}

// Snapshot writes a paused memory+device-state snapshot into dir.
func (v *vm) Snapshot(ctx context.Context, dir string) (vmm.Snapshot, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return vmm.Snapshot{}, err
	}
	// CH refuses to overwrite an existing snapshot; the daemon reuses one
	// snapshot directory per sandbox, so clear the previous snapshot first.
	if entries, err := os.ReadDir(dir); err == nil {
		for _, entry := range entries {
			if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
				return vmm.Snapshot{}, fmt.Errorf("cloudhypervisor: clear old snapshot: %w", err)
			}
		}
	}
	if err := v.api.snapshot(ctx, dir); err != nil {
		return vmm.Snapshot{}, fmt.Errorf("cloudhypervisor: snapshot: %w", err)
	}
	return vmm.Snapshot{Dir: dir}, nil
}

// Stop shuts the VM down and tears down the virtio-fs backends.
func (v *vm) Stop(ctx context.Context) error {
	v.mu.Lock()
	if v.stopped {
		v.mu.Unlock()
		return nil
	}
	v.stopped = true
	v.mu.Unlock()

	// Best-effort guest shutdown, then terminate the process group.
	shutdownCtx, cancel := context.WithTimeout(ctx, v.driver.stopTimeout)
	_ = v.api.shutdown(shutdownCtx)
	cancel()
	err := v.proc.stop(ctx, v.driver.stopTimeout)
	stopFSBackends(v.fss)
	v.api.close()
	return err
}

// Wait blocks until the VMM process exits.
func (v *vm) Wait(ctx context.Context) error { return v.proc.wait(ctx) }

// Stats reads host-side CPU/RSS counters for the VMM process, preferring CH's
// per-block-device counters over /proc/<pid>/io for block I/O.
func (v *vm) Stats(ctx context.Context) (vmm.Stats, error) {
	st, err := readVMStats(v.proc.pid())
	if err != nil {
		return st, err
	}
	if counts, cerr := v.api.counters(ctx); cerr == nil {
		if read, write, found := blockCounters(counts); found {
			st.BlockReadByte = read
			st.BlockWriteByte = write
		}
	}
	return st, nil
}

// APISocket reports the Cloud Hypervisor API socket path.
func (v *vm) APISocket() string { return v.paths.apiSock }

// fail tears down the VMM + fs backends and appends the VMM log tail.
func (v *vm) fail(err error) error {
	stopCtx, cancel := context.WithTimeout(context.Background(), v.driver.stopTimeout+time.Second)
	defer cancel()
	_ = v.proc.stop(stopCtx, v.driver.stopTimeout)
	stopFSBackends(v.fss)
	v.api.close()
	if tail := logTail(v.paths.vmmLog, 4096); tail != "" {
		return fmt.Errorf("%w\n--- VMM log tail (%s) ---\n%s", err, v.paths.vmmLog, tail)
	}
	return err
}
