// Package network provisions per-sandbox TAP devices, Linux-bridge ports,
// nftables NAT/DNAT and wake notifications (docs/DESIGN.md §5.5, §6.6).
//
// v1 topology (bridge backend, the tested default): the daemon runs in a
// network namespace (e.g. started under `unshare -Urn` in development) and the
// TAP, bridge, addresses and nftables rules all live in that namespace. The
// VMM is spawned in the same namespace, so Firecracker opens the persistent TAP
// by name and no setns is needed. Port.NetnsPath is therefore empty in bridge
// mode; a per-sandbox network namespace is a later optimisation.
package network

import (
	"context"
	"errors"
	"time"

	"fastvm/internal/types"
)

// CreateRequest asks for one sandbox network attachment.
type CreateRequest struct {
	SandboxID string
	Ports     []types.PortMapping
	MAC       string // optional; allocated when empty
	IP        string // optional; reused on adoption/reconcile, allocated when empty
}

// Port is the result of Create.
type Port struct {
	SandboxID string
	NetnsPath string
	VethHost  string
	VethIn    string
	TAPName   string
	MAC       string
	IP        string
	Gateway   string

	// SwitchPort/SwitchIfIndex are set in eBPF mode: the host-side veth switch
	// port for this sandbox (TAPName remains the guest TAP).
	SwitchPort    string
	SwitchIfIndex int
}

// WakeEvent is emitted by the switch when a frame targets a sleeping port.
type WakeEvent struct {
	SandboxID string
	Time      time.Time
}

// Activity summarises per-port traffic for the idle predicate (docs/DESIGN.md §5.3).
// Rx is host-to-guest (what the sandbox receives), Tx is guest-to-host.
type Activity struct {
	RxBytes          uint64
	TxBytes          uint64
	RxPackets        uint64
	TxPackets        uint64
	Drops            uint64
	ActivityPackets  uint64 // data, SYN/FIN/RST, UDP/ICMP payload
	KeepalivePackets uint64 // pure ACKs and zero-payload datagrams
	LastActivity     time.Time
	LastFrame        time.Time
}

// ActivitySource is implemented by managers that classify frames for the idle
// predicate. The wake dispatcher only needs Manager.WakeEvents.
type ActivitySource interface {
	Activity(ctx context.Context, sandboxID string) (Activity, error)
}

// Manager owns the host-side network data plane.
type Manager interface {
	Create(ctx context.Context, req CreateRequest) (Port, error)
	Delete(ctx context.Context, sandboxID string) error
	// SetAsleep flips the port's switch state: asleep ports still receive
	// frames (queue-live model) but emit wake events instead of staying quiet.
	SetAsleep(ctx context.Context, sandboxID string, asleep bool) error
	Stats(ctx context.Context, sandboxID string) (types.NetStats, error)
	// WakeEvents returns switch wake notifications. The channel is closed when
	// the manager closes.
	WakeEvents() <-chan WakeEvent
	Close() error
}

// Errors returned by Manager implementations.
var (
	ErrNotFound    = errors.New("network: sandbox port not found")
	ErrExists      = errors.New("network: sandbox port already exists")
	ErrClosed      = errors.New("network: manager is closed")
	ErrUnsupported = errors.New("network: operation unsupported on this backend")
)

// Options configures the bridge-backed Manager.
type Options struct {
	// Bridge is the Linux bridge that sandbox TAPs are attached to.
	// Default "fastvm0".
	Bridge string
	// CIDR is the IPv4 pool sandboxes are addressed from. Each sandbox gets a
	// point-to-point /30: gateway .1, guest .2. Default "10.77.0.0/16".
	CIDR string
	// SwitchMode is the configured L2 switch mode ("bridge" or "ebpf").
	// In bridge mode the Manager runs AF_PACKET wake sniffers; in ebpf mode
	// wake detection is owned by internal/switch and no sniffer is started.
	SwitchMode string
	// Isolate marks sandbox bridge ports as isolated: sandboxes can reach the
	// host/uplink but not each other (docs/DESIGN.md §6.6 default policy).
	Isolate bool
	// WakeNotify enables WakeEvents emission.
	WakeNotify bool
	// NFTBin is the nftables binary used to apply NAT/DNAT rules.
	// Default "nft".
	NFTBin string

	// UplinkInterface is the egress interface masqueraded for sandbox traffic.
	// Empty means auto-detect from the default route (and no masquerade rule
	// when there is no default route, e.g. inside a bare `unshare -Urn`).
	UplinkInterface string
	// KeepaliveAware excludes pure ACKs / empty datagrams from activity and
	// wake decisions. Default true.
	KeepaliveAware bool
	// IgnorePorts are destination ports that never count as activity and never
	// wake a sandbox (idle.ignore_ports).
	IgnorePorts []int
}

// DefaultOptions returns the built-in bridge-backend defaults.
func DefaultOptions() Options {
	return Options{
		Bridge:         "fastvm0",
		CIDR:           "10.77.0.0/16",
		SwitchMode:     "bridge",
		Isolate:        true,
		WakeNotify:     true,
		NFTBin:         "nft",
		KeepaliveAware: true,
	}
}

// NewManager constructs the network Manager for the configured switch mode.
func NewManager(opts Options) (Manager, error) {
	if opts.SwitchMode == "ebpf" {
		return newEBPFManager(opts)
	}
	return newManager(opts)
}
