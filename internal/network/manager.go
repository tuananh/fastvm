package network

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/vishvananda/netlink"

	"fastvm/internal/types"
)

// manager is the bridge-backed Manager (docs/DESIGN.md §6.6 fallback path).
//
// Every sandbox gets: a persistent TAP in the daemon network namespace, a
// per-sandbox point-to-point /30 gateway address on the shared bridge, a
// permanent ARP/FDB entry so the host can reach a sleeping guest, nftables
// DNAT for published ports (+ masquerade on the uplink), and a passive
// AF_PACKET sniffer for wake events and activity counters.
//
// The AF_PACKET sniffer is always enabled: host->guest frames are transmitted
// *out* of the TAP (not received on it), so a tc ingress eBPF classifier on the
// TAP cannot observe the wake-triggering direction. The eBPF L2 switch backend
// is implemented and tested standalone but is not wired into this bridge/TAP
// topology; see internal/switch.
type manager struct {
	opts   Options
	alloc  *allocator
	nft    nftApplier
	sniff  bool
	uplink string
	wake   chan WakeEvent

	mu       sync.Mutex
	watchers map[string]*packetWatcher
	closed   bool
}

func newManager(opts Options) (*manager, error) {
	if opts.Bridge == "" {
		opts.Bridge = "fastvm0"
	}
	if err := validateIfaceName(opts.Bridge); err != nil {
		return nil, fmt.Errorf("network: invalid bridge name: %w", err)
	}
	if opts.CIDR == "" {
		opts.CIDR = "10.77.0.0/16"
	}
	if opts.SwitchMode == "" {
		opts.SwitchMode = "bridge"
	}
	switch opts.SwitchMode {
	case "bridge", "ebpf":
	default:
		return nil, fmt.Errorf("network: switch mode %q must be bridge or ebpf", opts.SwitchMode)
	}
	if opts.NFTBin == "" {
		opts.NFTBin = "nft"
	}

	alloc, err := newAllocator(opts.CIDR)
	if err != nil {
		return nil, err
	}

	uplink := opts.UplinkInterface
	switch uplink {
	case "-": // explicitly disable masquerade (e.g. no uplink in tests)
		uplink = ""
	case "": // auto-detect; empty result means "no masquerade"
		uplink = detectUplink()
	default:
		if err := validateIfaceName(uplink); err != nil {
			return nil, fmt.Errorf("network: invalid uplink interface: %w", err)
		}
	}

	return &manager{
		opts:     opts,
		alloc:    alloc,
		nft:      nftApplier{bin: opts.NFTBin, table: nftTable},
		sniff:    true, // host->guest wake frames are not visible on TAP ingress
		uplink:   uplink,
		wake:     make(chan WakeEvent, 256),
		watchers: make(map[string]*packetWatcher),
	}, nil
}

func (m *manager) Create(ctx context.Context, req CreateRequest) (Port, error) {
	if err := ctx.Err(); err != nil {
		return Port{}, err
	}
	if req.SandboxID == "" {
		return Port{}, errors.New("network: create: sandbox ID must not be empty")
	}
	if err := validatePorts(req.Ports); err != nil {
		return Port{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Port{}, ErrClosed
	}
	if _, exists := m.alloc.get(req.SandboxID); exists {
		return Port{}, fmt.Errorf("%w: %s", ErrExists, req.SandboxID)
	}

	alloc, err := m.alloc.allocate(req.SandboxID, req.MAC, req.IP)
	if err != nil {
		return Port{}, err
	}
	alloc.ports = append([]types.PortMapping(nil), req.Ports...)

	// Localhost publishing needs a live loopback and route_localnet so the
	// DNAT'ed packet can leave via the bridge; hairpin SNAT is added to the
	// rendered rules below. Do this before touching the datapath.
	if err := prepareNetns(len(req.Ports) > 0); err != nil {
		m.alloc.release(alloc.sandboxID)
		return Port{}, err
	}

	// Roll back the whole attachment if any step fails; no partially configured
	// sandbox is ever reported as created.
	fail := func(stepErr error) (Port, error) {
		cleanupErr := m.teardownLocked(context.WithoutCancel(ctx), alloc)
		return Port{}, errors.Join(fmt.Errorf("network: create %s: %w", req.SandboxID, stepErr), cleanupErr)
	}

	if _, err := ensureBridge(m.opts.Bridge); err != nil {
		m.alloc.release(alloc.sandboxID)
		return Port{}, err
	}
	if err := createPersistentTAP(alloc.tap); err != nil {
		m.alloc.release(alloc.sandboxID)
		return Port{}, err
	}
	if err := configureTAP(alloc.tap, m.opts.Bridge, alloc.mac, m.opts.Isolate); err != nil {
		return fail(err)
	}
	if err := addGatewayAddr(m.opts.Bridge, alloc.gateway, alloc.prefix); err != nil {
		return fail(err)
	}
	if err := ensureStaticNeighbor(m.opts.Bridge, alloc.ip, alloc.mac); err != nil {
		return fail(err)
	}
	// Render the complete rule set including the new sandbox's ports.
	if err := m.applyNFT(ctx, nil); err != nil {
		return fail(err)
	}
	if m.sniff {
		link, err := netlink.LinkByName(alloc.tap)
		if err != nil {
			return fail(fmt.Errorf("look up TAP %q for sniffer: %w", alloc.tap, err))
		}
		w, err := newPacketWatcher(watcherConfig{
			sandboxID:   alloc.sandboxID,
			ifindex:     link.Attrs().Index,
			mac:         alloc.mac,
			wakeNotify:  m.opts.WakeNotify,
			wake:        m.wake,
			ignorePorts: ignorePortSet(m.opts.IgnorePorts),
		})
		if err != nil {
			return fail(err)
		}
		m.watchers[alloc.sandboxID] = w
	}

	return m.portFor(alloc), nil
}

func (m *manager) Delete(ctx context.Context, sandboxID string) error {
	if sandboxID == "" {
		return errors.New("network: delete: sandbox ID must not be empty")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	alloc, ok := m.alloc.get(sandboxID)
	if !ok {
		return nil // idempotent
	}
	return m.teardownLocked(ctx, alloc)
}

// applyNFT renders and applies the full rule set. except, when non-nil, is
// omitted from the rendering (used while tearing that sandbox down).
func (m *manager) applyNFT(ctx context.Context, except *allocation) error {
	body, err := renderNftables(m.nftSpec(except))
	if err != nil {
		return err
	}
	return m.nft.apply(ctx, body)
}

// teardownLocked removes everything Create installed. The allocation stays
// registered until every best-effort step has been attempted, so a failed
// Delete can be retried safely. Caller holds m.mu.
func (m *manager) teardownLocked(ctx context.Context, alloc *allocation) error {
	var errs []error

	if w, ok := m.watchers[alloc.sandboxID]; ok {
		delete(m.watchers, alloc.sandboxID)
		w.close()
	}

	if err := m.applyNFT(ctx, alloc); err != nil {
		errs = append(errs, err)
	}
	if err := removeStaticNeighbor(m.opts.Bridge, alloc.ip); err != nil {
		errs = append(errs, err)
	}
	if err := removeGatewayAddr(m.opts.Bridge, alloc.gateway, alloc.prefix); err != nil {
		errs = append(errs, err)
	}
	if err := deleteLink(alloc.tap); err != nil {
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		m.alloc.release(alloc.sandboxID)
	}
	return errors.Join(errs...)
}

func (m *manager) SetAsleep(ctx context.Context, sandboxID string, asleep bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	alloc, ok := m.alloc.get(sandboxID)
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, sandboxID)
	}
	if m.sniff {
		if w, ok := m.watchers[sandboxID]; ok {
			w.setAsleep(asleep)
		}
	}
	if asleep {
		// Refresh the static neighbor on the sleep transition (docs/DESIGN.md §5.5)
		// so the host can deliver the wake-triggering frame without ARP.
		if err := ensureStaticNeighbor(m.opts.Bridge, alloc.ip, alloc.mac); err != nil {
			return err
		}
	}
	return nil
}

func (m *manager) Stats(ctx context.Context, sandboxID string) (types.NetStats, error) {
	if err := ctx.Err(); err != nil {
		return types.NetStats{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return types.NetStats{}, ErrClosed
	}
	alloc, ok := m.alloc.get(sandboxID)
	if !ok {
		return types.NetStats{}, fmt.Errorf("%w: %s", ErrNotFound, sandboxID)
	}
	if w, ok := m.watchers[sandboxID]; ok {
		return w.netStats(), nil
	}
	return linkNetStats(alloc.tap)
}

func (m *manager) Activity(ctx context.Context, sandboxID string) (Activity, error) {
	if err := ctx.Err(); err != nil {
		return Activity{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Activity{}, ErrClosed
	}
	if _, ok := m.alloc.get(sandboxID); !ok {
		return Activity{}, fmt.Errorf("%w: %s", ErrNotFound, sandboxID)
	}
	if w, ok := m.watchers[sandboxID]; ok {
		return w.activity(), nil
	}
	return Activity{}, nil
}

func (m *manager) WakeEvents() <-chan WakeEvent { return m.wake }

func (m *manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	// Stop sniffers concurrently: each blocked read can take up to the socket
	// receive timeout to observe the close.
	var wg sync.WaitGroup
	for id, w := range m.watchers {
		wg.Add(1)
		delete(m.watchers, id)
		go func(w *packetWatcher) {
			defer wg.Done()
			w.close()
		}(w)
	}
	wg.Wait()
	close(m.wake)
	// Deliberately leave TAPs, bridge addresses and nftables rules in place:
	// VMMs and dormant sandboxes must survive a daemon restart (docs/DESIGN.md G7).
	// The reconciler re-creates Manager state and re-arms sniffers via Create
	// with the recorded MAC/IP.
	return nil
}

func (m *manager) portFor(alloc *allocation) Port {
	return Port{
		SandboxID: alloc.sandboxID,
		// Bridge mode keeps everything in the daemon network namespace; the VMM
		// is spawned in the same namespace, so no netns path is needed.
		NetnsPath: "",
		TAPName:   alloc.tap,
		MAC:       alloc.mac.String(),
		IP:        alloc.ip.String(),
		Gateway:   alloc.gateway.String(),
	}
}

func (m *manager) nftSpec(except *allocation) nftSpec {
	all := m.alloc.list()
	if except != nil {
		filtered := all[:0]
		for _, a := range all {
			if a.sandboxID != except.sandboxID {
				filtered = append(filtered, a)
			}
		}
		all = filtered
	}
	return nftSpec{
		Table:    nftTable,
		Bridge:   m.opts.Bridge,
		CIDR:     m.opts.CIDR,
		Uplink:   m.uplink,
		Mappings: nftMappings(all),
	}
}

func ignorePortSet(ports []int) map[int]bool {
	if len(ports) == 0 {
		return nil
	}
	set := make(map[int]bool, len(ports))
	for _, p := range ports {
		set[p] = true
	}
	return set
}

// String implements fmt.Stringer for diagnostics.
func (m *manager) String() string {
	return fmt.Sprintf("network.Manager(bridge=%s, mode=%s, uplink=%s)",
		m.opts.Bridge, m.opts.SwitchMode, orNone(m.uplink))
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return strings.TrimSpace(s)
}
