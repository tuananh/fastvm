package l2switch

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// TestEBPFAttachGated is the end-to-end eBPF data-plane test: it creates veths
// in the current network namespace, attaches the real tc program, and verifies
// redirect, default sandbox isolation, source-MAC anti-spoofing and ringbuf
// wake events.
//
// It skips unless the process has CAP_BPF/CAP_SYS_ADMIN *and* CAP_NET_ADMIN in
// the initial user namespace (a normal developer shell has neither). It is
// intended to be run manually on a privileged host, e.g.:
//
//	CGO_ENABLED=0 go test -c -o /tmp/l2switch.test ./internal/switch/
//	docker run --rm --privileged -v /tmp/l2switch.test:/l2switch.test \
//	    busybox /l2switch.test -test.run '^TestEBPFAttachGated$' -test.v
//
// It is not part of the default test suite and no test depends on Docker.
func TestEBPFAttachGated(t *testing.T) {
	if err := Probe(); err != nil {
		t.Skipf("eBPF unavailable: %v", err)
	}
	if !hasCapNetAdmin() {
		t.Skip("CAP_NET_ADMIN required to create test veths")
	}

	ctx := context.Background()
	const (
		sb0Port = "fvta0"
		sb0Peer = "fvta0p"
		sb1Port = "fvtb0"
		sb1Peer = "fvtb0p"
		upPort  = "fvtu0"
		upPeer  = "fvtu0p"
	)
	sb0MAC := net.HardwareAddr{0x02, 0xaa, 0x00, 0x00, 0x00, 0x01}
	sb1MAC := net.HardwareAddr{0x02, 0xaa, 0x00, 0x00, 0x00, 0x02}
	hostMAC := net.HardwareAddr{0x02, 0xaa, 0x00, 0x00, 0x00, 0xff}

	sb0 := mustVeth(t, sb0Port, sb0Peer)
	sb1 := mustVeth(t, sb1Port, sb1Peer)
	up := mustVeth(t, upPort, upPeer)
	mustSetMAC(t, sb0Port, sb0MAC)
	mustSetMAC(t, sb1Port, sb1MAC)

	sw, err := Open(Config{
		Mode:             ModeEBPF,
		IsolateSandboxes: true,
		WakeNotify:       true,
		UplinkIfIndex:    up.Attrs().Index,
	})
	if err != nil {
		t.Fatalf("Open eBPF: %v", err)
	}
	t.Cleanup(func() { _ = sw.Close() })

	for _, p := range []Port{
		{SandboxID: "sb0", IfIndex: sb0.Attrs().Index, MAC: sb0MAC, IsSandbox: true},
		{SandboxID: "sb1", IfIndex: sb1.Attrs().Index, MAC: sb1MAC, IsSandbox: true},
		{SandboxID: "uplink", IfIndex: up.Attrs().Index, MAC: hostMAC, IsSandbox: false},
	} {
		if err := sw.AddPort(ctx, p); err != nil {
			t.Fatalf("AddPort(%s): %v", p.SandboxID, err)
		}
	}
	if err := sw.SetAsleep(ctx, "sb0", true); err != nil {
		t.Fatalf("SetAsleep(sb0): %v", err)
	}

	// --- wake: uplink -> sleeping sandbox port --------------------------
	ifFrame := ipv4TCPFrame(sb0MAC, hostMAC, 0x02 /* SYN */, 0)
	upSock := bindPacket(t, upPeer)
	sb0PeerSock := bindPacket(t, sb0Peer)
	sendFrame(t, upPeer, ifFrame)
	ev := waitSwitchWake(t, sw, 3*time.Second)
	if ev.SandboxID != "sb0" {
		t.Errorf("wake event for %q, want sb0", ev.SandboxID)
	}
	if got := recvFrameMatching(t, sb0PeerSock, sb0MAC, 2*time.Second); got == nil {
		t.Error("frame for sleeping sandbox was not redirected into its port")
	}
	_ = upSock

	stats, err := sw.Stats(ctx, "sb0")
	if err != nil {
		t.Fatalf("Stats(sb0): %v", err)
	}
	if stats.TxPackets == 0 {
		t.Errorf("sb0 tx counters not incremented: %+v", stats)
	}

	// --- default isolation: sandbox -> sandbox must be dropped ----------
	if err := sw.SetAsleep(ctx, "sb0", false); err != nil {
		t.Fatalf("SetAsleep(sb0,false): %v", err)
	}
	drainSwitchWake(sw)
	_ = bindPacket(t, sb1Peer) // keep the peer socket open like a veth peer owner
	drainSwitchWake(sw)
	sendFrame(t, sb1Peer, ipv4TCPFrame(sb0MAC, sb1MAC, 0x10 /* ACK */, 0))
	if ev := recvFrameMatching(t, sb0PeerSock, sb0MAC, 400*time.Millisecond); ev != nil {
		t.Error("sandbox-to-sandbox frame was forwarded despite isolation")
	}
	stats1, err := sw.Stats(ctx, "sb1")
	if err != nil {
		t.Fatalf("Stats(sb1): %v", err)
	}
	if stats1.Drops == 0 {
		t.Errorf("isolation drop not counted on sb1: %+v", stats1)
	}

	// --- anti-spoof: sb1 sending with sb0's source MAC is dropped -------
	before, _ := sw.Stats(ctx, "sb1")
	sendFrame(t, sb1Peer, ipv4TCPFrame(hostMAC, sb0MAC, 0x10, 0))
	time.Sleep(150 * time.Millisecond)
	after, _ := sw.Stats(ctx, "sb1")
	if after.Drops <= before.Drops {
		t.Errorf("spoofed source MAC not counted as a drop: before=%+v after=%+v", before, after)
	}

	// --- unknown destination from a sandbox goes to the uplink ----------
	upPeerSock := bindPacket(t, upPeer)
	unknownMAC := net.HardwareAddr{0x02, 0xaa, 0x00, 0x00, 0x00, 0x99}
	sendFrame(t, sb1Peer, ipv4TCPFrame(unknownMAC, sb1MAC, 0x10, 4))
	if ev := recvFrameMatching(t, upPeerSock, unknownMAC, 2*time.Second); ev == nil {
		t.Error("frame with unknown destination was not redirected to the uplink")
	}

	if err := sw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := sw.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// hasCapNetAdmin reports CAP_NET_ADMIN in the effective capability set.
func hasCapNetAdmin() bool {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "CapEff:") {
			continue
		}
		var caps uint64
		if _, err := fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(line, "CapEff:")), "%x", &caps); err != nil {
			return false
		}
		return caps&(1<<12) != 0
	}
	return false
}

// mustVeth creates an up veth pair and returns the host-side link.
func mustVeth(t *testing.T, name, peer string) netlink.Link {
	t.Helper()
	attrs := netlink.NewLinkAttrs()
	attrs.Name = name
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: attrs, PeerName: peer}); err != nil {
		t.Fatalf("create veth %s/%s: %v", name, peer, err)
	}
	link, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("look up %s: %v", name, err)
	}
	peerLink, err := netlink.LinkByName(peer)
	if err != nil {
		t.Fatalf("look up %s: %v", peer, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("up %s: %v", name, err)
	}
	if err := netlink.LinkSetUp(peerLink); err != nil {
		t.Fatalf("up %s: %v", peer, err)
	}
	return link
}

func mustSetMAC(t *testing.T, name string, mac net.HardwareAddr) {
	t.Helper()
	link, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("look up %s: %v", name, err)
	}
	if err := netlink.LinkSetHardwareAddr(link, mac); err != nil {
		t.Fatalf("set MAC on %s: %v", name, err)
	}
}

// ipv4TCPFrame builds an Ethernet/IPv4/TCP frame.
func ipv4TCPFrame(dst, src net.HardwareAddr, flags byte, payload int) []byte {
	frame := make([]byte, 14+20+20+payload)
	copy(frame[0:6], dst)
	copy(frame[6:12], src)
	binary.BigEndian.PutUint16(frame[12:14], 0x0800)
	ip := frame[14:34]
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:4], uint16(20+20+payload))
	ip[8] = 64
	ip[9] = 6
	copy(ip[12:16], []byte{10, 0, 0, 1})
	copy(ip[16:20], []byte{10, 0, 0, 2})
	tcp := frame[34:54]
	binary.BigEndian.PutUint16(tcp[2:4], 80)
	tcp[12] = 5 << 4
	tcp[13] = flags
	for i := 54; i < len(frame); i++ {
		frame[i] = 'x'
	}
	return frame
}

// bindPacket opens a raw AF_PACKET socket bound to iface.
func bindPacket(t *testing.T, iface string) int {
	t.Helper()
	link, err := netlink.LinkByName(iface)
	if err != nil {
		t.Fatalf("look up %s: %v", iface, err)
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(htons16(unix.ETH_P_ALL)))
	if err != nil {
		t.Fatalf("AF_PACKET socket: %v", err)
	}
	sll := &unix.SockaddrLinklayer{Protocol: htons16(unix.ETH_P_ALL), Ifindex: link.Attrs().Index}
	if err := unix.Bind(fd, sll); err != nil {
		unix.Close(fd)
		t.Fatalf("bind AF_PACKET to %s: %v", iface, err)
	}
	tv := unix.NsecToTimeval(int64(500 * time.Millisecond))
	_ = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
	t.Cleanup(func() { unix.Close(fd) })
	return fd
}

// sendFrame injects an Ethernet frame on iface.
func sendFrame(t *testing.T, iface string, frame []byte) {
	t.Helper()
	link, err := netlink.LinkByName(iface)
	if err != nil {
		t.Fatalf("look up %s: %v", iface, err)
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(htons16(unix.ETH_P_ALL)))
	if err != nil {
		t.Fatalf("AF_PACKET socket: %v", err)
	}
	defer unix.Close(fd)
	sll := &unix.SockaddrLinklayer{
		Protocol: htons16(unix.ETH_P_ALL),
		Ifindex:  link.Attrs().Index,
		Halen:    6,
	}
	copy(sll.Addr[:], frame[0:6])
	if err := unix.Sendto(fd, frame, 0, sll); err != nil {
		t.Fatalf("send frame on %s: %v", iface, err)
	}
}

// recvFrameMatching reads until an Ethernet frame with the wanted destination
// MAC (and an IPv4 payload) arrives, ignoring IPv6 link-local noise and
// PACKET_OUTGOING copies of frames the test itself injected.
func recvFrameMatching(t *testing.T, fd int, wantDst net.HardwareAddr, timeout time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 1<<16)
	for time.Now().Before(deadline) {
		n, from, err := unix.Recvfrom(fd, buf, 0)
		if err != nil || n < 14 {
			continue
		}
		if sll, ok := from.(*unix.SockaddrLinklayer); ok && sll.Pkttype == unix.PACKET_OUTGOING {
			continue
		}
		frame := buf[:n]
		if binary.BigEndian.Uint16(frame[12:14]) != 0x0800 {
			continue
		}
		if net.HardwareAddr(frame[0:6]).String() != wantDst.String() {
			continue
		}
		return append([]byte(nil), frame...)
	}
	return nil
}

func waitSwitchWake(t *testing.T, sw Switch, timeout time.Duration) WakeEvent {
	t.Helper()
	select {
	case ev := <-sw.WakeEvents():
		return ev
	case <-time.After(timeout):
		t.Fatal("timed out waiting for eBPF ringbuf wake event")
		return WakeEvent{}
	}
}

func drainSwitchWake(sw Switch) {
	for {
		select {
		case <-sw.WakeEvents():
		default:
			return
		}
	}
}

func htons16(v uint16) uint16 { return v<<8 | v>>8 }
