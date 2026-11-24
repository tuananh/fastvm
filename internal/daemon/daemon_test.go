package daemon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"fastvm/internal/agent"
	"fastvm/internal/config"
	"fastvm/internal/image"
	"fastvm/internal/manager"
	"fastvm/internal/network"
	"fastvm/internal/store"
	l2switch "fastvm/internal/switch"
	"fastvm/internal/types"
	"fastvm/internal/vmm"
	"fastvm/internal/volume"
)

// --- fakes ---------------------------------------------------------------

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type fakeImages struct {
	disk    types.BaseDisk
	getErr  error
	removed []string
	pulls   []image.PullSpec
}

func (f *fakeImages) Pull(_ context.Context, spec image.PullSpec) (types.BaseDisk, error) {
	f.pulls = append(f.pulls, spec)
	return f.disk, nil
}
func (f *fakeImages) Ingest(context.Context, image.IngestSpec) (types.BaseDisk, error) {
	return f.disk, nil
}
func (f *fakeImages) List(context.Context) ([]types.BaseDisk, error) {
	return []types.BaseDisk{f.disk}, nil
}
func (f *fakeImages) Get(context.Context, string) (types.BaseDisk, error) {
	return f.disk, f.getErr
}
func (f *fakeImages) Remove(_ context.Context, ref string) (types.BaseDisk, error) {
	f.removed = append(f.removed, ref)
	return f.disk, nil
}

type fakeVolumes struct {
	mu      sync.Mutex
	created []string
	removed []string
	err     error
}

func (f *fakeVolumes) Create(_ context.Context, sandboxID string, _ types.BaseDisk, _ int, _ string) (volume.Volume, error) {
	if f.err != nil {
		return volume.Volume{}, f.err
	}
	f.mu.Lock()
	f.created = append(f.created, sandboxID)
	f.mu.Unlock()
	return volume.Volume{SandboxID: sandboxID, Path: "/volumes/" + sandboxID + ".img"}, nil
}
func (f *fakeVolumes) Remove(_ context.Context, sandboxID string) error {
	f.mu.Lock()
	f.removed = append(f.removed, sandboxID)
	f.mu.Unlock()
	return nil
}
func (f *fakeVolumes) Get(context.Context, string) (volume.Volume, bool, error) {
	return volume.Volume{}, false, nil
}

type fakeNet struct {
	mu        sync.Mutex
	created   map[string]network.CreateRequest
	portSets  [][]types.PortMapping
	deleted   []string
	asleep    map[string]bool
	wakeCh    chan network.WakeEvent
	stats     types.NetStats
	createErr error
}

func newFakeNet() *fakeNet {
	return &fakeNet{created: map[string]network.CreateRequest{}, asleep: map[string]bool{}, wakeCh: make(chan network.WakeEvent, 8)}
}
func (f *fakeNet) Create(_ context.Context, req network.CreateRequest) (network.Port, error) {
	if f.createErr != nil {
		return network.Port{}, f.createErr
	}
	f.mu.Lock()
	f.created[req.SandboxID] = req
	f.mu.Unlock()
	n := len(f.created)
	return network.Port{
		SandboxID: req.SandboxID, TAPName: "tap" + string(rune('0'+n)), MAC: "02:00:00:00:00:01",
		IP: "10.77.0." + string(rune('1'+n)), Gateway: "10.77.0." + string(rune('0'+n)),
	}, nil
}
func (f *fakeNet) SetPorts(_ context.Context, id string, ports []types.PortMapping) error {
	f.mu.Lock()
	f.portSets = append(f.portSets, append([]types.PortMapping(nil), ports...))
	f.mu.Unlock()
	return nil
}
func (f *fakeNet) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	f.deleted = append(f.deleted, id)
	f.mu.Unlock()
	return nil
}
func (f *fakeNet) SetAsleep(_ context.Context, id string, asleep bool) error {
	f.mu.Lock()
	f.asleep[id] = asleep
	f.mu.Unlock()
	return nil
}
func (f *fakeNet) Stats(context.Context, string) (types.NetStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats, nil
}
func (f *fakeNet) WakeEvents() <-chan network.WakeEvent { return f.wakeCh }
func (f *fakeNet) Close() error                         { return nil }

type fakeVM struct {
	mu      sync.Mutex
	paused  bool
	resumed int
	stopped bool
	snapDir string
	stats   types.VMStats
	stopErr error
	snapErr error
}

func (v *fakeVM) Pause(context.Context) error {
	v.mu.Lock()
	v.paused = true
	v.mu.Unlock()
	return nil
}
func (v *fakeVM) Resume(context.Context) error {
	v.mu.Lock()
	v.paused = false
	v.resumed++
	v.mu.Unlock()
	return nil
}
func (v *fakeVM) Snapshot(_ context.Context, dir string) (vmm.Snapshot, error) {
	if v.snapErr != nil {
		return vmm.Snapshot{}, v.snapErr
	}
	v.mu.Lock()
	v.snapDir = dir
	v.mu.Unlock()
	return vmm.Snapshot{Dir: dir}, nil
}
func (v *fakeVM) Stop(context.Context) error {
	v.mu.Lock()
	v.stopped = true
	v.mu.Unlock()
	return v.stopErr
}
func (v *fakeVM) Wait(context.Context) error { return nil }
func (v *fakeVM) Stats(context.Context) (vmm.Stats, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.stats, nil
}
func (v *fakeVM) APISocket() string { return "/tmp/api.sock" }

type fakeDriver struct {
	mu         sync.Mutex
	boots      []vmm.BootConfig
	restores   []vmm.RestoreConfig
	vms        []*fakeVM
	bootErr    error
	restoreErr error
}

func (d *fakeDriver) Boot(_ context.Context, cfg vmm.BootConfig) (vmm.VM, error) {
	if d.bootErr != nil {
		return nil, d.bootErr
	}
	vm := &fakeVM{}
	d.mu.Lock()
	d.boots = append(d.boots, cfg)
	d.vms = append(d.vms, vm)
	d.mu.Unlock()
	return vm, nil
}
func (d *fakeDriver) Restore(_ context.Context, cfg vmm.RestoreConfig) (vmm.VM, error) {
	if d.restoreErr != nil {
		return nil, d.restoreErr
	}
	vm := &fakeVM{}
	d.mu.Lock()
	d.restores = append(d.restores, cfg)
	d.vms = append(d.vms, vm)
	d.mu.Unlock()
	return vm, nil
}
func (d *fakeDriver) lastVM() *fakeVM {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.vms) == 0 {
		return nil
	}
	return d.vms[len(d.vms)-1]
}
func (d *fakeDriver) lastBoot() (vmm.BootConfig, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.boots) == 0 {
		return vmm.BootConfig{}, false
	}
	return d.boots[len(d.boots)-1], true
}
func (d *fakeDriver) lastRestore() (vmm.RestoreConfig, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.restores) == 0 {
		return vmm.RestoreConfig{}, false
	}
	return d.restores[len(d.restores)-1], true
}

type fakeAgent struct {
	mu        sync.Mutex
	ready     bool
	envs      []map[string]string
	ports     []int
	memAvail  uint64
	memTotal  uint64
	files     map[string][]byte
	fileModes map[string]uint32
	execs     []agent.Request
	events    chan agent.Event
	closed    bool
	stdout    string
	exitErr   error
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{ready: true, events: make(chan agent.Event, 8)}
}
func (a *fakeAgent) WaitReady(context.Context) error { return nil }
func (a *fakeAgent) Exec(_ context.Context, req agent.Request) (agent.Response, error) {
	a.mu.Lock()
	a.execs = append(a.execs, req)
	a.mu.Unlock()
	if a.exitErr != nil {
		return agent.Response{}, a.exitErr
	}
	return agent.Response{ID: req.ID, Stdout: a.stdout}, nil
}
func (a *fakeAgent) FileStat(_ context.Context, path string) (int64, uint32, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	data, ok := a.files[path]
	if !ok {
		return 0, 0, os.ErrNotExist
	}
	return int64(len(data)), a.fileModes[path], nil
}
func (a *fakeAgent) FileRead(_ context.Context, path string, offset int64, max int) ([]byte, int64, uint32, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	data, ok := a.files[path]
	if !ok {
		return nil, 0, 0, os.ErrNotExist
	}
	if offset >= int64(len(data)) {
		return nil, int64(len(data)), a.fileModes[path], nil
	}
	end := offset + int64(max)
	if end > int64(len(data)) {
		end = int64(len(data))
	}
	return append([]byte(nil), data[offset:end]...), int64(len(data)), a.fileModes[path], nil
}
func (a *fakeAgent) FileWrite(_ context.Context, path string, offset int64, mode uint32, data []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.files == nil {
		a.files = make(map[string][]byte)
		a.fileModes = make(map[string]uint32)
	}
	if offset == 0 {
		a.files[path] = append([]byte(nil), data...)
	} else {
		cur := a.files[path]
		if int64(len(cur)) < offset {
			cur = append(cur, make([]byte, offset-int64(len(cur)))...)
		}
		cur = append(cur[:offset], data...)
		a.files[path] = cur
	}
	a.fileModes[path] = mode
	return nil
}
func (a *fakeAgent) Stats(context.Context) (agent.GuestStats, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	total := a.memTotal
	if total == 0 {
		total = 512 << 20
	}
	avail := a.memAvail
	if avail == 0 {
		avail = total / 2
	}
	return agent.GuestStats{MemoryTotalBytes: total, MemoryAvailableBytes: avail, MemoryUsedBytes: total - avail}, nil
}

func (a *fakeAgent) Ports(context.Context) ([]int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]int(nil), a.ports...), nil
}
func (a *fakeAgent) SetEnv(_ context.Context, env map[string]string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	copied := make(map[string]string, len(env))
	for k, v := range env {
		copied[k] = v
	}
	a.envs = append(a.envs, copied)
	return nil
}
func (a *fakeAgent) Touch(context.Context) error                      { return nil }
func (a *fakeAgent) SetNetwork(context.Context, string, string) error { return nil }
func (a *fakeAgent) SetHostname(context.Context, string) error        { return nil }
func (a *fakeAgent) Events() <-chan agent.Event                       { return a.events }
func (a *fakeAgent) Close() error                                     { a.mu.Lock(); a.closed = true; a.mu.Unlock(); return nil }
func (a *fakeAgent) execCount() int                                   { a.mu.Lock(); defer a.mu.Unlock(); return len(a.execs) }

type fakeDialer struct {
	mu     sync.Mutex
	agents []*fakeAgent
	err    error
}

func (d *fakeDialer) Dial(context.Context, string, uint32) (agent.Client, error) {
	if d.err != nil {
		return nil, d.err
	}
	a := newFakeAgent()
	d.mu.Lock()
	d.agents = append(d.agents, a)
	d.mu.Unlock()
	return a, nil
}
func (d *fakeDialer) last() *fakeAgent {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.agents) == 0 {
		return nil
	}
	return d.agents[len(d.agents)-1]
}
func (d *fakeDialer) count() int { d.mu.Lock(); defer d.mu.Unlock(); return len(d.agents) }

// --- harness -------------------------------------------------------------

type harness struct {
	d     *Daemon
	clock *fakeClock
	imgs  *fakeImages
	vols  *fakeVolumes
	net   *fakeNet
	drv   *fakeDriver
	dial  *fakeDialer
	st    store.Store
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessOpts(t, nil)
}

// newHarnessOpts builds the harness with an optional eBPF switch backend.
func newHarnessOpts(t *testing.T, sw l2switch.Switch) *harness {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.SocketPath = t.TempDir() + "/fastvm.sock"
	cfg.KernelPath = "/tmp/vmlinux"
	cfg.AgentPath = "/tmp/fastvm-agent"
	cfg.Lifecycle.IdleWindow = config.Duration(30 * time.Second)
	cfg.Lifecycle.PauseAfter = config.Duration(2 * time.Minute)
	cfg.Lifecycle.OffloadAfter = config.Duration(10 * time.Minute)
	cfg.Lifecycle.DestroyAfter = config.Duration(24 * time.Hour)
	cfg.Lifecycle.IdleDetection.SampleInterval = config.Duration(time.Second)
	cfg.Lifecycle.WakePolicy.CoalesceWindow = config.Duration(100 * time.Millisecond)

	st, err := store.Open(cfg.DataDir + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	h := &harness{
		clock: clock,
		imgs:  &fakeImages{disk: types.BaseDisk{Name: "img", Tag: "img:test", Digest: "sha256:abc", Path: "/base.img"}},
		vols:  &fakeVolumes{},
		net:   newFakeNet(),
		drv:   &fakeDriver{},
		dial:  &fakeDialer{},
		st:    st,
	}
	deps := Deps{
		Config: cfg, Store: st, Images: h.imgs, Volumes: h.vols, Network: h.net,
		VMM: h.drv, DialAgent: h.dial.Dial, Log: slog.New(slog.NewTextHandler(testWriter{t}, nil)),
		Now: clock.Now, Switch: sw,
	}
	if sw != nil {
		deps.IfIndex = func(string) (int, error) { return 42, nil }
	}
	d, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	h.d = d
	t.Cleanup(func() { _ = d.Close() })
	return h
}

// testWriter silences daemon logs unless the test fails.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func (h *harness) create(t *testing.T) types.Sandbox {
	t.Helper()
	sb, err := h.d.Create(context.Background(), manager.CreateOptions{
		Name: "web", Image: "img:test", Cmd: []string{"sleep", "1000"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return sb
}

func (h *harness) runtime(t *testing.T, id string) *runtime {
	t.Helper()
	h.d.mu.Lock()
	defer h.d.mu.Unlock()
	rt := h.d.runtimes[id]
	if rt == nil {
		t.Fatalf("runtime %s not found", id)
	}
	return rt
}

// --- tests ---------------------------------------------------------------

func TestCreateGeneratesHaikunatorName(t *testing.T) {
	h := newHarness(t)
	sb, err := h.d.Create(context.Background(), manager.CreateOptions{Image: "img:test"})
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[a-z]+-[a-z]+-[0-9]{4}$`).MatchString(sb.Name) {
		t.Fatalf("generated name = %q", sb.Name)
	}
	if sb.State != types.StateRunning {
		t.Fatalf("state = %s", sb.State)
	}
}

func TestCreateRunning(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t)
	if sb.State != types.StateRunning {
		t.Fatalf("state = %s", sb.State)
	}
	if sb.VolumePath == "" || sb.TAPName == "" || sb.IP == "" {
		t.Fatalf("missing allocations: %+v", sb)
	}
	if len(h.vols.created) != 1 || len(h.net.created) != 1 {
		t.Fatalf("volume/net create count = %d/%d", len(h.vols.created), len(h.net.created))
	}
	boot, ok := h.drv.lastBoot()
	if !ok || boot.KernelPath != "/tmp/vmlinux" || boot.DiskPath != sb.VolumePath {
		t.Fatalf("boot config = %+v", boot)
	}
	if boot.TAPName != sb.TAPName || boot.GuestCID == 0 {
		t.Fatalf("boot tap/cid = %q/%d", boot.TAPName, boot.GuestCID)
	}
	if h.dial.count() != 1 {
		t.Fatalf("agent dials = %d", h.dial.count())
	}
	if got := h.dial.last().execCount(); got != 1 {
		t.Fatalf("workload execs = %d, want 1 detached", got)
	}
	if !h.dial.last().execs[0].Detach {
		t.Fatal("workload exec must be detached")
	}
	stored, ok, err := h.st.GetSandbox(context.Background(), sb.ID)
	if err != nil || !ok || stored.State != types.StateRunning {
		t.Fatalf("store: ok=%v err=%v state=%+v", ok, err, stored)
	}
}

func TestCreateAndExecCarrySandboxEnv(t *testing.T) {
	h := newHarness(t)
	sb, err := h.d.Create(context.Background(), manager.CreateOptions{
		Name: "env", Image: "img:test", Env: map[string]string{"TOKEN": "secret"}, Workdir: "/workspace",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	a := h.dial.last()
	a.mu.Lock()
	if len(a.envs) != 1 || a.envs[0]["TOKEN"] != "secret" {
		t.Fatalf("agent envs = %v", a.envs)
	}
	a.mu.Unlock()

	if _, err := h.d.Exec(context.Background(), sb.ID, []string{"sh"}, nil); err != nil {
		t.Fatalf("exec: %v", err)
	}
	a.mu.Lock()
	last := a.execs[len(a.execs)-1]
	a.mu.Unlock()
	if last.Env["TOKEN"] != "secret" {
		t.Fatalf("exec env = %v, want TOKEN=secret", last.Env)
	}
	if sb.Workdir != "/workspace" {
		t.Fatalf("sandbox workdir = %q", sb.Workdir)
	}
	a.mu.Lock()
	if len(a.execs) == 0 || a.execs[0].Dir != "/workspace" {
		t.Fatalf("workload dir = %v", a.execs)
	}
	a.mu.Unlock()
}

func TestCreateImageNotFound(t *testing.T) {
	h := newHarness(t)
	h.imgs.getErr = errors.New("missing")
	_, err := h.d.Create(context.Background(), manager.CreateOptions{Image: "nope"})
	if !errors.Is(err, manager.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if len(h.vols.created) != 0 {
		t.Fatal("volume should not be created")
	}
}

func TestCreateNetworkFailureCleansUp(t *testing.T) {
	h := newHarness(t)
	h.net.createErr = errors.New("no netns")
	_, err := h.d.Create(context.Background(), manager.CreateOptions{Image: "img:test"})
	if err == nil {
		t.Fatal("expected error")
	}
	if len(h.vols.removed) != 1 {
		t.Fatalf("volume cleanup = %d", len(h.vols.removed))
	}
}

func TestExecWakesPausedSandbox(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t)
	if err := h.d.Sleep(context.Background(), sb.ID, false); err != nil {
		t.Fatal(err)
	}
	rt := h.runtime(t, sb.ID)
	rt.mu.Lock()
	if rt.sb.State != types.StatePaused {
		t.Fatalf("state = %s", rt.sb.State)
	}
	rt.mu.Unlock()
	if !h.net.asleep[sb.ID] {
		t.Fatal("port must be marked asleep")
	}
	h.dial.last().stdout = "hello\n"
	resp, err := h.d.Exec(context.Background(), sb.ID, []string{"echo", "hello"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Stdout != "hello\n" {
		t.Fatalf("stdout = %q", resp.Stdout)
	}
	if h.drv.lastVM().resumed != 1 {
		t.Fatalf("resumes = %d", h.drv.lastVM().resumed)
	}
	got, _ := h.d.Get(context.Background(), sb.ID)
	if got.State != types.StateRunning {
		t.Fatalf("state after exec = %s", got.State)
	}
	if h.net.asleep[sb.ID] {
		t.Fatal("port must be awake")
	}
}

func TestSleepDeepOffloadAndWake(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t)
	preVMM := h.drv.lastVM()
	if err := h.d.Sleep(context.Background(), sb.ID, true); err != nil {
		t.Fatal(err)
	}
	got, _ := h.d.Get(context.Background(), sb.ID)
	if got.State != types.StateDormant {
		t.Fatalf("state = %s, want Dormant", got.State)
	}
	restore, ok := h.drv.lastRestore()
	if !ok || restore.Resume {
		t.Fatalf("restore = %+v ok=%v (want paused restore)", restore, ok)
	}
	if preVMM == nil || !preVMM.stopped {
		t.Fatal("pre-snapshot VMM should be stopped")
	}
	if !h.net.asleep[sb.ID] {
		t.Fatal("port must be asleep while dormant")
	}
	wakeVM := h.drv.lastVM() // restored VM
	if err := h.d.Wake(context.Background(), sb.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = h.d.Get(context.Background(), sb.ID)
	if got.State != types.StateRunning || got.WakeCount != 1 {
		t.Fatalf("state/wakes = %s/%d", got.State, got.WakeCount)
	}
	if wakeVM.resumed != 1 {
		t.Fatalf("restored VM resumes = %d", wakeVM.resumed)
	}
	if h.dial.count() < 2 {
		t.Fatal("expected a fresh agent connection after dormant wake")
	}
}

func TestStatsFromSamplerCache(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t)
	ctx := context.Background()
	rt := h.runtime(t, sb.ID)
	vm := h.drv.lastVM()
	vm.mu.Lock()
	vm.stats = types.VMStats{
		VCPUTimeNS: 1_000_000, BlockReadByte: 100, BlockWriteByte: 200,
		MemoryRSSBytes: 104857600,
	}
	vm.mu.Unlock()

	// The first sample only establishes the CPU baseline.
	h.clock.Advance(time.Second)
	rt.mu.Lock()
	h.d.sampleLocked(rt, &rt.sb)
	rt.mu.Unlock()

	// Second sample: 65ms of CPU on 2 vCPUs over 1s = 3.25%.
	vm.mu.Lock()
	vm.stats.VCPUTimeNS += 65_000_000
	vm.mu.Unlock()
	h.net.mu.Lock()
	h.net.stats = types.NetStats{RxBytes: 1024, TxBytes: 2048}
	h.net.mu.Unlock()
	h.clock.Advance(time.Second)
	rt.mu.Lock()
	h.d.sampleLocked(rt, &rt.sb)
	rt.mu.Unlock()

	got, err := h.d.Stats(ctx, sb.Name)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if got.SandboxID != sb.ID || got.Name != "web" || got.State != types.StateRunning {
		t.Fatalf("identity = %+v", got)
	}
	if got.CPUPercent != 3.25 {
		t.Errorf("cpu_percent = %v, want 3.25", got.CPUPercent)
	}
	if got.VCPUs != 2 || got.MemoryMB != 512 {
		t.Errorf("vcpus/memory_mb = %d/%d", got.VCPUs, got.MemoryMB)
	}
	if got.MemoryRSSBytes != 104857600 {
		t.Errorf("memory_rss_bytes = %d", got.MemoryRSSBytes)
	}
	if got.BlockReadBytes != 100 || got.BlockWriteBytes != 200 {
		t.Errorf("block_read/write = %d/%d", got.BlockReadBytes, got.BlockWriteBytes)
	}
	if got.RxBytes != 1024 || got.TxBytes != 2048 {
		t.Errorf("rx/tx = %d/%d", got.RxBytes, got.TxBytes)
	}
	if got.UptimeSeconds != 2 {
		t.Errorf("uptime_seconds = %d, want 2", got.UptimeSeconds)
	}
	if got.WakeCount != 0 {
		t.Errorf("wake_count = %d", got.WakeCount)
	}
	if !got.CapturedAt.Equal(h.clock.Now()) {
		t.Errorf("captured_at = %v, want %v", got.CapturedAt, h.clock.Now())
	}
}

func TestStatsPausedReportsZeroCPU(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t)
	rt := h.runtime(t, sb.ID)
	rt.mu.Lock()
	rt.cpuPct = 42 // a stale sample from before the pause
	rt.mu.Unlock()
	if err := h.d.Sleep(context.Background(), sb.ID, false); err != nil {
		t.Fatal(err)
	}
	got, err := h.d.Stats(context.Background(), sb.ID)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if got.State != types.StatePaused {
		t.Fatalf("state = %s, want Paused", got.State)
	}
	if got.CPUPercent != 0 {
		t.Fatalf("paused cpu_percent = %v, want 0", got.CPUPercent)
	}
}

func TestStatsUnknownRef(t *testing.T) {
	h := newHarness(t)
	if _, err := h.d.Stats(context.Background(), "nope"); !errors.Is(err, manager.ErrNotFound) {
		t.Fatalf("Stats(unknown) error = %v, want ErrNotFound", err)
	}
}

func TestStatsWithoutRuntimeReturnsRecord(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	rec := &types.Sandbox{
		ID: "gone123", Name: "gone", State: types.StateFailed,
		VCPUs: 4, MemoryMB: 1024, WakeCount: 3,
		CreatedAt: h.clock.Now().Add(-time.Minute),
	}
	if err := h.st.PutSandbox(ctx, rec); err != nil {
		t.Fatal(err)
	}
	got, err := h.d.Stats(ctx, "gone")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if got.SandboxID != rec.ID || got.Name != "gone" || got.State != types.StateFailed {
		t.Fatalf("record fields = %+v", got)
	}
	if got.CPUPercent != 0 || got.MemoryRSSBytes != 0 || got.BlockReadBytes != 0 ||
		got.BlockWriteBytes != 0 || got.RxBytes != 0 || got.TxBytes != 0 {
		t.Fatalf("utilization should be zero without a runtime: %+v", got)
	}
	if got.UptimeSeconds != 60 {
		t.Errorf("uptime_seconds = %d, want 60", got.UptimeSeconds)
	}
	if got.VCPUs != 4 || got.MemoryMB != 1024 || got.WakeCount != 3 {
		t.Errorf("record bookkeeping = %+v", got)
	}
	if got.CapturedAt.IsZero() {
		t.Error("captured_at must be set even without a live sample")
	}
}

func TestIdlePredicateAutoPause(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t)
	rt := h.runtime(t, sb.ID)

	h.clock.Advance(31 * time.Second)
	h.d.tick(rt)
	rt.mu.Lock()
	state := rt.sb.State
	reason := rt.idleReason
	rt.mu.Unlock()
	if state != types.StateIdle {
		t.Fatalf("state = %s, want Idle", state)
	}
	if !strings.Contains(reason, "quiet") {
		t.Fatalf("reason = %q", reason)
	}

	h.clock.Advance(2*time.Minute + time.Second)
	h.d.tick(rt)
	rt.mu.Lock()
	state = rt.sb.State
	rt.mu.Unlock()
	if state != types.StatePaused {
		t.Fatalf("state = %s, want Paused", state)
	}
	if !h.drv.lastVM().paused {
		t.Fatal("VMM should be paused")
	}
}

func TestWorkloadExitMovesToIdle(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t)
	ac := h.dial.last()
	ac.events <- agent.Event{Type: "workload_exit", Code: 0}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := h.d.Get(context.Background(), sb.ID)
		if got.State == types.StateIdle {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("sandbox did not move to Idle after workload exit")
}

func TestWakeEventCoalescing(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t)
	if err := h.d.Sleep(context.Background(), sb.ID, false); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(time.Second)
	h.d.handleWakeEvent(network.WakeEvent{SandboxID: sb.ID, Time: h.clock.Now()})
	got, _ := h.d.Get(context.Background(), sb.ID)
	if got.State != types.StateRunning || got.WakeCount != 1 {
		t.Fatalf("after first wake: %s/%d", got.State, got.WakeCount)
	}
	// Immediately after: coalesced, no extra wake.
	if err := h.d.Sleep(context.Background(), sb.ID, false); err != nil {
		t.Fatal(err)
	}
	h.d.handleWakeEvent(network.WakeEvent{SandboxID: sb.ID, Time: h.clock.Now()})
	got, _ = h.d.Get(context.Background(), sb.ID)
	if got.State != types.StatePaused {
		t.Fatalf("coalesced wake should not resume: %s", got.State)
	}
}

func TestDeleteRemovesResources(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t)
	if err := h.d.Delete(context.Background(), sb.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := h.st.GetSandbox(context.Background(), sb.ID); ok {
		t.Fatal("sandbox record not deleted")
	}
	if len(h.vols.removed) != 1 || len(h.net.deleted) != 1 {
		t.Fatalf("cleanup volume/net = %d/%d", len(h.vols.removed), len(h.net.deleted))
	}
	if !h.drv.lastVM().stopped {
		t.Fatal("VMM not stopped")
	}
	if err := h.d.Delete(context.Background(), sb.ID); !errors.Is(err, manager.ErrNotFound) {
		t.Fatalf("second delete err = %v", err)
	}
}

func TestRecoverStaleStates(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	_ = st.PutSandbox(context.Background(), &types.Sandbox{ID: "running", State: types.StateRunning})
	_ = st.PutSandbox(context.Background(), &types.Sandbox{ID: "dormant", State: types.StateDormant})
	cfg := config.Default()
	cfg.DataDir = dir
	d, err := New(Deps{
		Config: cfg, Store: st, Images: &fakeImages{}, Volumes: &fakeVolumes{},
		Network: newFakeNet(), VMM: &fakeDriver{}, DialAgent: (&fakeDialer{}).Dial,
		Log: slog.Default(), Now: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got, _ := d.Get(context.Background(), "running")
	if got.State != types.StateFailed {
		t.Fatalf("running after restart = %s, want Failed", got.State)
	}
	got, _ = d.Get(context.Background(), "dormant")
	if got.State != types.StateDormant {
		t.Fatalf("dormant after restart = %s, want Dormant", got.State)
	}
}

// fakeSwitch is a deterministic l2switch.Switch for daemon wiring tests.
type fakeSwitch struct {
	mu      sync.Mutex
	mode    l2switch.Mode
	added   []l2switch.Port
	removed []string
	asleep  map[string]bool
	wake    chan l2switch.WakeEvent
}

func newFakeSwitch() *fakeSwitch {
	return &fakeSwitch{mode: l2switch.ModeEBPF, asleep: map[string]bool{}, wake: make(chan l2switch.WakeEvent, 4)}
}
func (f *fakeSwitch) Mode() l2switch.Mode { return f.mode }
func (f *fakeSwitch) AddPort(_ context.Context, p l2switch.Port) error {
	f.mu.Lock()
	f.added = append(f.added, p)
	f.mu.Unlock()
	return nil
}
func (f *fakeSwitch) RemovePort(_ context.Context, id string) error {
	f.mu.Lock()
	f.removed = append(f.removed, id)
	f.mu.Unlock()
	return nil
}
func (f *fakeSwitch) SetAsleep(_ context.Context, id string, asleep bool) error {
	f.mu.Lock()
	f.asleep[id] = asleep
	f.mu.Unlock()
	return nil
}
func (f *fakeSwitch) Stats(context.Context, string) (l2switch.PortStats, error) {
	return l2switch.PortStats{}, nil
}
func (f *fakeSwitch) WakeEvents() <-chan l2switch.WakeEvent { return f.wake }
func (f *fakeSwitch) Close() error                          { return nil }

func TestSwitchWiring(t *testing.T) {
	sw := newFakeSwitch()
	h := newHarnessOpts(t, sw)
	sb := h.create(t)
	sw.mu.Lock()
	if len(sw.added) != 1 || sw.added[0].IfIndex != 42 || sw.added[0].SandboxID != sb.ID {
		sw.mu.Unlock()
		t.Fatalf("switch ports = %+v", sw.added)
	}
	sw.mu.Unlock()

	if err := h.d.Sleep(context.Background(), sb.ID, false); err != nil {
		t.Fatal(err)
	}
	sw.mu.Lock()
	if !sw.asleep[sb.ID] {
		sw.mu.Unlock()
		t.Fatal("switch port not marked asleep")
	}
	sw.mu.Unlock()

	sw.wake <- l2switch.WakeEvent{SandboxID: sb.ID, IfIndex: 42, Time: h.clock.Now()}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := h.d.Get(context.Background(), sb.ID)
		if got.State == types.StateRunning && got.WakeCount == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	got, _ := h.d.Get(context.Background(), sb.ID)
	if got.State != types.StateRunning || got.WakeCount != 1 {
		t.Fatalf("switch wake: state=%s wakes=%d", got.State, got.WakeCount)
	}
	sw.mu.Lock()
	awake, ok := sw.asleep[sb.ID]
	sw.mu.Unlock()
	if ok && awake {
		t.Fatal("switch port still asleep after wake")
	}

	if err := h.d.Delete(context.Background(), sb.ID); err != nil {
		t.Fatal(err)
	}
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if len(sw.removed) != 1 || sw.removed[0] != sb.ID {
		t.Fatalf("removed ports = %v", sw.removed)
	}
}

func TestCloseSnapshotsActiveSandboxForRestart(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t)

	if err := h.d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, ok, err := h.st.GetSandbox(context.Background(), sb.ID)
	if err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
	if !ok {
		t.Fatal("sandbox record disappeared")
	}
	if got.State != types.StateDormant {
		t.Fatalf("state after graceful close = %s, want Dormant", got.State)
	}
	restore, ok := h.drv.lastRestore()
	if !ok {
		t.Fatal("no restore was performed during shutdown snapshot")
	}
	if restore.Resume {
		t.Error("shutdown restore should be paused (Resume=false)")
	}
}

func TestBootCmdlineIncludesEntropySeed(t *testing.T) {
	sb := types.Sandbox{TAPName: "tap0", IP: "10.77.0.2", Gateway: "10.77.0.1"}
	cmd := bootCmdline(&sb, "deadbeef", guestBootFiles{})
	if !strings.Contains(cmd, " fastvm.seed=deadbeef") {
		t.Fatalf("cmdline missing entropy seed: %q", cmd)
	}
	if !strings.Contains(cmd, " fastvm.ip=10.77.0.2/30") || !strings.Contains(cmd, " fastvm.gw=10.77.0.1") {
		t.Fatalf("cmdline missing network config: %q", cmd)
	}
}

func TestEventsCarrySandboxName(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t)
	ctx := context.Background()

	events, err := h.d.Events(ctx, 100)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	foundState := false
	for _, ev := range events {
		if ev.SandboxID == sb.ID && ev.Type == "state" {
			if ev.SandboxName != "web" {
				t.Errorf("state event name = %q, want web", ev.SandboxName)
			}
			foundState = true
		}
	}
	if !foundState {
		t.Fatal("no state event for sandbox")
	}

	if err := h.d.Delete(ctx, sb.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	events, err = h.d.Events(ctx, 100)
	if err != nil {
		t.Fatalf("events after delete: %v", err)
	}
	foundDeleted := false
	for _, ev := range events {
		if ev.SandboxID == sb.ID && ev.Type == "deleted" {
			if ev.SandboxName != "web" {
				t.Errorf("deleted event name = %q, want web", ev.SandboxName)
			}
			foundDeleted = true
		}
	}
	if !foundDeleted {
		t.Fatal("no deleted event for sandbox")
	}
}

func TestGetResolvesNameAndUniqueIDPrefix(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for _, sb := range []*types.Sandbox{
		{ID: "abc11111111111111111", Name: "alpha"},
		{ID: "abc22222222222222222", Name: "beta"},
		{ID: "def11111111111111111", Name: "dup"},
		{ID: "def22222222222222222", Name: "dup"},
		{ID: "fed11111111111111111", Name: "alpine"},
	} {
		if err := h.st.PutSandbox(ctx, sb); err != nil {
			t.Fatalf("seed %s: %v", sb.ID, err)
		}
	}

	for _, tc := range []struct {
		ref, wantID string
	}{
		{"abc11111111111111111", "abc11111111111111111"}, // full ID
		{"beta", "abc22222222222222222"},                 // exact name
		{"abc1", "abc11111111111111111"},                 // unique ID prefix
		{"ABC1", "abc11111111111111111"},                 // case-insensitive ID prefix
		{"DEF22222222222222222", "def22222222222222222"}, // uppercase full ID
		{"alph", "abc11111111111111111"},                 // unique name prefix
		{"bet", "abc22222222222222222"},                  // unique name prefix
	} {
		got, err := h.d.Get(ctx, tc.ref)
		if err != nil {
			t.Errorf("Get(%q): %v", tc.ref, err)
			continue
		}
		if got.ID != tc.wantID {
			t.Errorf("Get(%q) = %s, want %s", tc.ref, got.ID, tc.wantID)
		}
	}

	for _, ref := range []string{"abc", "def"} {
		if _, err := h.d.Get(ctx, ref); !errors.Is(err, manager.ErrConflict) {
			t.Errorf("Get(%q) error = %v, want ErrConflict", ref, err)
		}
	}
	if _, err := h.d.Get(ctx, "dup"); !errors.Is(err, manager.ErrConflict) {
		t.Errorf("duplicate name error = %v, want ErrConflict", err)
	}
	if _, err := h.d.Get(ctx, "alp"); !errors.Is(err, manager.ErrConflict) {
		t.Errorf("ambiguous name prefix error = %v, want ErrConflict", err)
	}
	if _, err := h.d.Get(ctx, "   "); !errors.Is(err, manager.ErrInvalid) {
		t.Errorf("empty ref error = %v, want ErrInvalid", err)
	}
}

func TestRemoveImageChecksSandboxReferences(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t)
	ctx := context.Background()

	if _, err := h.d.RemoveImage(ctx, "img:test", false); !errors.Is(err, manager.ErrConflict) {
		t.Fatalf("RemoveImage in use error = %v, want ErrConflict", err)
	}
	if len(h.imgs.removed) != 0 {
		t.Fatalf("image removed despite conflict: %v", h.imgs.removed)
	}
	disk, err := h.d.RemoveImage(ctx, "img:test", true)
	if err != nil {
		t.Fatalf("RemoveImage force: %v", err)
	}
	if disk.Digest != sb.BaseDisk {
		t.Errorf("removed digest = %s, want %s", disk.Digest, sb.BaseDisk)
	}
	if len(h.imgs.removed) != 1 || h.imgs.removed[0] != "sha256:abc" {
		t.Fatalf("fake image removals = %v, want sha256:abc", h.imgs.removed)
	}
}

func TestAutoPublishPorts(t *testing.T) {
	h := newHarness(t)
	sb, err := h.d.Create(context.Background(), manager.CreateOptions{
		Name: "auto", Image: "img:test", AutoPublish: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	a := h.dial.last()
	a.mu.Lock()
	a.ports = []int{80, 18080}
	a.mu.Unlock()

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		h.net.mu.Lock()
		sets := len(h.net.portSets)
		var got []types.PortMapping
		if sets > 0 {
			got = append(got, h.net.portSets[sets-1]...)
		}
		h.net.mu.Unlock()
		for _, p := range got {
			if p.GuestPort == 80 {
				t.Fatalf("privileged port was auto-published: %+v", p)
			}
		}
		if len(got) == 1 && got[0].GuestPort == 18080 && got[0].Proto == "tcp" && got[0].HostPort == 18080 && got[0].LoopbackOnly {
			events, err := h.d.Events(context.Background(), 20)
			if err != nil {
				t.Fatalf("events: %v", err)
			}
			mapped := false
			for _, ev := range events {
				if ev.SandboxID == sb.ID && ev.Type == "port" && strings.Contains(ev.Message, "18080") && strings.Contains(ev.Message, "mapped") {
					mapped = true
				}
			}
			if !mapped {
				t.Fatalf("no mapped event for port 18080: %+v", events)
			}
			if err := h.d.Delete(context.Background(), sb.ID); err != nil {
				t.Fatalf("delete: %v", err)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("auto-publish did not publish guest port 18080")
}

func TestAutoPublishSkipsOccupiedPortThenRetries(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:18081")
	if err != nil {
		t.Skipf("cannot reserve test port: %v", err)
	}
	defer ln.Close()

	h := newHarness(t)
	sb, err := h.d.Create(context.Background(), manager.CreateOptions{
		Name: "auto-busy", Image: "img:test", AutoPublish: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	a := h.dial.last()
	a.mu.Lock()
	a.ports = []int{18081}
	a.mu.Unlock()

	time.Sleep(1500 * time.Millisecond)
	h.net.mu.Lock()
	early := len(h.net.portSets)
	for _, set := range h.net.portSets {
		for _, p := range set {
			if p.HostPort == 18081 {
				t.Fatalf("occupied port was published: %+v", p)
			}
		}
	}
	h.net.mu.Unlock()
	_ = early

	_ = ln.Close()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		h.net.mu.Lock()
		var got []types.PortMapping
		for _, set := range h.net.portSets {
			for _, p := range set {
				if p.GuestPort == 18081 && p.HostPort == 18081 {
					got = append(got, p)
				}
			}
		}
		h.net.mu.Unlock()
		if len(got) > 0 {
			_ = h.d.Delete(context.Background(), sb.ID)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("occupied port was not published after it became free")
}

func TestAutoPublishLoopStartsForAdoptedRuntime(t *testing.T) {
	h := newHarness(t)
	sb, err := h.d.Create(context.Background(), manager.CreateOptions{
		Name: "adopted", Image: "img:test",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	rt := h.runtime(t, sb.ID)
	rt.mu.Lock()
	rt.sb.AutoPublish = true
	rt.autoLoopStarted = false
	rt.mu.Unlock()
	h.d.startAutoPublishLoop(rt)
	a := h.dial.last()
	a.mu.Lock()
	a.ports = []int{18082}
	a.mu.Unlock()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		h.net.mu.Lock()
		var got []types.PortMapping
		if n := len(h.net.portSets); n > 0 {
			got = append(got, h.net.portSets[n-1]...)
		}
		h.net.mu.Unlock()
		for _, p := range got {
			if p.GuestPort == 18082 && p.HostPort == 18082 && p.LoopbackOnly {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("adopted runtime did not start auto-publish discovery")
}

func TestNoNetworkSleepDoesNotTouchNetwork(t *testing.T) {
	h := newHarness(t)
	netEnabled := false
	sb, err := h.d.Create(context.Background(), manager.CreateOptions{
		Name: "no-net", Image: "img:test", NetworkEnabled: &netEnabled,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := h.d.Sleep(context.Background(), sb.ID, false); err != nil {
		t.Fatalf("sleep: %v", err)
	}
	if err := h.d.Wake(context.Background(), sb.ID); err != nil {
		t.Fatalf("wake: %v", err)
	}
	h.net.mu.Lock()
	_, touched := h.net.asleep[sb.ID]
	h.net.mu.Unlock()
	if touched {
		t.Fatal("network-disabled sandbox must not call SetAsleep")
	}
	if err := h.d.Delete(context.Background(), sb.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestPrometheusMetrics(t *testing.T) {
	h := newHarness(t)
	h.imgs.disk.SizeBytes = 12345
	sb, err := h.d.Create(context.Background(), manager.CreateOptions{
		Name: "metrics", Image: "img:test", MemoryMB: 1024,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := h.d.Exec(context.Background(), sb.ID, []string{"true"}, nil); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if err := h.d.Sleep(context.Background(), sb.ID, false); err != nil {
		t.Fatalf("sleep: %v", err)
	}
	if err := h.d.Wake(context.Background(), sb.ID); err != nil {
		t.Fatalf("wake: %v", err)
	}
	data, err := h.d.PrometheusMetrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	for _, want := range []string{
		"fastvm_up 1",
		`fastvm_sandboxes{state="Running"} 1`,
		"fastvm_images 1",
		"fastvm_image_cache_bytes 12345",
		`fastvm_sandbox_info{sandbox_id="` + sb.ID + `"`,
		`fastvm_sandbox_cpu_percent{sandbox_id="` + sb.ID + `"}`,
		`fastvm_sandbox_memory_rss_bytes{sandbox_id="` + sb.ID + `"}`,
		`fastvm_sandbox_network_rx_bytes_total{sandbox_id="` + sb.ID + `"}`,
		`fastvm_sandbox_wakes_total{sandbox_id="` + sb.ID + `"} 1`,
		`fastvm_sandbox_sleeps_total{sandbox_id="` + sb.ID + `"} 1`,
		`fastvm_sandbox_execs_total{sandbox_id="` + sb.ID + `"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q:\n%s", want, out)
		}
	}
	_ = h.d.Delete(context.Background(), sb.ID)
}

func TestCopyGuestFile(t *testing.T) {
	h := newHarness(t)
	sb, err := h.d.Create(context.Background(), manager.CreateOptions{Name: "copy", Image: "img:test"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	payload := bytes.Repeat([]byte("copy-data-"), 1000)
	if err := h.d.CopyToGuest(context.Background(), sb.ID, "dir/file.txt", 0o640, bytes.NewReader(payload)); err != nil {
		t.Fatalf("copy to guest: %v", err)
	}
	var out bytes.Buffer
	mode, size, err := h.d.CopyFromGuest(context.Background(), sb.ID, "dir/file.txt", &out)
	if err != nil {
		t.Fatalf("copy from guest: %v", err)
	}
	if !bytes.Equal(out.Bytes(), payload) || size != int64(len(payload)) || mode.Perm() != 0o640 {
		t.Fatalf("copied data mismatch: size=%d mode=%o", size, mode)
	}
	_ = h.d.Delete(context.Background(), sb.ID)
}

func TestRuntimeForStartsMonitor(t *testing.T) {
	h := newHarness(t)
	h.d.cfg.Lifecycle.IdleDetection.SampleInterval = config.Duration(10 * time.Millisecond)
	sb := h.create(t)
	h.drv.mu.Lock()
	h.drv.vms[0].stats = types.VMStats{MemoryRSSBytes: 1234, VCPUTimeNS: 1000}
	h.drv.mu.Unlock()

	// Simulate a runtime created outside Create (e.g. adopted after restart).
	h.d.mu.Lock()
	old := h.d.runtimes[sb.ID]
	delete(h.d.runtimes, sb.ID)
	h.d.mu.Unlock()
	old.stop()
	time.Sleep(30 * time.Millisecond)

	if _, err := h.d.runtimeFor(context.Background(), sb.ID); err != nil {
		t.Fatalf("runtimeFor: %v", err)
	}
	rt := h.runtime(t, sb.ID)
	rt.mu.Lock()
	rt.vm = h.drv.vms[0]
	rt.mu.Unlock()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		stats, err := h.d.Stats(context.Background(), sb.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stats.MemoryRSSBytes == 1234 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("monitor did not sample adopted runtime stats")
}
