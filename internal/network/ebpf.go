package network

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/vishvananda/netlink"

	"fastvm/internal/types"
)

// eBPF-mode network topology.
//
//	host stack fvup <-> fvuplink (switch uplink port)
//	                       |
//	             eBPF switch (static MAC -> port)
//	                       |
//	                    tap-<id> -> Firecracker -> guest
//
// The switch has one ingress classifier per port:
//   - TAP ingress sees guest->host frames; the program redirects them to the
//     uplink port.
//   - Uplink ingress sees host->guest frames; the program looks up the guest
//     MAC -> TAP port, emits a ringbuf wake event if that port is asleep, and
//     redirects the frame into the TAP (Firecracker's virtio-net queue).
//
// No Linux bridge is needed: the eBPF switch owns L2 forwarding, isolation,
// anti-spoofing and wake notifications. Gateway addresses and static neighbors
// live on fvup (the host-stack side of the uplink pair); nftables DNAT and
// masquerade rules are rendered against it.
type ebpfManager struct {
	opts  Options
	alloc *allocator
	nft   nftApplier

	uplink    string // external interface for masquerade/NAT
	hostIface string // fvup: host-stack side of the uplink veth pair
	switchIf  string // fvuplink: switch uplink port

	mu     sync.Mutex
	ports  map[string]*ebpfPort
	closed bool
	wake   chan WakeEvent
}

type ebpfPort struct {
	alloc   *allocation
	ifIndex int
}

const (
	ebpfUplinkHost   = "fvup"
	ebpfUplinkSwitch = "fvuplink"
)

func newEBPFManager(opts Options) (*ebpfManager, error) {
	if opts.CIDR == "" {
		opts.CIDR = "10.77.0.0/16"
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
	case "-":
		uplink = ""
	case "":
		uplink = detectUplink()
	default:
		if err := validateIfaceName(uplink); err != nil {
			return nil, fmt.Errorf("network: invalid uplink interface: %w", err)
		}
	}
	return &ebpfManager{
		opts:      opts,
		alloc:     alloc,
		nft:       nftApplier{bin: opts.NFTBin, table: nftTable},
		uplink:    uplink,
		hostIface: ebpfUplinkHost,
		switchIf:  ebpfUplinkSwitch,
		ports:     make(map[string]*ebpfPort),
		wake:      make(chan WakeEvent, 1),
	}, nil
}

// UplinkPort is the switch port representing the host stack.
type UplinkPort struct {
	Name    string
	IfIndex int
	MAC     string
}

// UplinkProvider is implemented by managers whose data plane has a switch
// uplink port (eBPF mode). The daemon registers it with the L2 switch.
type UplinkProvider interface {
	UplinkPort() (UplinkPort, error)
}

// UplinkPort creates/reuses fvup<->fvuplink and returns the switch side.
func (m *ebpfManager) UplinkPort() (UplinkPort, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ensureUplinkLocked(); err != nil {
		return UplinkPort{}, err
	}
	host, err := netlink.LinkByName(m.hostIface)
	if err != nil {
		return UplinkPort{}, fmt.Errorf("network: look up %s: %w", m.hostIface, err)
	}
	sw, err := netlink.LinkByName(m.switchIf)
	if err != nil {
		return UplinkPort{}, fmt.Errorf("network: look up %s: %w", m.switchIf, err)
	}
	return UplinkPort{Name: m.switchIf, IfIndex: sw.Attrs().Index, MAC: host.Attrs().HardwareAddr.String()}, nil
}

func (m *ebpfManager) ensureUplinkLocked() error {
	port, peer, err := ensureVethPair(m.switchIf, m.hostIface)
	if err != nil {
		return err
	}
	if err := netlink.LinkSetUp(port); err != nil {
		return fmt.Errorf("network: bring up %s: %w", m.switchIf, err)
	}
	if err := netlink.LinkSetUp(peer); err != nil {
		return fmt.Errorf("network: bring up %s: %w", m.hostIface, err)
	}
	return nil
}

func (m *ebpfManager) Create(ctx context.Context, req CreateRequest) (Port, error) {
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

	if err := prepareNetns(len(req.Ports) > 0); err != nil {
		m.alloc.release(alloc.sandboxID)
		return Port{}, err
	}
	if err := ensureIPForward(); err != nil {
		m.alloc.release(alloc.sandboxID)
		return Port{}, err
	}
	if err := m.ensureUplinkLocked(); err != nil {
		m.alloc.release(alloc.sandboxID)
		return Port{}, err
	}

	ep := &ebpfPort{alloc: alloc}

	fail := func(stepErr error) (Port, error) {
		cleanupErr := m.teardownLocked(context.WithoutCancel(ctx), ep)
		return Port{}, errors.Join(fmt.Errorf("network: create %s: %w", req.SandboxID, stepErr), cleanupErr)
	}

	if err := createPersistentTAP(alloc.tap); err != nil {
		m.alloc.release(alloc.sandboxID)
		return Port{}, err
	}
	// Upgrading from the bridge backend leaves per-sandbox gateway addresses
	// and static neighbors on the old shared bridge; remove them so they do not
	// shadow the fvup routes.
	if m.opts.Bridge != "" && m.opts.Bridge != m.hostIface {
		_ = removeStaticNeighbor(m.opts.Bridge, alloc.ip)
		_ = removeGatewayAddr(m.opts.Bridge, alloc.gateway, alloc.prefix)
	}
	if err := configureTAPDirect(alloc.tap, alloc.mac); err != nil {
		return fail(err)
	}
	if err := addGatewayAddr(m.hostIface, alloc.gateway, alloc.prefix); err != nil {
		return fail(err)
	}
	if err := ensureStaticNeighbor(m.hostIface, alloc.ip, alloc.mac); err != nil {
		return fail(err)
	}
	tapLink, err := netlink.LinkByName(alloc.tap)
	if err != nil {
		return fail(fmt.Errorf("look up TAP %q: %w", alloc.tap, err))
	}
	ep.ifIndex = tapLink.Attrs().Index
	// Register before rendering: DNAT mappings are derived from m.ports.
	m.ports[alloc.sandboxID] = ep
	if err := m.applyNFT(ctx, nil); err != nil {
		return fail(err)
	}

	return Port{
		SandboxID:     alloc.sandboxID,
		TAPName:       alloc.tap,
		MAC:           alloc.mac.String(),
		IP:            alloc.ip.String(),
		Gateway:       alloc.gateway.String(),
		SwitchPort:    alloc.tap,
		SwitchIfIndex: tapLink.Attrs().Index,
	}, nil
}

func (m *ebpfManager) Delete(ctx context.Context, sandboxID string) error {
	if sandboxID == "" {
		return errors.New("network: delete: sandbox ID must not be empty")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	ep, ok := m.ports[sandboxID]
	if !ok {
		return nil
	}
	return m.teardownLocked(ctx, ep)
}

func (m *ebpfManager) applyNFT(ctx context.Context, except *ebpfPort) error {
	body, err := renderNftables(m.nftSpec(except))
	if err != nil {
		return err
	}
	return m.nft.apply(ctx, body)
}

func (m *ebpfManager) nftSpec(except *ebpfPort) nftSpec {
	var mappings []nftMapping
	for _, ep := range m.ports {
		if except != nil && ep.alloc.sandboxID == except.alloc.sandboxID {
			continue
		}
		for _, p := range ep.alloc.ports {
			mappings = append(mappings, nftMapping{
				GuestIP: ep.alloc.ip.String(), HostPort: p.HostPort, GuestPort: p.GuestPort, Proto: p.Proto,
			})
		}
	}
	return nftSpec{
		Table:    nftTable,
		Bridge:   m.hostIface,
		CIDR:     m.opts.CIDR,
		Uplink:   m.uplink,
		Mappings: mappings,
	}
}

func (m *ebpfManager) teardownLocked(ctx context.Context, ep *ebpfPort) error {
	delete(m.ports, ep.alloc.sandboxID)
	var errs []error
	if err := m.applyNFT(ctx, ep); err != nil {
		errs = append(errs, err)
	}
	if err := removeStaticNeighbor(m.hostIface, ep.alloc.ip); err != nil {
		errs = append(errs, err)
	}
	if err := removeGatewayAddr(m.hostIface, ep.alloc.gateway, ep.alloc.prefix); err != nil {
		errs = append(errs, err)
	}
	if err := deleteLink(ep.alloc.tap); err != nil {
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		m.alloc.release(ep.alloc.sandboxID)
	}
	return errors.Join(errs...)
}

func (m *ebpfManager) SetAsleep(ctx context.Context, sandboxID string, asleep bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	ep, ok := m.ports[sandboxID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, sandboxID)
	}
	if asleep {
		if err := ensureStaticNeighbor(m.hostIface, ep.alloc.ip, ep.alloc.mac); err != nil {
			return err
		}
	}
	return nil
}

func (m *ebpfManager) Stats(ctx context.Context, sandboxID string) (types.NetStats, error) {
	if err := ctx.Err(); err != nil {
		return types.NetStats{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return types.NetStats{}, ErrClosed
	}
	ep, ok := m.ports[sandboxID]
	if !ok {
		return types.NetStats{}, fmt.Errorf("%w: %s", ErrNotFound, sandboxID)
	}
	return linkNetStats(ep.alloc.tap)
}

func (m *ebpfManager) WakeEvents() <-chan WakeEvent { return m.wake }

func (m *ebpfManager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	close(m.wake)
	// Leave the interfaces and nft rules in place: dormant sandboxes must be
	// adoptable by a restarted daemon.
	return nil
}

func (m *ebpfManager) String() string {
	return fmt.Sprintf("network.Manager(ebpf bridge-per-sandbox, uplink=%s, host=%s)", m.uplink, m.hostIface)
}

// ensureIPForward makes guest<->host/external L3 forwarding possible.
func ensureIPForward() error {
	const path = "/proc/sys/net/ipv4/ip_forward"
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("network: read %s: %w", path, err)
	}
	if strings.TrimSpace(string(b)) == "1" {
		return nil
	}
	if err := os.WriteFile(path, []byte("1"), 0o644); err != nil {
		return fmt.Errorf("network: enable IPv4 forwarding: %w", err)
	}
	return nil
}
