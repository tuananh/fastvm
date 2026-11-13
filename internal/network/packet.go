package network

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"fastvm/internal/types"
)

// AF_PACKET wake/stats sniffer for the bridge backend (docs/DESIGN.md §5.5:
// "in switch.mode=bridge, a passive AF_PACKET tap supplies the same event").
//
// The sniffer is passive: it observes a copy of every frame crossing the TAP,
// it never consumes, delays or injects frames. While the port is asleep a
// qualifying unicast frame addressed to the guest emits a WakeEvent; the frame
// still flows into the guest's virtio-net queue.
//
// Direction on a TAP device (from the guest's point of view):
//   - PACKET_OUTGOING: the host/kernel transmits toward the guest -> Rx
//   - anything else: userspace (the VMM) wrote a guest frame -> Tx
//
// Keepalive classification mirrors bpf/l2switch.c so bridge and eBPF mode make
// identical activity/wake decisions.

const (
	ethHeaderLen  = 14
	etherTypeIPv4 = 0x0800
	etherTypeARP  = 0x0806
	etherTypeIPv6 = 0x86dd

	packetRecvBuffer  = 1 << 16
	packetRecvTimeout = 200 * time.Millisecond
)

// frameInfo is the result of classifying one Ethernet frame.
type frameInfo struct {
	toGuest   bool // unicast addressed to the guest MAC
	activity  bool // data / SYN / FIN / RST / UDP payload / ICMP
	keepalive bool // pure ACK or zero-payload datagram
	arp       bool
	proto     string // "tcp", "udp", "icmp" or ""
	dstPort   int
}

// classifyFrame parses an Ethernet frame and classifies its L4 payload for the
// idle predicate and the wake trigger. guestMAC is the sandbox's assigned MAC.
func classifyFrame(frame []byte, guestMAC net.HardwareAddr) frameInfo {
	var info frameInfo
	if len(frame) < ethHeaderLen {
		return info
	}
	// Broadcast/multicast frames are noise: they are forwarded but never count
	// as activity and never wake a sandbox (docs/DESIGN.md §5.3).
	if frame[0]&0x01 != 0 {
		return info
	}
	if bytes.Equal(frame[0:6], guestMAC) {
		info.toGuest = true
	}
	etherType := binary.BigEndian.Uint16(frame[12:14])
	switch etherType {
	case etherTypeARP:
		info.arp = true
		return info
	case etherTypeIPv6:
		// IPv6/NDP is out of scope for v0.1 and never wakes a sandbox.
		return info
	case etherTypeIPv4:
	default:
		return info
	}

	ip := frame[ethHeaderLen:]
	if len(ip) < 20 || ip[0]>>4 != 4 {
		return info
	}
	ihl := int(ip[0]&0x0f) * 4
	if ihl < 20 || len(ip) < ihl {
		return info
	}
	proto := ip[9]
	fragOff := binary.BigEndian.Uint16(ip[6:8])
	l4 := ip[ihl:]

	// Non-first fragments carry no L4 header; treat them as data in flight.
	if fragOff&0x1fff != 0 {
		info.activity = true
		return info
	}

	switch proto {
	case unix.IPPROTO_TCP:
		if len(l4) < 20 {
			return info
		}
		doff := int(l4[12]>>4) * 4
		if doff < 20 || len(l4) < doff {
			return info
		}
		info.proto = "tcp"
		info.dstPort = int(binary.BigEndian.Uint16(l4[2:4]))
		flags := l4[13]
		const syn, fin, rst = 0x02, 0x01, 0x04
		payload := len(l4) - doff
		info.activity = flags&(syn|fin|rst) != 0 || payload > 0
		info.keepalive = !info.activity
	case unix.IPPROTO_UDP:
		if len(l4) < 8 {
			return info
		}
		info.proto = "udp"
		info.dstPort = int(binary.BigEndian.Uint16(l4[2:4]))
		ulen := int(binary.BigEndian.Uint16(l4[4:6]))
		payload := ulen - 8
		if payload < 0 || payload > len(l4)-8 {
			payload = len(l4) - 8
		}
		info.activity = payload > 0
		info.keepalive = !info.activity
	case unix.IPPROTO_ICMP:
		info.proto = "icmp"
		info.activity = true
	}
	return info
}

// watcherConfig configures one per-sandbox AF_PACKET sniffer.
type watcherConfig struct {
	sandboxID      string
	ifindex        int
	mac            net.HardwareAddr
	keepaliveAware bool
	ignorePorts    map[int]bool
	wakeNotify     bool
	wake           chan<- WakeEvent
}

// packetWatcher is one sandbox's passive TAP sniffer.
type packetWatcher struct {
	cfg watcherConfig
	fd  int

	asleep  atomic.Bool
	closed  atomic.Bool
	done    chan struct{}
	closeMu sync.Mutex

	rxBytes        atomic.Uint64
	txBytes        atomic.Uint64
	rxPackets      atomic.Uint64
	txPackets      atomic.Uint64
	rxDrops        atomic.Uint64
	wakeDrops      atomic.Uint64
	activityFrames atomic.Uint64
	keepalive      atomic.Uint64
	lastAct        atomic.Int64
	lastFrame      atomic.Int64
}

// newPacketWatcher opens the AF_PACKET sampler and starts its read loop.
// Requires CAP_NET_RAW in the current network namespace.
func newPacketWatcher(cfg watcherConfig) (*packetWatcher, error) {
	if cfg.sandboxID == "" {
		return nil, errors.New("network: watcher requires a sandbox ID")
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(htons(unix.ETH_P_ALL)))
	if err != nil {
		return nil, fmt.Errorf("network: open AF_PACKET socket: %w (needs CAP_NET_RAW in the daemon network namespace)", err)
	}
	sll := &unix.SockaddrLinklayer{
		Protocol: htons(unix.ETH_P_ALL),
		Ifindex:  cfg.ifindex,
	}
	if err := unix.Bind(fd, sll); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("network: bind AF_PACKET to ifindex %d: %w", cfg.ifindex, err)
	}
	tv := unix.NsecToTimeval(int64(packetRecvTimeout))
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("network: set AF_PACKET timeout: %w", err)
	}
	w := &packetWatcher{cfg: cfg, fd: fd, done: make(chan struct{})}
	go w.run()
	return w, nil
}

// htons converts a host-order 16-bit value to network byte order.
func htons(v uint16) uint16 { return v<<8 | v>>8 }

func (w *packetWatcher) run() {
	defer close(w.done)
	buf := make([]byte, packetRecvBuffer)
	for {
		if w.closed.Load() {
			return
		}
		n, from, err := unix.Recvfrom(w.fd, buf, 0)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EINTR) {
				continue
			}
			// EBADF/ENETDOWN after Close: stop quietly. Other errors count as drops.
			if w.closed.Load() {
				return
			}
			w.rxDrops.Add(1)
			continue
		}
		if n <= 0 {
			continue
		}
		w.handleFrame(buf[:n], from)
	}
}

func (w *packetWatcher) handleFrame(frame []byte, from unix.Sockaddr) {
	pktType := uint8(unix.PACKET_HOST)
	if sll, ok := from.(*unix.SockaddrLinklayer); ok {
		pktType = sll.Pkttype
	}
	inbound := pktType == unix.PACKET_OUTGOING // host -> guest
	info := classifyFrame(frame, w.cfg.mac)

	now := time.Now().UnixNano()
	w.lastFrame.Store(now)
	if inbound {
		w.rxPackets.Add(1)
		w.rxBytes.Add(uint64(len(frame)))
	} else {
		w.txPackets.Add(1)
		w.txBytes.Add(uint64(len(frame)))
	}

	ignored := info.dstPort != 0 && w.cfg.ignorePorts[info.dstPort]
	var activity, keepalive bool
	if !ignored {
		if info.activity {
			activity = true
		} else if info.keepalive && !w.cfg.keepaliveAware {
			// keepalive_aware=false: pure ACKs and empty datagrams count as
			// activity, keeping chatty idle connections awake.
			activity = true
		} else if info.keepalive {
			keepalive = true
		}
	}
	if activity {
		w.activityFrames.Add(1)
		w.lastAct.Store(now)
	} else if keepalive {
		w.keepalive.Add(1)
	}

	// Wake only for a qualifying unicast frame delivered toward a sleeping
	// guest. ARP/NDP, broadcast and keepalive traffic never wake.
	if inbound && info.toGuest && activity && w.asleep.Load() && w.cfg.wakeNotify {
		ev := WakeEvent{SandboxID: w.cfg.sandboxID, Time: time.Unix(0, now)}
		select {
		case w.cfg.wake <- ev:
		default:
			w.wakeDrops.Add(1)
		}
	}
}

// setAsleep flips the wake gate.
func (w *packetWatcher) setAsleep(asleep bool) { w.asleep.Store(asleep) }

func (w *packetWatcher) isAsleep() bool { return w.asleep.Load() }

func (w *packetWatcher) activity() Activity {
	act := Activity{
		RxBytes:          w.rxBytes.Load(),
		TxBytes:          w.txBytes.Load(),
		RxPackets:        w.rxPackets.Load(),
		TxPackets:        w.txPackets.Load(),
		Drops:            w.rxDrops.Load() + w.wakeDrops.Load(),
		ActivityPackets:  w.activityFrames.Load(),
		KeepalivePackets: w.keepalive.Load(),
	}
	if ns := w.lastAct.Load(); ns > 0 {
		act.LastActivity = time.Unix(0, ns)
	}
	if ns := w.lastFrame.Load(); ns > 0 {
		act.LastFrame = time.Unix(0, ns)
	}
	return act
}

func (w *packetWatcher) netStats() types.NetStats {
	return types.NetStats{
		RxBytes:   w.rxBytes.Load(),
		TxBytes:   w.txBytes.Load(),
		RxPackets: w.rxPackets.Load(),
		TxPackets: w.txPackets.Load(),
		Drops:     w.rxDrops.Load() + w.wakeDrops.Load(),
	}
}

// close stops the read loop and releases the socket. It is idempotent.
func (w *packetWatcher) close() {
	w.closeMu.Lock()
	defer w.closeMu.Unlock()
	if w.closed.Swap(true) {
		return
	}
	unix.Close(w.fd)
	<-w.done
}
