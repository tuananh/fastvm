package agent

import (
	"encoding/binary"
	"net"
	"syscall"
	"testing"
)

func TestParseCmdlineNetwork(t *testing.T) {
	tests := []struct {
		name    string
		cmdline string
		want    *GuestNetworkConfig
		wantErr bool
	}{
		{
			name:    "no fastvm keys",
			cmdline: "console=ttyS0 root=/dev/vda rw init=/init",
		},
		{
			name:    "empty cmdline",
			cmdline: "",
		},
		{
			name:    "ip and gateway",
			cmdline: "console=ttyS0 root=/dev/vda rw init=/init fastvm.ip=10.77.0.2/30 fastvm.gw=10.77.0.1",
			want: &GuestNetworkConfig{
				Address:   net.IPv4(10, 77, 0, 2).To4(),
				PrefixLen: 30,
				Gateway:   net.IPv4(10, 77, 0, 1).To4(),
			},
		},
		{
			name:    "ip without gateway",
			cmdline: "fastvm.ip=192.168.7.5/24",
			want: &GuestNetworkConfig{
				Address:   net.IPv4(192, 168, 7, 5).To4(),
				PrefixLen: 24,
			},
		},
		{
			name:    "slash 32",
			cmdline: "fastvm.ip=10.0.0.1/32 fastvm.gw=10.0.0.2",
			want: &GuestNetworkConfig{
				Address:   net.IPv4(10, 0, 0, 1).To4(),
				PrefixLen: 32,
				Gateway:   net.IPv4(10, 0, 0, 2).To4(),
			},
		},
		{name: "gateway without ip", cmdline: "fastvm.gw=10.77.0.1", wantErr: true},
		{name: "ip without prefix", cmdline: "fastvm.ip=10.77.0.2", wantErr: true},
		{name: "bad ip", cmdline: "fastvm.ip=not-an-ip/30", wantErr: true},
		{name: "ipv6 rejected", cmdline: "fastvm.ip=fd00::2/64", wantErr: true},
		{name: "bad prefix", cmdline: "fastvm.ip=10.77.0.2/33", wantErr: true},
		{name: "bad gateway", cmdline: "fastvm.ip=10.77.0.2/30 fastvm.gw=nope", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseCmdlineNetwork(tc.cmdline)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCmdlineNetwork: %v", err)
			}
			if tc.want == nil {
				if got != nil {
					t.Fatalf("got %+v, want nil", got)
				}
				return
			}
			if !got.Address.Equal(tc.want.Address) || got.PrefixLen != tc.want.PrefixLen {
				t.Fatalf("address = %v/%d, want %v/%d", got.Address, got.PrefixLen, tc.want.Address, tc.want.PrefixLen)
			}
			if (got.Gateway == nil) != (tc.want.Gateway == nil) {
				t.Fatalf("gateway = %v, want %v", got.Gateway, tc.want.Gateway)
			}
			if got.Gateway != nil && !got.Gateway.Equal(tc.want.Gateway) {
				t.Fatalf("gateway = %v, want %v", got.Gateway, tc.want.Gateway)
			}
		})
	}
}

func TestConfigureGuestNetworkNilConfig(t *testing.T) {
	if err := ConfigureGuestNetwork(GuestIface, nil); err != nil {
		t.Fatalf("nil config must be a no-op: %v", err)
	}
	cfg := &GuestNetworkConfig{Address: net.IPv4(10, 0, 0, 2).To4(), PrefixLen: 30}
	if err := ConfigureGuestNetwork("", cfg); err == nil {
		t.Fatal("empty interface name should fail")
	}
}

func TestBuildDefaultRouteRequest(t *testing.T) {
	seq := uint32(7)
	ifindex := uint32(42)
	gw := net.IPv4(10, 77, 0, 1).To4()

	msg, err := buildDefaultRouteRequest(seq, ifindex, gw)
	if err != nil {
		t.Fatalf("buildDefaultRouteRequest: %v", err)
	}
	if got, want := int(binary.LittleEndian.Uint32(msg[0:4])), len(msg); got != want {
		t.Fatalf("nlmsg_len = %d, want %d", got, want)
	}
	if typ := binary.LittleEndian.Uint16(msg[4:6]); typ != syscall.RTM_NEWROUTE {
		t.Fatalf("nlmsg_type = %d", typ)
	}
	flags := binary.LittleEndian.Uint16(msg[6:8])
	for _, want := range []uint16{syscall.NLM_F_REQUEST, syscall.NLM_F_ACK, syscall.NLM_F_CREATE, syscall.NLM_F_REPLACE} {
		if flags&want == 0 {
			t.Fatalf("flags %#x missing %#x", flags, want)
		}
	}
	if got := binary.LittleEndian.Uint32(msg[8:12]); got != seq {
		t.Fatalf("nlmsg_seq = %d, want %d", got, seq)
	}

	rtmsg := msg[nlmsgHdrLen : nlmsgHdrLen+rtmsgLen]
	if rtmsg[0] != syscall.AF_INET || rtmsg[4] != syscall.RT_TABLE_MAIN ||
		rtmsg[5] != syscall.RTPROT_BOOT || rtmsg[6] != syscall.RT_SCOPE_UNIVERSE ||
		rtmsg[7] != syscall.RTN_UNICAST {
		t.Fatalf("rtmsg = %v", rtmsg)
	}
	if rtmsg[1] != 0 || rtmsg[2] != 0 || rtmsg[3] != 0 {
		t.Fatalf("default route dst/src/tos must be zero: %v", rtmsg[:4])
	}

	attrs := msg[nlmsgHdrLen+rtmsgLen:]
	ok := false
	for len(attrs) >= rtattrHdr {
		length := int(binary.LittleEndian.Uint16(attrs[0:2]))
		typ := binary.LittleEndian.Uint16(attrs[2:4])
		if length < rtattrHdr || length > len(attrs) {
			t.Fatalf("bad rtattr length %d (remaining %d)", length, len(attrs))
		}
		data := attrs[rtattrHdr:length]
		switch typ {
		case syscall.RTA_GATEWAY:
			if net.IP(data).String() != gw.String() {
				t.Fatalf("RTA_GATEWAY = %v, want %v", net.IP(data), gw)
			}
			ok = true
		case syscall.RTA_OIF:
			if got := binary.LittleEndian.Uint32(data); got != ifindex {
				t.Fatalf("RTA_OIF = %d, want %d", got, ifindex)
			}
		}
		// Attributes are aligned to 4 bytes.
		aligned := (length + 3) &^ 3
		attrs = attrs[aligned:]
	}
	if !ok {
		t.Fatal("RTA_GATEWAY attribute missing")
	}
}

func TestBuildDefaultRouteRequestRejectsIPv6(t *testing.T) {
	if _, err := buildDefaultRouteRequest(1, 1, net.ParseIP("fd00::1")); err == nil {
		t.Fatal("IPv6 gateway should be rejected")
	}
}
