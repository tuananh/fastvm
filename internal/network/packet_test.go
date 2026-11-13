package network

import (
	"encoding/binary"
	"net"
	"testing"
)

var (
	testGuestMAC = net.HardwareAddr{0x02, 0xaa, 0xbb, 0xcc, 0xdd, 0xee}
	testHostMAC  = net.HardwareAddr{0x02, 0x11, 0x22, 0x33, 0x44, 0x55}
	testBcastMAC = net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	testMultiMAC = net.HardwareAddr{0x01, 0x00, 0x5e, 0x00, 0x00, 0x01}
)

func ethFrame(dst, src net.HardwareAddr, etherType uint16) []byte {
	f := make([]byte, ethHeaderLen)
	copy(f[0:6], dst)
	copy(f[6:12], src)
	binary.BigEndian.PutUint16(f[12:14], etherType)
	return f
}

func ipv4Frame(dst, src net.HardwareAddr, protocol byte, l4 []byte, fragOff uint16) []byte {
	ip := make([]byte, 20)
	ip[0] = 0x45 // IPv4, IHL 5
	binary.BigEndian.PutUint16(ip[2:4], uint16(20+len(l4)))
	binary.BigEndian.PutUint16(ip[6:8], fragOff)
	ip[8] = 64 // TTL
	ip[9] = protocol
	f := ethFrame(dst, src, etherTypeIPv4)
	f = append(f, ip...)
	return append(f, l4...)
}

func tcpSegment(flags byte, payloadLen int) []byte {
	tcp := make([]byte, 20+payloadLen)
	binary.BigEndian.PutUint16(tcp[2:4], 443) // dst port
	tcp[12] = 5 << 4                          // data offset
	tcp[13] = flags
	for i := 20; i < len(tcp); i++ {
		tcp[i] = 0x41
	}
	return tcp
}

func udpDatagram(payloadLen int) []byte {
	udp := make([]byte, 8+payloadLen)
	binary.BigEndian.PutUint16(udp[2:4], 53) // dst port
	binary.BigEndian.PutUint16(udp[4:6], uint16(8+payloadLen))
	for i := 8; i < len(udp); i++ {
		udp[i] = 0x42
	}
	return udp
}

func TestClassifyTCP(t *testing.T) {
	const syn, ack, fin, rst = 0x02, 0x10, 0x01, 0x04
	tests := []struct {
		name      string
		flags     byte
		payload   int
		activity  bool
		keepalive bool
	}{
		{"syn", syn, 0, true, false},
		{"syn-ack", syn | ack, 0, true, false},
		{"fin", fin, 0, true, false},
		{"rst", rst, 0, true, false},
		{"pure-ack", ack, 0, false, true},
		{"data", ack, 10, true, false},
		{"data-with-fin", fin | ack, 3, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			frame := ipv4Frame(testGuestMAC, testHostMAC, 6, tcpSegment(tc.flags, tc.payload), 0)
			info := classifyFrame(frame, testGuestMAC)
			if !info.toGuest {
				t.Error("toGuest = false")
			}
			if info.activity != tc.activity || info.keepalive != tc.keepalive {
				t.Errorf("activity=%v keepalive=%v, want %v/%v", info.activity, info.keepalive, tc.activity, tc.keepalive)
			}
			if info.proto != "tcp" || info.dstPort != 443 {
				t.Errorf("proto=%q dstPort=%d", info.proto, info.dstPort)
			}
		})
	}
}

func TestClassifyUDP(t *testing.T) {
	for _, tc := range []struct {
		name      string
		payload   int
		activity  bool
		keepalive bool
	}{
		{"payload", 16, true, false},
		{"empty", 0, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame := ipv4Frame(testGuestMAC, testHostMAC, 17, udpDatagram(tc.payload), 0)
			info := classifyFrame(frame, testGuestMAC)
			if info.activity != tc.activity || info.keepalive != tc.keepalive {
				t.Errorf("activity=%v keepalive=%v", info.activity, info.keepalive)
			}
			if info.proto != "udp" || info.dstPort != 53 {
				t.Errorf("proto=%q dstPort=%d", info.proto, info.dstPort)
			}
		})
	}
}

func TestClassifyICMP(t *testing.T) {
	frame := ipv4Frame(testGuestMAC, testHostMAC, 1, []byte{8, 0, 0, 0}, 0)
	info := classifyFrame(frame, testGuestMAC)
	if !info.activity || info.proto != "icmp" {
		t.Errorf("ICMP classified as %+v", info)
	}
}

func TestClassifyNonWakeTraffic(t *testing.T) {
	arp := ethFrame(testGuestMAC, testHostMAC, etherTypeARP)
	arp = append(arp, make([]byte, 28)...)
	ipv6 := ethFrame(testGuestMAC, testHostMAC, etherTypeIPv6)
	ipv6 = append(ipv6, make([]byte, 40)...)
	broadcast := ipv4Frame(testBcastMAC, testHostMAC, 6, tcpSegment(0x02, 0), 0)
	multicast := ipv4Frame(testMultiMAC, testHostMAC, 6, tcpSegment(0x02, 0), 0)

	for name, frame := range map[string][]byte{
		"arp":       arp,
		"ipv6":      ipv6,
		"broadcast": broadcast,
		"multicast": multicast,
	} {
		info := classifyFrame(frame, testGuestMAC)
		if info.activity || info.keepalive {
			t.Errorf("%s: should be pure noise, got %+v", name, info)
		}
	}
	if !classifyFrame(arp, testGuestMAC).arp {
		t.Error("ARP frame not flagged as arp")
	}
}

func TestClassifyOtherUnicastMAC(t *testing.T) {
	other := net.HardwareAddr{0x02, 0xde, 0xad, 0xbe, 0xef, 0x01}
	frame := ipv4Frame(other, testHostMAC, 6, tcpSegment(0x10, 5), 0)
	info := classifyFrame(frame, testGuestMAC)
	if info.toGuest {
		t.Error("toGuest = true for frame addressed to another MAC")
	}
	// The L4 classification still applies (used for guest->host activity).
	if !info.activity {
		t.Error("activity = false for other-unicast TCP data frame")
	}
}

func TestClassifyFragments(t *testing.T) {
	frame := ipv4Frame(testGuestMAC, testHostMAC, 17, make([]byte, 8), 0x0001)
	info := classifyFrame(frame, testGuestMAC)
	if !info.activity {
		t.Error("non-first fragment should count as data in flight")
	}
}

func TestClassifyTruncatedFrames(t *testing.T) {
	for n := 0; n < 60; n++ {
		classifyFrame(make([]byte, n), testGuestMAC) // must not panic
	}
	// Truncated L4 header inside a valid IPv4 frame.
	frame := ipv4Frame(testGuestMAC, testHostMAC, 6, []byte{1}, 0)
	if info := classifyFrame(frame, testGuestMAC); info.activity || info.keepalive {
		t.Errorf("truncated TCP classified as %+v", info)
	}
}

func TestClassifyHostDirection(t *testing.T) {
	// Guest -> host frame (destination is the host MAC): still classifies for
	// activity accounting, but it is not a wake candidate.
	frame := ipv4Frame(testHostMAC, testGuestMAC, 6, tcpSegment(0x10, 100), 0)
	info := classifyFrame(frame, testGuestMAC)
	if info.toGuest {
		t.Error("toGuest = true for guest->host frame")
	}
	if !info.activity {
		t.Error("guest->host response should count as activity")
	}
}
