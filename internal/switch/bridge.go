package l2switch

import (
	"context"
	"fmt"
	"sync"
)

// BridgeSwitch is the fallback backend. It deliberately does not program any
// data plane: in bridge mode the Linux bridge (network.Manager) forwards
// frames and the AF_PACKET sniffer in internal/network produces wake events and
// counters. This type exists so the factory has one interface, and so the
// daemon can observe which backend is active and why.
type BridgeSwitch struct {
	cfg    Config
	reason error // eBPF failure that triggered the fallback, if any

	mu        sync.Mutex
	ports     map[string]Port
	byIfindex map[int]string
	wake      chan WakeEvent
	closed    bool
}

func newBridgeSwitch(cfg Config, reason error) *BridgeSwitch {
	return &BridgeSwitch{
		cfg:       cfg,
		reason:    reason,
		ports:     make(map[string]Port),
		byIfindex: make(map[int]string),
		wake:      make(chan WakeEvent, 64),
	}
}

// Mode returns ModeBridge.
func (b *BridgeSwitch) Mode() Mode { return ModeBridge }

// FallbackReason returns the eBPF error that caused the fallback, or nil when
// the bridge mode was configured explicitly.
func (b *BridgeSwitch) FallbackReason() error { return b.reason }

// AddPort records the port. The bridge data plane is owned by network.Manager,
// so there is nothing to program here.
func (b *BridgeSwitch) AddPort(_ context.Context, p Port) error {
	if p.SandboxID == "" {
		return fmt.Errorf("switch: add port: sandbox ID must not be empty")
	}
	if p.IfIndex <= 0 {
		return fmt.Errorf("switch: add port %s: invalid ifindex %d", p.SandboxID, p.IfIndex)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return fmt.Errorf("switch: closed")
	}
	if _, ok := b.ports[p.SandboxID]; ok {
		return fmt.Errorf("switch: port %s already exists", p.SandboxID)
	}
	b.ports[p.SandboxID] = p
	b.byIfindex[p.IfIndex] = p.SandboxID
	return nil
}

// RemovePort forgets a port; it is idempotent.
func (b *BridgeSwitch) RemovePort(_ context.Context, sandboxID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.ports[sandboxID]
	if !ok {
		return nil
	}
	delete(b.ports, sandboxID)
	delete(b.byIfindex, p.IfIndex)
	return nil
}

// SetAsleep is a no-op: network.Manager's sniffer owns bridge-mode wake gating.
func (b *BridgeSwitch) SetAsleep(_ context.Context, sandboxID string, _ bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.ports[sandboxID]; !ok {
		return fmt.Errorf("switch: port %s not found", sandboxID)
	}
	return nil
}

// Stats returns zeros: bridge-mode counters come from network.Manager.Stats /
// network.ActivitySource.
func (b *BridgeSwitch) Stats(_ context.Context, sandboxID string) (PortStats, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.ports[sandboxID]; !ok {
		return PortStats{}, fmt.Errorf("switch: port %s not found", sandboxID)
	}
	return PortStats{}, nil
}

// WakeEvents returns a channel that never receives events in bridge mode (the
// daemon must consume network.Manager.WakeEvents instead). Closed on Close.
func (b *BridgeSwitch) WakeEvents() <-chan WakeEvent { return b.wake }

// Close releases the backend; idempotent.
func (b *BridgeSwitch) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	close(b.wake)
	b.ports = make(map[string]Port)
	b.byIfindex = make(map[int]string)
	return nil
}

// lookupIfindex returns the sandbox ID for an ifindex (used by tests/ebpf).
func (b *BridgeSwitch) lookupIfindex(ifindex int) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id, ok := b.byIfindex[ifindex]
	return id, ok
}

var _ Switch = (*BridgeSwitch)(nil)
