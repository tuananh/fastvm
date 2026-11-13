// Package l2switch implements the per-host L2 switch (docs/DESIGN.md §6.6, M3).
//
// Two backends share one interface:
//
//   - ebpf (default): tc clsact programs + BPF maps loaded with cilium/ebpf.
//     Frames are redirected in-kernel, sandbox-to-sandbox traffic is dropped by
//     default, source MACs are anti-spoofed, per-port counters feed the idle
//     predicate and sleeping ports produce ringbuf wake events.
//   - bridge (dev/CI/rollback fallback): a no-op Switch. The real bridge data
//     plane, wake sniffers and counters live in internal/network; the daemon
//     must use network.Manager.WakeEvents/Stats in bridge mode.
//
// Open returns a typed *UnsupportedError when eBPF cannot be loaded (missing
// BTF, verifier failure, no CAP_BPF) so the daemon can fall back explicitly;
// OpenWithFallback does that fallback for it.
package l2switch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"fastvm/internal/config"
)

// Mode selects the L2 switch backend.
type Mode string

const (
	ModeEBPF   Mode = "ebpf"
	ModeBridge Mode = "bridge"
)

// ParseMode validates a configured switch.mode value. Empty means the eBPF
// default (docs/DESIGN.md §5.4).
func ParseMode(s string) (Mode, error) {
	switch s {
	case "", string(ModeEBPF):
		return ModeEBPF, nil
	case string(ModeBridge):
		return ModeBridge, nil
	default:
		return "", fmt.Errorf("switch: mode %q must be ebpf or bridge", s)
	}
}

// Port is one attachment point on the switch: a sandbox's host-side veth
// (IsSandbox) or the uplink.
type Port struct {
	SandboxID string
	IfIndex   int
	MAC       net.HardwareAddr
	IsSandbox bool
}

// PortStats are the per-port counters exported from the BPF maps.
type PortStats struct {
	RxBytes          uint64
	RxPackets        uint64
	TxBytes          uint64
	TxPackets        uint64
	Drops            uint64
	ActivityPackets  uint64
	KeepalivePackets uint64
	LastActivity     time.Time
}

// WakeEvent is emitted when a qualifying frame targets a sleeping port.
type WakeEvent struct {
	SandboxID string
	IfIndex   int
	Time      time.Time
}

// Switch is the L2 data plane.
type Switch interface {
	Mode() Mode
	AddPort(ctx context.Context, p Port) error
	RemovePort(ctx context.Context, sandboxID string) error
	SetAsleep(ctx context.Context, sandboxID string, asleep bool) error
	Stats(ctx context.Context, sandboxID string) (PortStats, error)
	WakeEvents() <-chan WakeEvent
	Close() error
}

// Config configures a backend.
type Config struct {
	// Mode selects the backend; empty means ModeEBPF.
	Mode Mode
	// IsolateSandboxes drops sandbox-to-sandbox frames (default policy).
	IsolateSandboxes bool
	// WakeNotify enables ringbuf wake events for sleeping ports.
	WakeNotify bool
	// UplinkIfIndex is the switch's uplink port (0 = allow the host stack to
	// handle unknown destinations instead of redirecting to an uplink).
	UplinkIfIndex int
	// Object overrides the embedded eBPF object (tests / pinned rebuilds).
	Object []byte
}

// FromConfig derives a switch Config from the daemon config.
func FromConfig(c config.Switch) (Config, error) {
	mode, err := ParseMode(c.Mode)
	if err != nil {
		return Config{}, err
	}
	return Config{
		Mode:             mode,
		IsolateSandboxes: c.IsolateSandboxes,
		WakeNotify:       c.WakeNotify,
	}, nil
}

// UnsupportedError means the requested backend is unavailable in this
// environment; the daemon should fall back to switch.mode=bridge.
type UnsupportedError struct {
	Err error
}

func (e *UnsupportedError) Error() string {
	if e.Err == nil {
		return "switch: ebpf backend unsupported; use switch.mode=bridge"
	}
	return "switch: ebpf backend unsupported: " + e.Err.Error() + "; use switch.mode=bridge"
}

func (e *UnsupportedError) Unwrap() error { return e.Err }

// IsUnsupported reports whether err is an *UnsupportedError.
func IsUnsupported(err error) bool {
	var ue *UnsupportedError
	return errors.As(err, &ue)
}

// Open opens the configured backend. An eBPF failure is returned as
// *UnsupportedError; use OpenWithFallback for automatic fallback.
func Open(cfg Config) (Switch, error) {
	mode := cfg.Mode
	if mode == "" {
		mode = ModeEBPF
	}
	switch mode {
	case ModeBridge:
		return newBridgeSwitch(cfg, nil), nil
	case ModeEBPF:
		return openEBPF(cfg)
	default:
		return nil, fmt.Errorf("switch: unknown mode %q", mode)
	}
}

// OpenWithFallback opens the configured backend, falling back to the bridge
// backend when eBPF is unavailable. The returned Switch reports the mode
// actually in use; for the bridge fallback the reason is available via
// *BridgeSwitch.FallbackReason.
func OpenWithFallback(cfg Config) (Switch, error) {
	mode := cfg.Mode
	if mode == "" {
		mode = ModeEBPF
	}
	if mode == ModeBridge {
		return newBridgeSwitch(cfg, nil), nil
	}
	s, err := openEBPF(cfg)
	if err == nil {
		return s, nil
	}
	if !IsUnsupported(err) {
		return nil, err
	}
	return newBridgeSwitch(cfg, err), nil
}
