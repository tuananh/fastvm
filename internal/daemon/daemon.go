// Package daemon implements the fastvm runtime: it wires the store, image
// builder, volume manager, network plane, VMM driver and guest agent into the
// lifecycle from docs/DESIGN.md §5.
package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"fastvm/internal/agent"
	"fastvm/internal/config"
	"fastvm/internal/image"
	"fastvm/internal/lifecycle"
	"fastvm/internal/manager"
	"fastvm/internal/network"
	"fastvm/internal/store"
	l2switch "fastvm/internal/switch"
	"fastvm/internal/types"
	"fastvm/internal/vmm"
	"fastvm/internal/volume"
)

// Deps are the daemon dependencies. Interfaces keep the daemon testable with
// fakes; cmd/fastvmd injects the real implementations.
type Deps struct {
	Config    *config.Config
	Store     store.Store
	Images    image.Builder
	Volumes   volume.Manager
	Network   network.Manager
	Switch    l2switch.Switch // optional; eBPF mode only
	VMM       vmm.Driver
	DialAgent func(ctx context.Context, vsockUDS string, guestCID uint32) (agent.Client, error)
	// IfIndex resolves a TAP name to an interface index (test seam; defaults to
	// net.InterfaceByName).
	IfIndex func(name string) (int, error)
	Log     *slog.Logger
	Now     func() time.Time
}

// Daemon implements manager.Runtime and manager.Images.
type Daemon struct {
	cfg     *config.Config
	st      store.Store
	img     image.Builder
	vol     volume.Manager
	net     network.Manager
	sw      l2switch.Switch
	vmmD    vmm.Driver
	dial    func(ctx context.Context, vsockUDS string, guestCID uint32) (agent.Client, error)
	ifIndex func(string) (int, error)
	log     *slog.Logger
	now     func() time.Time

	mu       sync.Mutex
	runtimes map[string]*runtime
	names    sync.Map // sandbox ID -> name, retained for event enrichment

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	wakeMu    sync.Mutex
	lastWake  map[string]time.Time
	wakeTimes map[string][]time.Time

	closeOnce sync.Once
}

var (
	_ manager.Runtime = (*Daemon)(nil)
	_ manager.Images  = (*Daemon)(nil)
)

// New validates dependencies and starts the wake-event consumer.
func New(deps Deps) (*Daemon, error) {
	if deps.Config == nil {
		return nil, errors.New("daemon: config required")
	}
	if deps.Store == nil || deps.Images == nil || deps.Volumes == nil ||
		deps.Network == nil || deps.VMM == nil || deps.DialAgent == nil {
		return nil, errors.New("daemon: all dependencies are required")
	}
	if err := deps.Config.Validate(); err != nil {
		return nil, err
	}
	switch deps.Config.Memory.OffloadMode {
	case "", "file", "never":
	default:
		return nil, fmt.Errorf("daemon: memory.offload_mode %q is not implemented (use file or never)", deps.Config.Memory.OffloadMode)
	}
	log := deps.Log
	if log == nil {
		log = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	ifIndex := deps.IfIndex
	if ifIndex == nil {
		ifIndex = func(name string) (int, error) {
			ifi, err := net.InterfaceByName(name)
			if err != nil {
				return 0, err
			}
			return ifi.Index, nil
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &Daemon{
		cfg: deps.Config, st: deps.Store, img: deps.Images, vol: deps.Volumes,
		net: deps.Network, sw: deps.Switch, vmmD: deps.VMM, dial: deps.DialAgent,
		ifIndex: ifIndex, log: log, now: now,
		runtimes: make(map[string]*runtime),
		ctx:      ctx, cancel: cancel,
		lastWake: make(map[string]time.Time), wakeTimes: make(map[string][]time.Time),
	}
	if d.sw != nil && d.sw.Mode() != l2switch.ModeBridge {
		if up, ok := d.net.(network.UplinkProvider); ok {
			p, perr := up.UplinkPort()
			if perr != nil {
				d.cancel()
				return nil, fmt.Errorf("switch uplink: %w", perr)
			}
			mac, merr := net.ParseMAC(p.MAC)
			if merr != nil {
				d.cancel()
				return nil, fmt.Errorf("switch uplink MAC %q: %w", p.MAC, merr)
			}
			if aerr := d.sw.AddPort(ctx, l2switch.Port{
				SandboxID: "uplink", IfIndex: p.IfIndex, MAC: mac, IsSandbox: false,
			}); aerr != nil {
				d.cancel()
				return nil, fmt.Errorf("switch uplink port: %w", aerr)
			}
		}
	}
	d.recoverStale()
	d.wg.Add(1)
	go d.wakeLoop()
	return d, nil
}

// runtime is one active sandbox (VMM + agent + timers).
type runtime struct {
	sb     types.Sandbox
	vm     vmm.VM
	ac     agent.Client
	cpu    *lifecycle.CPUWindow
	events chan agent.Event

	lastCPU      uint64
	lastBlock    uint64
	lastNetRx    uint64
	lastNetTx    uint64
	lastTick     time.Time
	execActive   int
	workload     []string
	workloadEnv  map[string]string
	guestBusy    bool
	warmupUntil  time.Time
	idleSince    time.Time
	pausedSince  time.Time
	snapshotting bool
	idleReason   string
	closed       bool
	done         chan struct{}
	stopOnce     sync.Once
	mu           sync.Mutex
}

func (rt *runtime) stop() {
	rt.stopOnce.Do(func() { close(rt.done) })
}

func (d *Daemon) newRuntime(id string) *runtime {
	cfg := lifecycle.IdleConfig{
		Window:          d.cfg.Lifecycle.IdleWindow.D(),
		SampleInterval:  d.cfg.Idle.SampleInterval.D(),
		ExitHold:        d.cfg.Idle.ExitHold.D(),
		Warmup:          d.cfg.Idle.Warmup.D(),
		CPUThresholdPct: d.cfg.Idle.CPUThresholdPct,
		CPUWakePct:      d.cfg.Idle.CPUWakePct,
		BlockIOBytes:    d.cfg.Idle.BlockIOBytes,
		KeepaliveAware:  d.cfg.Idle.KeepaliveAware,
	}
	return &runtime{cpu: lifecycle.NewCPUWindow(cfg), events: make(chan agent.Event, 32), done: make(chan struct{})}
}

// --- manager.Runtime -------------------------------------------------------

func (d *Daemon) Create(ctx context.Context, opts manager.CreateOptions) (types.Sandbox, error) {
	if strings.TrimSpace(opts.Image) == "" {
		return types.Sandbox{}, fmt.Errorf("%w: image is required", manager.ErrInvalid)
	}
	vcpus, mem := opts.VCPUs, opts.MemoryMB
	if vcpus == 0 {
		vcpus = d.cfg.Defaults.VCPUs
	}
	if mem == 0 {
		mem = d.cfg.Defaults.MemoryMB
	}
	if vcpus < 1 || mem < 64 {
		return types.Sandbox{}, fmt.Errorf("%w: vcpus>=1 and memory_mb>=64 required", manager.ErrInvalid)
	}
	base, err := d.img.Get(ctx, opts.Image)
	if err != nil {
		return types.Sandbox{}, fmt.Errorf("%w: image %q: %v", manager.ErrNotFound, opts.Image, err)
	}
	id, err := newID()
	if err != nil {
		return types.Sandbox{}, err
	}
	dir := filepath.Join(d.cfg.DataDir, "sandboxes", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return types.Sandbox{}, err
	}
	sb := types.Sandbox{
		ID: id, Name: opts.Name, BaseDisk: base.Digest, VCPUs: vcpus, MemoryMB: mem,
		State: types.StateCreating, NoAutoSleep: opts.NoAutoSleep, Ports: opts.Ports,
		VMMAPISock:  filepath.Join(dir, "vmm-api.sock"),
		VsockUDPath: filepath.Join(dir, "vsock.sock"),
		SerialPath:  filepath.Join(dir, "serial.log"),
		SnapshotDir: filepath.Join(dir, "memory"),
		CreatedAt:   d.now(), UpdatedAt: d.now(), LastActivityAt: d.now(),
	}
	if sb.Name == "" {
		sb.Name = "sb-" + id[:8]
	}
	d.names.Store(id, sb.Name)
	rt := d.newRuntime(id)
	rt.sb = sb
	rt.workload = append([]string(nil), opts.Cmd...)
	rt.workloadEnv = opts.Env
	rt.workload = append([]string(nil), opts.Cmd...)
	rt.workloadEnv = opts.Env

	vol, err := d.vol.Create(ctx, id, base, 0, d.cfg.Volume.COW) // 0 = use base disk size
	if err != nil {
		d.fail(ctx, rt, "volume: %v", err)
		return types.Sandbox{}, err
	}
	sb.VolumePath = vol.Path

	port, err := d.net.Create(ctx, network.CreateRequest{SandboxID: id, Ports: opts.Ports})
	if err != nil {
		_ = d.vol.Remove(context.Background(), id)
		d.fail(ctx, rt, "network: %v", err)
		return types.Sandbox{}, err
	}
	sb.NetnsPath, sb.VethHost, sb.VethIn = port.NetnsPath, port.VethHost, port.VethIn
	sb.TAPName, sb.MAC, sb.IP, sb.Gateway = port.TAPName, port.MAC, port.IP, port.Gateway
	if d.sw != nil {
		mac, merr := net.ParseMAC(sb.MAC)
		if merr != nil {
			_ = d.net.Delete(context.Background(), id)
			_ = d.vol.Remove(context.Background(), id)
			d.fail(ctx, rt, "switch: bad MAC %q: %v", sb.MAC, merr)
			return types.Sandbox{}, merr
		}
		ifidx := port.SwitchIfIndex
		if ifidx == 0 {
			var ierr error
			ifidx, ierr = d.ifIndex(sb.TAPName)
			if ierr != nil {
				_ = d.net.Delete(context.Background(), id)
				_ = d.vol.Remove(context.Background(), id)
				d.fail(ctx, rt, "switch: tap %q: %v", sb.TAPName, ierr)
				return types.Sandbox{}, ierr
			}
		}
		if aerr := d.sw.AddPort(ctx, l2switch.Port{
			SandboxID: id, IfIndex: ifidx, MAC: mac, IsSandbox: true,
		}); aerr != nil {
			_ = d.net.Delete(context.Background(), id)
			_ = d.vol.Remove(context.Background(), id)
			d.fail(ctx, rt, "switch: add port: %v", aerr)
			return types.Sandbox{}, aerr
		}
	}
	rt.mu.Lock()
	rt.sb = sb
	if err := d.persistLocked(ctx, rt, &sb); err != nil {
		rt.mu.Unlock()
		d.cleanupFailed(context.Background(), &sb)
		return types.Sandbox{}, err
	}
	rt.mu.Unlock()

	rt.mu.Lock()
	err = d.bootLocked(ctx, rt)
	sbOut := rt.sb
	rt.mu.Unlock()
	if err != nil {
		d.cleanupFailed(context.Background(), &sbOut)
		return types.Sandbox{}, err
	}

	d.mu.Lock()
	d.runtimes[id] = rt
	d.mu.Unlock()
	d.wg.Add(1)
	go d.monitor(rt)
	return sbOut, nil
}

func (d *Daemon) bootLocked(ctx context.Context, rt *runtime) error {
	sb := &rt.sb
	_ = d.persistLocked(ctx, rt, sb)
	vm, err := d.vmmD.Boot(ctx, vmm.BootConfig{
		SandboxID:   sb.ID,
		KernelPath:  d.cfg.KernelPath,
		DiskPath:    sb.VolumePath,
		VCPUs:       sb.VCPUs,
		MemoryMB:    sb.MemoryMB,
		TAPName:     sb.TAPName,
		MAC:         sb.MAC,
		GuestCID:    guestCID(sb.IP),
		VsockUDPath: sb.VsockUDPath,
		APISocket:   sb.VMMAPISock,
		SerialPath:  sb.SerialPath,
		Cmdline:     bootCmdline(sb, entropySeed(), d.guestBootFiles(sb)),
	})
	if err != nil {
		d.event(ctx, sb.ID, "error", fmt.Sprintf("vmm boot: %v", err))
		sb.State = types.StateFailed
		sb.Error = err.Error()
		return err
	}
	rt.vm = vm
	ac, err := d.dial(ctx, sb.VsockUDPath, guestCID(sb.IP))
	if err != nil {
		_ = vm.Stop(context.Background())
		d.event(ctx, sb.ID, "error", fmt.Sprintf("agent dial: %v", err))
		sb.State = types.StateFailed
		sb.Error = err.Error()
		return err
	}
	rt.ac = ac
	readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ac.WaitReady(readyCtx); err != nil {
		_ = vm.Stop(context.Background())
		d.event(ctx, sb.ID, "error", fmt.Sprintf("agent ready: %v", err))
		sb.State = types.StateFailed
		sb.Error = err.Error()
		return err
	}
	now := d.now()
	sb.State = types.StateRunning
	sb.LastActivityAt = now
	sb.WakeCount = 0
	rt.warmupUntil = now.Add(d.cfg.Idle.Warmup.D())
	rt.lastTick = now
	rt.lastCPU = 0
	rt.lastBlock = 0
	rt.lastNetRx, rt.lastNetTx = 0, 0
	d.event(ctx, sb.ID, "state", "sandbox running")
	if len(rt.workload) > 0 {
		_, werr := ac.Exec(ctx, agent.Request{
			ID: newReqID(), Op: agent.OpExec, Detach: true,
			Argv: rt.workload, Env: rt.workloadEnv,
		})
		if werr != nil {
			d.event(ctx, sb.ID, "error", "workload start: "+werr.Error())
		}
	}
	d.wg.Add(1)
	go func() { defer d.wg.Done(); d.agentEvents(rt) }()
	return d.persistLocked(ctx, rt, sb)
}

func (d *Daemon) Get(ctx context.Context, ref string) (types.Sandbox, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return types.Sandbox{}, fmt.Errorf("%w: empty sandbox reference", manager.ErrInvalid)
	}

	// Exact ID first.
	if sb, ok, err := d.st.GetSandbox(ctx, ref); err != nil {
		return types.Sandbox{}, err
	} else if ok {
		return *sb, nil
	}

	list, err := d.st.ListSandboxes(ctx)
	if err != nil {
		return types.Sandbox{}, err
	}

	// Exact name next (Docker-like: names win over ID-prefix matches).
	var byName []*types.Sandbox
	for _, cand := range list {
		if cand.Name == ref {
			byName = append(byName, cand)
		}
	}
	switch len(byName) {
	case 1:
		return *byName[0], nil
	case 0:
		// Fall through to ID-prefix resolution.
	default:
		return types.Sandbox{}, fmt.Errorf("%w: sandbox name %q is ambiguous (matches %s)",
			manager.ErrConflict, ref, joinSandboxIDs(byName))
	}

	// Docker-style unique ID prefix. Compare case-insensitively so pasted
	// uppercase IDs still work.
	prefix := strings.ToLower(ref)
	var byPrefix []*types.Sandbox
	for _, cand := range list {
		if strings.HasPrefix(strings.ToLower(cand.ID), prefix) {
			byPrefix = append(byPrefix, cand)
		}
	}
	switch len(byPrefix) {
	case 0:
		return types.Sandbox{}, fmt.Errorf("%w: sandbox %q", manager.ErrNotFound, ref)
	case 1:
		return *byPrefix[0], nil
	default:
		return types.Sandbox{}, fmt.Errorf("%w: sandbox ID prefix %q is ambiguous (matches %s)",
			manager.ErrConflict, ref, joinSandboxIDs(byPrefix))
	}
}

func joinSandboxIDs(list []*types.Sandbox) string {
	ids := make([]string, 0, len(list))
	for _, sb := range list {
		ids = append(ids, sb.ID)
	}
	return strings.Join(ids, ", ")
}

func (d *Daemon) List(ctx context.Context) ([]types.Sandbox, error) {
	list, err := d.st.ListSandboxes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]types.Sandbox, 0, len(list))
	for _, sb := range list {
		out = append(out, *sb)
	}
	return out, nil
}

func (d *Daemon) Exec(ctx context.Context, ref string, argv []string, env map[string]string) (agent.Response, error) {
	if len(argv) == 0 {
		return agent.Response{}, fmt.Errorf("%w: argv required", manager.ErrInvalid)
	}
	rt, err := d.runtimeFor(ctx, ref)
	if err != nil {
		return agent.Response{}, err
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	sb := &rt.sb
	if sb.State.Sleeping() {
		if err := d.wakeLocked(ctx, rt, sb); err != nil {
			return agent.Response{}, err
		}
	}
	if sb.State != types.StateRunning && sb.State != types.StateIdle {
		return agent.Response{}, fmt.Errorf("%w: sandbox is %s", manager.ErrConflict, sb.State)
	}
	ac, err := d.ensureAgentLocked(ctx, rt, sb)
	if err != nil {
		return agent.Response{}, err
	}
	rt.execActive++
	defer func() { rt.execActive-- }()
	d.touchLocked(rt, sb)
	req := agent.Request{ID: newReqID(), Op: agent.OpExec, Argv: argv, Env: env, TimeoutMS: 10 * 60 * 1000}
	resp, err := ac.Exec(ctx, req)
	if err != nil {
		return agent.Response{}, err
	}
	if err := d.persistLocked(ctx, rt, sb); err != nil {
		return resp, err
	}
	return resp, nil
}

func (d *Daemon) Sleep(ctx context.Context, ref string, deep bool) error {
	rt, err := d.runtimeFor(ctx, ref)
	if err != nil {
		return err
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	sb := &rt.sb
	if sb.State == types.StateDormant || sb.State == types.StatePaused {
		if deep && sb.State == types.StatePaused {
			return d.offloadLocked(ctx, rt, sb)
		}
		return nil
	}
	if sb.State != types.StateRunning && sb.State != types.StateIdle {
		return fmt.Errorf("%w: sandbox is %s", manager.ErrConflict, sb.State)
	}
	if err := d.pauseLocked(ctx, rt, sb); err != nil {
		return err
	}
	if deep {
		return d.offloadLocked(ctx, rt, sb)
	}
	return nil
}

func (d *Daemon) Wake(ctx context.Context, ref string) error {
	rt, err := d.runtimeFor(ctx, ref)
	if err != nil {
		return err
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return d.wakeLocked(ctx, rt, &rt.sb)
}

func (d *Daemon) Delete(ctx context.Context, ref string) error {
	rt, err := d.runtimeFor(ctx, ref)
	if err != nil {
		return err
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	sb := &rt.sb
	if rt.closed {
		return nil
	}
	rt.closed = true
	rt.stop()
	if rt.ac != nil {
		_ = rt.ac.Close()
	}
	if rt.vm != nil {
		_ = rt.vm.Stop(context.Background())
	}
	_ = d.net.Delete(context.Background(), sb.ID)
	if d.sw != nil {
		_ = d.sw.RemovePort(context.Background(), sb.ID)
	}
	if d.cfg.Volume.DiscardOnDestroy {
		_ = d.vol.Remove(context.Background(), sb.ID)
	}
	_ = os.RemoveAll(filepath.Join(d.cfg.DataDir, "sandboxes", sb.ID))
	if err := d.st.DeleteSandbox(ctx, sb.ID); err != nil {
		return err
	}
	d.mu.Lock()
	delete(d.runtimes, sb.ID)
	d.mu.Unlock()
	rt.stop()
	d.event(ctx, sb.ID, "deleted", "sandbox deleted")
	return nil
}

func (d *Daemon) Events(ctx context.Context, limit int) ([]types.Event, error) {
	events, err := d.st.ListEvents(ctx, limit)
	if err != nil {
		return nil, err
	}
	// Backfill events written before names were captured (or by older daemons).
	for i := range events {
		if events[i].SandboxName == "" {
			events[i].SandboxName = d.sandboxName(events[i].SandboxID)
		}
	}
	return events, nil
}

// snapshotForRestartLocked snapshots an active sandbox and leaves it Dormant so
// a following daemon start can adopt it. The runtime lock must be held.
func (d *Daemon) snapshotForRestartLocked(ctx context.Context, rt *runtime) {
	sb := &rt.sb
	if rt.vm == nil || d.cfg.Memory.OffloadMode == "never" {
		return
	}
	switch sb.State {
	case types.StateRunning, types.StateIdle:
		if err := d.pauseLocked(ctx, rt, sb); err != nil {
			d.log.Warn("shutdown pause failed; sandbox will not be recoverable", "sandbox", sb.ID, "err", err)
			return
		}
	case types.StatePaused:
		// already paused; snapshot below
	default:
		return
	}
	if err := d.offloadLocked(ctx, rt, sb); err != nil {
		d.log.Warn("shutdown snapshot failed; sandbox will not be recoverable", "sandbox", sb.ID, "err", err)
		return
	}
	d.log.Info("snapshotted sandbox for restart", "sandbox", sb.ID)
}

// Close stops all sandboxes and background work.
func (d *Daemon) Close() error {
	var err error
	d.closeOnce.Do(func() {
		d.cancel()
		d.mu.Lock()
		runtimes := make([]*runtime, 0, len(d.runtimes))
		for _, rt := range d.runtimes {
			runtimes = append(runtimes, rt)
		}
		d.mu.Unlock()
		for _, rt := range runtimes {
			rt.mu.Lock()
			// Graceful restart: snapshot Running/Idle/Paused sandboxes to Dormant
			// so the next daemon start can adopt them instead of marking them
			// Failed. Snapshotting is best-effort; a failure falls back to the
			// old behaviour (stop the VMM; recovery reports the sandbox failed).
			snapCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			d.snapshotForRestartLocked(snapCtx, rt)
			cancel()
			if rt.ac != nil {
				_ = rt.ac.Close()
			}
			if rt.vm != nil {
				_ = rt.vm.Stop(context.Background())
			}
			rt.closed = true
			rt.stop()
			id := rt.sb.ID
			rt.mu.Unlock()
			// Graceful shutdown: remove host-side network state for stopped VMs.
			// Dormant memory images and volumes stay on disk for the next start.
			_ = d.net.Delete(context.Background(), id)
			if d.sw != nil {
				_ = d.sw.RemovePort(context.Background(), id)
			}
		}
		err = d.net.Close()
		d.wg.Wait()
	})
	return err
}

// --- manager.Images --------------------------------------------------------

func (d *Daemon) BuildImage(ctx context.Context, spec manager.BuildOptions) (types.BaseDisk, error) {
	if spec.Tag == "" {
		return types.BaseDisk{}, fmt.Errorf("%w: tag required", manager.ErrInvalid)
	}
	ctxDir := spec.ContextDir
	if ctxDir == "" {
		ctxDir = "."
	}
	return d.img.Build(ctx, image.BuildSpec{
		Dockerfile: spec.Dockerfile, ContextDir: ctxDir, Tag: spec.Tag,
		SizeGB: spec.SizeGB, KernelPath: d.cfg.KernelPath,
		AgentPath: d.cfg.AgentPath, InitPath: d.cfg.InitPath,
	})
}

func (d *Daemon) ListImages(ctx context.Context) ([]types.BaseDisk, error) {
	return d.img.List(ctx)
}

func (d *Daemon) IngestRootfs(ctx context.Context, rootfsDir, tag string) (types.BaseDisk, error) {
	if tag == "" || rootfsDir == "" {
		return types.BaseDisk{}, fmt.Errorf("%w: rootfs_dir and tag required", manager.ErrInvalid)
	}
	return d.img.Ingest(ctx, image.IngestSpec{
		RootfsDir: rootfsDir, Tag: tag, SizeGB: 0,
		AgentPath: d.cfg.AgentPath, InitPath: d.cfg.InitPath,
	})
}

// RemoveImage deletes an image and its tags. By default it refuses while any
// sandbox record still references the base digest; force overrides that check.
func (d *Daemon) RemoveImage(ctx context.Context, ref string, force bool) (types.BaseDisk, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return types.BaseDisk{}, fmt.Errorf("%w: image reference required", manager.ErrInvalid)
	}
	disk, err := d.img.Get(ctx, ref)
	if err != nil {
		return types.BaseDisk{}, err
	}
	if !force {
		list, err := d.List(ctx)
		if err != nil {
			return types.BaseDisk{}, err
		}
		users := make([]string, 0)
		for _, sb := range list {
			if sb.BaseDisk == disk.Digest {
				users = append(users, sb.Name)
			}
		}
		if len(users) > 0 {
			return types.BaseDisk{}, fmt.Errorf("%w: image %s is used by sandbox(es) %s; delete them or use --force",
				manager.ErrConflict, disk.Identity(), strings.Join(users, ", "))
		}
	}
	return d.img.Remove(ctx, disk.Digest)
}

// --- lifecycle internals ---------------------------------------------------

func (d *Daemon) pauseLocked(ctx context.Context, rt *runtime, sb *types.Sandbox) error {
	if rt.snapshotting || rt.execActive > 0 {
		return fmt.Errorf("%w: sleep barrier active (exec or snapshot)", manager.ErrConflict)
	}
	if sb.State == types.StateRunning {
		if err := d.setStateLocked(ctx, rt, sb, types.StateIdle, "idle"); err != nil {
			return err
		}
	}
	// Arm wake notifications before halting vCPUs so no frame is missed.
	if err := d.net.SetAsleep(ctx, sb.ID, true); err != nil {
		d.log.Warn("set asleep failed", "sandbox", sb.ID, "err", err)
	}
	if d.sw != nil {
		if err := d.sw.SetAsleep(ctx, sb.ID, true); err != nil {
			d.log.Warn("switch set asleep failed", "sandbox", sb.ID, "err", err)
		}
	}
	if rt.vm != nil {
		if err := rt.vm.Pause(ctx); err != nil {
			_ = d.net.SetAsleep(ctx, sb.ID, false)
			return fmt.Errorf("pause: %w", err)
		}
	}
	rt.pausedSince = d.now()
	return d.setStateLocked(ctx, rt, sb, types.StatePaused, "paused: "+rt.idleReason)
}

func (d *Daemon) offloadLocked(ctx context.Context, rt *runtime, sb *types.Sandbox) error {
	if d.cfg.Memory.OffloadMode == "never" {
		return fmt.Errorf("%w: memory offload is disabled (memory.offload_mode=never)", manager.ErrConflict)
	}
	if sb.State != types.StatePaused {
		return fmt.Errorf("%w: offload requires Paused", manager.ErrConflict)
	}
	if rt.vm == nil {
		return fmt.Errorf("%w: no VMM to snapshot", manager.ErrConflict)
	}
	if err := os.MkdirAll(sb.SnapshotDir, 0o700); err != nil {
		return err
	}
	rt.snapshotting = true
	defer func() { rt.snapshotting = false }()
	snap, err := rt.vm.Snapshot(ctx, sb.SnapshotDir)
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	if err := rt.vm.Stop(ctx); err != nil {
		d.log.Warn("stop after snapshot failed", "sandbox", sb.ID, "err", err)
	}
	newVM, err := d.vmmD.Restore(ctx, vmm.RestoreConfig{
		SandboxID: sb.ID, Snapshot: snap,
		TAPName: sb.TAPName, VsockUDPath: sb.VsockUDPath,
		APISocket: sb.VMMAPISock, SerialPath: sb.SerialPath, Resume: false,
	})
	if err != nil {
		return fmt.Errorf("restore dormant: %w", err)
	}
	rt.vm = newVM
	rt.ac = nil // vsock resets across restore; re-dial on demand
	rt.pausedSince = d.now()
	return d.setStateLocked(ctx, rt, sb, types.StateDormant, "memory offloaded to disk: "+snap.Dir)
}

func (d *Daemon) wakeLocked(ctx context.Context, rt *runtime, sb *types.Sandbox) error {
	switch sb.State {
	case types.StateRunning, types.StateIdle:
		return nil
	case types.StatePaused:
		if rt.vm != nil {
			if err := rt.vm.Resume(ctx); err != nil {
				return fmt.Errorf("resume: %w", err)
			}
		}
	case types.StateDormant:
		if rt.vm == nil {
			// Daemon restart: restore the paused memory file now.
			snap := vmm.Snapshot{
				Dir:       sb.SnapshotDir,
				MemPath:   filepath.Join(sb.SnapshotDir, "mem"),
				StatePath: filepath.Join(sb.SnapshotDir, "vmstate"),
			}
			vm, err := d.vmmD.Restore(ctx, vmm.RestoreConfig{
				SandboxID: sb.ID, Snapshot: snap,
				TAPName: sb.TAPName, VsockUDPath: sb.VsockUDPath,
				APISocket: sb.VMMAPISock, SerialPath: sb.SerialPath, Resume: true,
			})
			if err != nil {
				return fmt.Errorf("restore: %w", err)
			}
			rt.vm = vm
		} else if err := rt.vm.Resume(ctx); err != nil {
			return fmt.Errorf("resume: %w", err)
		}
	default:
		return fmt.Errorf("%w: cannot wake %s", manager.ErrConflict, sb.State)
	}
	if err := d.net.SetAsleep(ctx, sb.ID, false); err != nil {
		d.log.Warn("clear asleep failed", "sandbox", sb.ID, "err", err)
	}
	if d.sw != nil {
		if err := d.sw.SetAsleep(ctx, sb.ID, false); err != nil {
			d.log.Warn("switch clear asleep failed", "sandbox", sb.ID, "err", err)
		}
	}
	now := d.now()
	rt.warmupUntil = now.Add(d.cfg.Idle.Warmup.D())
	rt.lastTick = now
	sb.LastActivityAt = now
	sb.WakeCount++
	if err := d.setStateLocked(ctx, rt, sb, types.StateRunning, "woken"); err != nil {
		return err
	}
	// Dormant wake needs a fresh agent connection (vsock reset on restore).
	if rt.ac == nil {
		ac, err := d.dial(ctx, sb.VsockUDPath, guestCID(sb.IP))
		if err == nil {
			rt.ac = ac
			go d.agentEvents(rt)
		} else {
			d.log.Warn("agent reconnect failed", "sandbox", sb.ID, "err", err)
		}
	}
	return nil
}

func (d *Daemon) setStateLocked(ctx context.Context, rt *runtime, sb *types.Sandbox, to types.State, msg string) error {
	if sb.State != to {
		if err := lifecycle.ValidateTransition(sb.State, to); err != nil {
			return err
		}
	}
	sb.State = to
	sb.UpdatedAt = d.now()
	if err := d.persistLocked(ctx, rt, sb); err != nil {
		return err
	}
	d.event(ctx, sb.ID, "state", fmt.Sprintf("%s: %s", to, msg))
	return nil
}

func (d *Daemon) persist(ctx context.Context, rt *runtime, sb *types.Sandbox) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return d.persistLocked(ctx, rt, sb)
}

func (d *Daemon) persistLocked(_ context.Context, _ *runtime, sb *types.Sandbox) error {
	copy := *sb
	// Store has its own context; the operation must succeed to remain durable.
	return d.st.PutSandbox(context.Background(), &copy)
}

func (d *Daemon) touchLocked(rt *runtime, sb *types.Sandbox) {
	sb.LastActivityAt = d.now()
	sb.UpdatedAt = sb.LastActivityAt
}

// --- idle monitor ----------------------------------------------------------

func (d *Daemon) monitor(rt *runtime) {
	defer d.wg.Done()
	ticker := time.NewTicker(d.cfg.Idle.SampleInterval.D())
	if d.cfg.Idle.SampleInterval.D() <= 0 {
		ticker.Reset(time.Second)
	}
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-rt.done:
			return
		case <-ticker.C:
			d.tick(rt)
		}
	}
}

func (d *Daemon) tick(rt *runtime) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.closed {
		return
	}
	sb := &rt.sb
	switch sb.State {
	case types.StateRunning, types.StateIdle:
		d.sampleLocked(rt, sb)
		d.evaluateLocked(rt, sb)
	case types.StatePaused:
		if d.cfg.Memory.OffloadMode != "never" && d.cfg.Lifecycle.OffloadAfter.D() > 0 && !rt.pausedSince.IsZero() &&
			d.now().Sub(rt.pausedSince) >= d.cfg.Lifecycle.OffloadAfter.D() {
			if err := d.offloadLocked(d.ctx, rt, sb); err != nil {
				d.log.Warn("auto offload failed", "sandbox", sb.ID, "err", err)
			}
		}
	case types.StateDormant, types.StateFailed:
		// nothing periodic
	}
	// Destroy after a long idle period.
	if d.cfg.Lifecycle.DestroyAfter.D() > 0 && !sb.LastActivityAt.IsZero() &&
		d.now().Sub(sb.LastActivityAt) >= d.cfg.Lifecycle.DestroyAfter.D() {
		if err := d.deleteLocked(rt, sb); err != nil {
			d.log.Warn("auto destroy failed", "sandbox", sb.ID, "err", err)
		}
	}
}

func (d *Daemon) sampleLocked(rt *runtime, sb *types.Sandbox) {
	now := d.now()
	elapsed := now.Sub(rt.lastTick)
	if elapsed <= 0 {
		elapsed = d.cfg.Idle.SampleInterval.D()
	}
	rt.lastTick = now
	if rt.vm != nil {
		stats, err := rt.vm.Stats(context.Background())
		if err == nil {
			if rt.lastCPU == 0 {
				rt.lastCPU = stats.VCPUTimeNS
			} else if stats.VCPUTimeNS > rt.lastCPU {
				delta := float64(stats.VCPUTimeNS - rt.lastCPU)
				pct := delta / float64(elapsed.Nanoseconds()*int64(sb.VCPUs)) * 100
				if pct < 0 || pct > 100 {
					pct = 0
				}
				rt.cpu.Add(now, pct)
				rt.lastCPU = stats.VCPUTimeNS
			}
			block := stats.BlockReadByte + stats.BlockWriteByte
			if block > rt.lastBlock {
				delta := block - rt.lastBlock
				rt.lastBlock = block
				if delta >= uint64(d.cfg.Idle.BlockIOBytes) {
					d.touchLocked(rt, sb)
				}
			}
		}
	}
	if st, err := d.net.Stats(context.Background(), sb.ID); err == nil {
		delta := (st.RxBytes - rt.lastNetRx) + (st.TxBytes - rt.lastNetTx)
		rt.lastNetRx, rt.lastNetTx = st.RxBytes, st.TxBytes
		if delta > 256 { // ignore pure ACKs/keepalives
			d.touchLocked(rt, sb)
		}
	}
}

func (d *Daemon) evaluateLocked(rt *runtime, sb *types.Sandbox) {
	now := d.now()
	cpuAvg := rt.cpu.Average(now)
	decision := lifecycle.EvaluateIdle(lifecycle.IdleConfig{
		Window:          d.cfg.Lifecycle.IdleWindow.D(),
		SampleInterval:  d.cfg.Idle.SampleInterval.D(),
		ExitHold:        d.cfg.Idle.ExitHold.D(),
		Warmup:          d.cfg.Idle.Warmup.D(),
		CPUThresholdPct: d.cfg.Idle.CPUThresholdPct,
		CPUWakePct:      d.cfg.Idle.CPUWakePct,
		BlockIOBytes:    d.cfg.Idle.BlockIOBytes,
	}, lifecycle.Observation{
		Now: now, LastActivityAt: sb.LastActivityAt, CPUAvgPct: cpuAvg,
		ExecActive: rt.execActive > 0, GuestBusy: rt.guestBusy,
		WarmupUntil: rt.warmupUntil, NoAutoSleep: sb.NoAutoSleep,
	})
	if sb.State == types.StateRunning {
		if decision.Idle {
			rt.idleSince = now
			rt.idleReason = decision.Reason
			if err := d.setStateLocked(d.ctx, rt, sb, types.StateIdle, decision.Reason); err != nil {
				d.log.Warn("enter idle failed", "sandbox", sb.ID, "err", err)
			}
		}
		return
	}
	// Idle: stay asleep-to-be unless something woke it.
	if decision.Idle {
		if d.cfg.Lifecycle.PauseAfter.D() > 0 && !rt.idleSince.IsZero() &&
			now.Sub(rt.idleSince) >= d.cfg.Lifecycle.PauseAfter.D() {
			if err := d.pauseLocked(d.ctx, rt, sb); err != nil {
				d.log.Warn("auto pause failed", "sandbox", sb.ID, "err", err)
			}
		}
		return
	}
	if rt.cpu.AboveFor(now, d.cfg.Idle.CPUWakePct, d.cfg.Idle.ExitHold.D()) {
		rt.idleReason = "cpu activity"
	} else {
		rt.idleReason = "activity"
	}
	if err := d.setStateLocked(d.ctx, rt, sb, types.StateRunning, rt.idleReason); err != nil {
		d.log.Warn("leave idle failed", "sandbox", sb.ID, "err", err)
	}
}

func (d *Daemon) deleteLocked(rt *runtime, sb *types.Sandbox) error {
	ctx := context.Background()
	rt.closed = true
	rt.stop()
	if rt.ac != nil {
		_ = rt.ac.Close()
	}
	if rt.vm != nil {
		_ = rt.vm.Stop(ctx)
	}
	_ = d.net.Delete(ctx, sb.ID)
	if d.sw != nil {
		_ = d.sw.RemovePort(ctx, sb.ID)
	}
	if d.cfg.Volume.DiscardOnDestroy {
		_ = d.vol.Remove(ctx, sb.ID)
	}
	_ = os.RemoveAll(filepath.Join(d.cfg.DataDir, "sandboxes", sb.ID))
	if err := d.st.DeleteSandbox(ctx, sb.ID); err != nil {
		return err
	}
	d.mu.Lock()
	delete(d.runtimes, sb.ID)
	d.mu.Unlock()
	d.event(ctx, sb.ID, "deleted", "destroy_after elapsed")
	return nil
}

// --- agent events ----------------------------------------------------------

func (d *Daemon) agentEvents(rt *runtime) {
	rt.mu.Lock()
	ac := rt.ac
	rt.mu.Unlock()
	if ac == nil {
		return
	}
	events := ac.Events()
	for {
		var ev agent.Event
		var ok bool
		select {
		case <-rt.done:
			return
		case <-d.ctx.Done():
			return
		case ev, ok = <-events:
			if !ok {
				return
			}
		}
		rt.mu.Lock()
		sb := &rt.sb
		switch ev.Type {
		case "workload_started":
			d.touchLocked(rt, sb)
		case "workload_exit":
			rt.guestBusy = false
			sb.WorkloadFinished = true
			d.touchLocked(rt, sb)
			policy, err := lifecycle.WorkloadExitTarget(d.cfg.Lifecycle.OnWorkloadExit)
			if err == nil && sb.State == types.StateRunning {
				rt.idleSince = d.now()
				if policy == types.StateIdle {
					_ = d.setStateLocked(d.ctx, rt, sb, types.StateIdle, "workload exited")
				}
			}
		case "workload_busy":
			rt.guestBusy = true
		case "touch":
			d.touchLocked(rt, sb)
		}
		rt.mu.Unlock()
	}
}

func (d *Daemon) ensureAgentLocked(ctx context.Context, rt *runtime, sb *types.Sandbox) (agent.Client, error) {
	if rt.ac != nil {
		return rt.ac, nil
	}
	ac, err := d.dial(ctx, sb.VsockUDPath, guestCID(sb.IP))
	if err != nil {
		return nil, fmt.Errorf("agent dial: %w", err)
	}
	rt.ac = ac
	go d.agentEvents(rt)
	return ac, nil
}

// --- wake loop -------------------------------------------------------------

func (d *Daemon) wakeLoop() {
	defer d.wg.Done()
	if d.sw != nil && d.sw.Mode() != l2switch.ModeBridge {
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			for {
				select {
				case <-d.ctx.Done():
					return
				case ev, ok := <-d.sw.WakeEvents():
					if !ok {
						return
					}
					d.handleWakeEvent(network.WakeEvent{SandboxID: ev.SandboxID, Time: ev.Time})
				}
			}
		}()
	}
	events := d.net.WakeEvents()
	for {
		select {
		case <-d.ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			d.handleWakeEvent(ev)
		}
	}
}

func (d *Daemon) handleWakeEvent(ev network.WakeEvent) {
	d.wakeMu.Lock()
	now := d.now()
	last := d.lastWake[ev.SandboxID]
	if !last.IsZero() && now.Sub(last) < d.cfg.Wake.CoalesceWindow.D() {
		d.wakeMu.Unlock()
		return
	}
	if max := d.cfg.Wake.MaxWakesPerMinute; max > 0 {
		cutoff := now.Add(-time.Minute)
		times := d.wakeTimes[ev.SandboxID]
		kept := times[:0]
		for _, t := range times {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		if len(kept) >= max {
			d.wakeMu.Unlock()
			d.log.Warn("wake rate limited", "sandbox", ev.SandboxID)
			return
		}
		d.wakeTimes[ev.SandboxID] = append(kept, now)
	}
	d.lastWake[ev.SandboxID] = now
	d.wakeMu.Unlock()
	if err := d.Wake(context.Background(), ev.SandboxID); err != nil {
		d.log.Warn("wake failed", "sandbox", ev.SandboxID, "err", err)
	}
}

// --- helpers ---------------------------------------------------------------

func (d *Daemon) runtimeFor(ctx context.Context, ref string) (*runtime, error) {
	sb, err := d.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	rt := d.runtimes[sb.ID]
	if rt == nil {
		rt = d.newRuntime(sb.ID)
		rt.sb = sb
		d.runtimes[sb.ID] = rt
	}
	d.mu.Unlock()
	rt.mu.Lock()
	if rt.sb.ID == "" {
		rt.sb = sb
	}
	rt.mu.Unlock()
	return rt, nil
}

func (d *Daemon) cleanupFailed(ctx context.Context, sb *types.Sandbox) {
	_ = d.net.Delete(ctx, sb.ID)
	if d.sw != nil {
		_ = d.sw.RemovePort(ctx, sb.ID)
	}
	_ = d.vol.Remove(ctx, sb.ID)
	if rt := d.lookupRuntime(sb.ID); rt != nil {
		rt.mu.Lock()
		if rt.vm != nil {
			_ = rt.vm.Stop(ctx)
		}
		if rt.ac != nil {
			_ = rt.ac.Close()
		}
		rt.closed = true
		rt.mu.Unlock()
	}
	_ = os.RemoveAll(filepath.Join(d.cfg.DataDir, "sandboxes", sb.ID))
}

func (d *Daemon) fail(ctx context.Context, rt *runtime, format string, args ...any) {
	sb := &rt.sb
	msg := fmt.Sprintf(format, args...)
	rt.mu.Lock()
	sb.State = types.StateFailed
	sb.Error = msg
	_ = d.persistLocked(ctx, rt, sb)
	rt.mu.Unlock()
	d.event(ctx, sb.ID, "error", msg)
}

func (d *Daemon) lookupRuntime(id string) *runtime {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.runtimes[id]
}

func (d *Daemon) event(_ context.Context, id, typ, msg string) {
	if id == "" {
		id = "daemon"
	}
	_ = d.st.AppendEvent(context.Background(), types.Event{
		Time: d.now(), SandboxID: id, SandboxName: d.sandboxName(id), Type: typ, Message: msg,
	})
}

// sandboxName resolves a sandbox name for event records. Names are cached so a
// "deleted" event still carries the name after the sandbox record is gone.
func (d *Daemon) sandboxName(id string) string {
	if id == "" || id == "daemon" {
		return ""
	}
	if v, ok := d.names.Load(id); ok {
		if name, _ := v.(string); name != "" {
			return name
		}
	}
	sb, ok, err := d.st.GetSandbox(context.Background(), id)
	if err != nil || !ok || sb == nil {
		return ""
	}
	d.names.Store(id, sb.Name)
	return sb.Name
}

func (d *Daemon) recoverStale() {
	list, err := d.st.ListSandboxes(context.Background())
	if err != nil {
		d.log.Warn("recover stale sandboxes", "err", err)
		return
	}
	for _, sb := range list {
		d.names.Store(sb.ID, sb.Name)
		switch sb.State {
		case types.StateDormant:
			// Recoverable: the memory file is on disk and no VMM is running.
			// Re-register the network attachment (persistent TAP + wake sniffer)
			// so the next inbound frame can wake it.
			port, err := d.net.Create(context.Background(), network.CreateRequest{
				SandboxID: sb.ID, Ports: sb.Ports, MAC: sb.MAC, IP: sb.IP,
			})
			if err != nil {
				d.log.Warn("dormant sandbox network adoption failed", "sandbox", sb.ID, "err", err)
			} else {
				sb.NetnsPath, sb.VethHost, sb.VethIn = port.NetnsPath, port.VethHost, port.VethIn
				sb.TAPName, sb.MAC, sb.IP, sb.Gateway = port.TAPName, port.MAC, port.IP, port.Gateway
				sb.UpdatedAt = d.now()
				_ = d.st.PutSandbox(context.Background(), sb)
				if err := d.addSwitchPort(context.Background(), sb.ID, port); err != nil {
					d.log.Warn("dormant sandbox switch adoption failed", "sandbox", sb.ID, "err", err)
				} else if d.sw != nil && d.sw.Mode() != l2switch.ModeBridge {
					// Re-arm the wake notification: a freshly loaded switch maps
					// every port as awake.
					if err := d.sw.SetAsleep(context.Background(), sb.ID, true); err != nil {
						d.log.Warn("dormant sandbox switch arm failed", "sandbox", sb.ID, "err", err)
					}
				}
				if err := d.net.SetAsleep(context.Background(), sb.ID, true); err != nil {
					d.log.Warn("dormant sandbox network arm failed", "sandbox", sb.ID, "err", err)
				}
			}
			d.log.Info("recovered dormant sandbox", "sandbox", sb.ID, "ip", sb.IP)
		case types.StateRunning, types.StateIdle, types.StatePaused, types.StateStarting,
			types.StateCreating, types.StateResuming:
			sb.State = types.StateFailed
			sb.Error = "daemon restarted; in-flight state was not recoverable"
			sb.UpdatedAt = d.now()
			_ = d.st.PutSandbox(context.Background(), sb)
			d.event(context.Background(), sb.ID, "error", sb.Error)
		}
	}
}

// addSwitchPort registers a sandbox's switch port after network adoption.
func (d *Daemon) addSwitchPort(ctx context.Context, id string, port network.Port) error {
	if d.sw == nil || d.sw.Mode() == l2switch.ModeBridge {
		return nil
	}
	ifidx := port.SwitchIfIndex
	if ifidx == 0 {
		var err error
		ifidx, err = d.ifIndex(port.TAPName)
		if err != nil {
			return err
		}
	}
	mac, err := net.ParseMAC(port.MAC)
	if err != nil {
		return err
	}
	return d.sw.AddPort(ctx, l2switch.Port{SandboxID: id, IfIndex: ifidx, MAC: mac, IsSandbox: true})
}

func newID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func newReqID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// entropySeed returns fresh random bytes for the guest CRNG seed, or "" if
// the host RNG failed.
func entropySeed() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

// bootCmdline builds the kernel cmdline, including the guest network config and
// a host-provided entropy seed. The guest agent reads fastvm.ip/fastvm.gw to
// configure eth0 before starting the workload, and fastvm.seed to credit the
// kernel random pool (otherwise getrandom can block in a fresh microVM).
func bootCmdline(sb *types.Sandbox, seed string, guest guestBootFiles) string {
	cmd := "console=ttyS0 root=/dev/vda rw init=/init"
	if sb.TAPName != "" && sb.IP != "" {
		cmd += " fastvm.ip=" + sb.IP + "/30"
		if sb.Gateway != "" {
			cmd += " fastvm.gw=" + sb.Gateway
		}
	}
	if seed != "" {
		cmd += " fastvm.seed=" + seed
	}
	if guest.Hostname != "" {
		cmd += " " + cmdlineGuestHostnameKey + "=" + guest.Hostname
	}
	if b64 := encodeCmdlineFile(guest.Resolv); b64 != "" {
		cmd += " " + cmdlineGuestResolvKey + "=" + b64
	}
	if b64 := encodeCmdlineFile(guest.Hosts); b64 != "" {
		cmd += " " + cmdlineGuestHostsKey + "=" + b64
	}
	return cmd
}

// guestCID derives a stable, unique-per-host CID from the sandbox IP.
func guestCID(ip string) uint32 {
	parsed := net.ParseIP(ip).To4()
	if parsed == nil {
		return 3
	}
	return 3 + uint32(parsed[2])<<8 + uint32(parsed[3])
}
