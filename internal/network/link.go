package network

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"fastvm/internal/types"
)

// Bridge/tap link management. All functions operate on the current network
// namespace and are safe to call repeatedly (ensure*/remove* are idempotent).

func linkNotFound(err error) bool {
	var nf netlink.LinkNotFoundError
	return errors.As(err, &nf)
}

// ensureBridge creates the host bridge if missing and brings it up.
func ensureBridge(name string) (netlink.Link, error) {
	if err := validateIfaceName(name); err != nil {
		return nil, err
	}
	link, err := netlink.LinkByName(name)
	if err == nil {
		if _, ok := link.(*netlink.Bridge); !ok {
			return nil, fmt.Errorf("network: %q exists but is not a bridge (%T)", name, link)
		}
		if link.Attrs().Flags&net.FlagUp == 0 {
			if err := netlink.LinkSetUp(link); err != nil {
				return nil, fmt.Errorf("network: bring bridge %q up: %w", name, err)
			}
		}
		return link, nil
	}
	if !linkNotFound(err) {
		return nil, fmt.Errorf("network: look up bridge %q: %w", name, err)
	}
	br := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: name}}
	if err := netlink.LinkAdd(br); err != nil {
		return nil, fmt.Errorf("network: create bridge %q: %w", name, err)
	}
	if err := netlink.LinkSetUp(br); err != nil {
		return nil, fmt.Errorf("network: bring bridge %q up: %w", name, err)
	}
	return br, nil
}

// configureTAP sets the TAP MAC, attaches it to the bridge, isolates the port
// when configured and brings the link up.
func configureTAP(tapName, bridgeName string, mac net.HardwareAddr, isolate bool) error {
	tap, err := netlink.LinkByName(tapName)
	if err != nil {
		if linkNotFound(err) {
			return fmt.Errorf("network: TAP %q disappeared: %w", tapName, err)
		}
		return fmt.Errorf("network: look up TAP %q: %w", tapName, err)
	}
	// An explicitly assigned MAC keeps the guest identity stable across
	// sleep/wake and is baked into snapshot metadata.
	if len(mac) > 0 && tap.Attrs().HardwareAddr.String() != mac.String() {
		if err := netlink.LinkSetHardwareAddr(tap, mac); err != nil {
			return fmt.Errorf("network: set TAP %q MAC: %w", tapName, err)
		}
	}
	br, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return fmt.Errorf("network: look up bridge %q: %w", bridgeName, err)
	}
	if err := netlink.LinkSetMaster(tap, br); err != nil {
		return fmt.Errorf("network: attach TAP %q to bridge %q: %w", tapName, bridgeName, err)
	}
	if isolate {
		// Isolated bridge ports can only talk to the bridge itself, i.e. the
		// host/uplink, never to another sandbox port (bridge-mode equivalent of
		// the eBPF default sandbox isolation policy).
		if err := netlink.LinkSetIsolated(tap, true); err != nil {
			return fmt.Errorf("network: isolate TAP %q: %w", tapName, err)
		}
	}
	if err := netlink.LinkSetUp(tap); err != nil {
		return fmt.Errorf("network: bring TAP %q up: %w", tapName, err)
	}
	return nil
}

// deleteLink removes a link; a missing link is not an error (idempotent).
func deleteLink(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		if linkNotFound(err) {
			return nil
		}
		return fmt.Errorf("network: look up link %q: %w", name, err)
	}
	if err := netlink.LinkDel(link); err != nil && !linkNotFound(err) {
		return fmt.Errorf("network: delete link %q: %w", name, err)
	}
	return nil
}

// addGatewayAddr adds the per-sandbox /30 gateway address to the bridge. The
// address may already exist (adoption); EEXIST is treated as success.
func addGatewayAddr(bridgeName string, gateway net.IP, prefix int) error {
	br, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return fmt.Errorf("network: look up bridge %q: %w", bridgeName, err)
	}
	addr := &netlink.Addr{IPNet: &net.IPNet{IP: gateway, Mask: net.CIDRMask(prefix, 32)}}
	if err := netlink.AddrAdd(br, addr); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("network: add gateway %s to bridge %q: %w", addr, bridgeName, err)
	}
	return nil
}

// removeGatewayAddr removes a per-sandbox /30 gateway address (idempotent).
func removeGatewayAddr(bridgeName string, gateway net.IP, prefix int) error {
	br, err := netlink.LinkByName(bridgeName)
	if err != nil {
		if linkNotFound(err) {
			return nil
		}
		return fmt.Errorf("network: look up bridge %q: %w", bridgeName, err)
	}
	addr := &netlink.Addr{IPNet: &net.IPNet{IP: gateway, Mask: net.CIDRMask(prefix, 32)}}
	if err := netlink.AddrDel(br, addr); err != nil {
		// EADDRNOTAVAIL / ENOENT: already gone.
		if errors.Is(err, unix.EADDRNOTAVAIL) || errors.Is(err, unix.ENOENT) || linkNotFound(err) {
			return nil
		}
		return fmt.Errorf("network: remove gateway %s from bridge %q: %w", addr, bridgeName, err)
	}
	return nil
}

// ensureStaticNeighbor installs a permanent bridge FDB/ARP entry mapping the
// guest IP to the guest MAC, so the host can deliver frames to a sleeping
// sandbox without waiting for an ARP reply (docs/DESIGN.md §5.5).
func ensureStaticNeighbor(bridgeName string, ip net.IP, mac net.HardwareAddr) error {
	br, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return fmt.Errorf("network: look up bridge %q: %w", bridgeName, err)
	}
	neigh := &netlink.Neigh{
		LinkIndex:    br.Attrs().Index,
		Family:       unix.AF_INET,
		State:        netlink.NUD_PERMANENT,
		IP:           ip,
		HardwareAddr: mac,
	}
	if err := netlink.NeighSet(neigh); err != nil {
		return fmt.Errorf("network: install static neighbor %s -> %s: %w", ip, mac, err)
	}
	return nil
}

// removeStaticNeighbor removes the permanent neighbor entry (idempotent).
func removeStaticNeighbor(bridgeName string, ip net.IP) error {
	br, err := netlink.LinkByName(bridgeName)
	if err != nil {
		if linkNotFound(err) {
			return nil
		}
		return fmt.Errorf("network: look up bridge %q: %w", bridgeName, err)
	}
	neigh := &netlink.Neigh{
		LinkIndex: br.Attrs().Index,
		Family:    unix.AF_INET,
		State:     netlink.NUD_PERMANENT,
		IP:        ip,
	}
	if err := netlink.NeighDel(neigh); err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EADDRNOTAVAIL) || linkNotFound(err) {
			return nil
		}
		return fmt.Errorf("network: remove static neighbor %s: %w", ip, err)
	}
	return nil
}

// linkNetStats returns kernel counters for a TAP. The TAP is viewed from the
// console side: packets the host delivers to the guest are Rx for the guest
// (they are the TAP's Tx), and packets the guest emits are the TAP's Rx.
func linkNetStats(tapName string) (types.NetStats, error) {
	link, err := netlink.LinkByName(tapName)
	if err != nil {
		if linkNotFound(err) {
			return types.NetStats{}, fmt.Errorf("%w: %s", ErrNotFound, tapName)
		}
		return types.NetStats{}, fmt.Errorf("network: look up TAP %q: %w", tapName, err)
	}
	st := link.Attrs().Statistics
	if st == nil {
		return types.NetStats{}, nil
	}
	return types.NetStats{
		RxBytes:   st.TxBytes,
		TxBytes:   st.RxBytes,
		RxPackets: st.TxPackets,
		TxPackets: st.RxPackets,
		Drops:     st.RxDropped + st.TxDropped,
	}, nil
}

// detectUplink returns the interface used by the default route, or "".
func detectUplink() string {
	routes, err := netlink.RouteGet(net.ParseIP("1.1.1.1"))
	if err != nil || len(routes) == 0 {
		return ""
	}
	idx := routes[0].LinkIndex
	if idx == 0 {
		return ""
	}
	link, err := netlink.LinkByIndex(idx)
	if err != nil {
		return ""
	}
	return link.Attrs().Name
}

// prepareNetns makes host-local port publishing work: the loopback interface
// must be up (fresh network namespaces start with lo DOWN), and
// route_localnet lets a DNAT'ed packet with a 127.0.0.1 source leave through
// the bridge. requireLocalnet is true when the sandbox publishes host ports.
func prepareNetns(requireLocalnet bool) error {
	lo, err := netlink.LinkByName("lo")
	if err == nil {
		if err := netlink.LinkSetUp(lo); err != nil {
			return fmt.Errorf("network: bring up loopback: %w", err)
		}
	} else if requireLocalnet {
		return fmt.Errorf("network: loopback interface: %w", err)
	}
	if requireLocalnet {
		if err := enableRouteLocalnet(); err != nil {
			return err
		}
	}
	return nil
}

func enableRouteLocalnet() error {
	const path = "/proc/sys/net/ipv4/conf/all/route_localnet"
	if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) == "1" {
		return nil
	}
	if err := os.WriteFile(path, []byte("1"), 0o644); err != nil {
		return fmt.Errorf("network: enable route_localnet: %w", err)
	}
	return nil
}

// ensureVethPair creates (or reuses) a veth pair with the given ends in the
// current network namespace and returns both links. Both ends are brought up;
// callers attach the bridge side as needed.
func ensureVethPair(portName, peerName string) (netlink.Link, netlink.Link, error) {
	if err := validateIfaceName(portName); err != nil {
		return nil, nil, err
	}
	if err := validateIfaceName(peerName); err != nil {
		return nil, nil, err
	}
	port, portErr := netlink.LinkByName(portName)
	peer, peerErr := netlink.LinkByName(peerName)
	switch {
	case portErr == nil && peerErr == nil:
		if _, ok := port.(*netlink.Veth); !ok {
			return nil, nil, fmt.Errorf("network: %q exists but is not a veth", portName)
		}
		if _, ok := peer.(*netlink.Veth); !ok {
			return nil, nil, fmt.Errorf("network: %q exists but is not a veth", peerName)
		}
		return port, peer, nil
	case portErr == nil || peerErr == nil:
		return nil, nil, fmt.Errorf("network: incomplete veth pair %q/%q", portName, peerName)
	}
	veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: portName}, PeerName: peerName}
	if err := netlink.LinkAdd(veth); err != nil {
		return nil, nil, fmt.Errorf("network: create veth %q/%q: %w", portName, peerName, err)
	}
	port, err := netlink.LinkByName(portName)
	if err != nil {
		return nil, nil, fmt.Errorf("network: look up veth %q: %w", portName, err)
	}
	peer, err = netlink.LinkByName(peerName)
	if err != nil {
		return nil, nil, fmt.Errorf("network: look up veth peer %q: %w", peerName, err)
	}
	if err := netlink.LinkSetUp(port); err != nil {
		return nil, nil, fmt.Errorf("network: bring up veth %q: %w", portName, err)
	}
	if err := netlink.LinkSetUp(peer); err != nil {
		return nil, nil, fmt.Errorf("network: bring up veth %q: %w", peerName, err)
	}
	return port, peer, nil
}

// configureTAPDirect sets a TAP's MAC and brings it up without attaching it to
// a bridge. eBPF mode uses the TAP itself as a switch port.
func configureTAPDirect(tapName string, mac net.HardwareAddr) error {
	tap, err := netlink.LinkByName(tapName)
	if err != nil {
		return fmt.Errorf("network: look up TAP %q: %w", tapName, err)
	}
	// Upgrade/adoption path: a TAP created by the bridge backend may still be
	// enslaved to the old shared bridge. Detach it so it can act as a switch port.
	if tap.Attrs().MasterIndex != 0 {
		if err := netlink.LinkSetNoMaster(tap); err != nil {
			return fmt.Errorf("network: detach TAP %q from bridge: %w", tapName, err)
		}
	}
	if len(mac) > 0 && tap.Attrs().HardwareAddr.String() != mac.String() {
		if err := netlink.LinkSetHardwareAddr(tap, mac); err != nil {
			return fmt.Errorf("network: set TAP %q MAC: %w", tapName, err)
		}
	}
	if err := netlink.LinkSetUp(tap); err != nil {
		return fmt.Errorf("network: bring TAP %q up: %w", tapName, err)
	}
	return nil
}
