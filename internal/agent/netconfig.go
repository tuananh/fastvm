//go:build linux

package agent

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	// GuestIface is the virtio-net interface name Firecracker exposes.
	GuestIface = "eth0"

	// CmdlineIPKey and CmdlineGWKey carry the guest NIC configuration on the
	// kernel command line, e.g. "fastvm.ip=10.77.0.2/30 fastvm.gw=10.77.0.1".
	CmdlineIPKey = "fastvm.ip"
	CmdlineGWKey = "fastvm.gw"

	cmdlineTokenPrefix = "fastvm."
)

// GuestNetworkConfig is the guest NIC configuration supplied by the daemon via
// the kernel command line.
type GuestNetworkConfig struct {
	Address   net.IP // IPv4 address
	PrefixLen int    // CIDR prefix length, 0..32
	Gateway   net.IP // IPv4 gateway; nil means no default route
}

// ParseCmdlineNetwork extracts fastvm.ip=<ip>/<prefix> and fastvm.gw=<ip> from
// a kernel command line. It returns (nil, nil) when neither key is present, so
// callers can treat guest networking as optional.
func ParseCmdlineNetwork(cmdline string) (*GuestNetworkConfig, error) {
	var cfg GuestNetworkConfig
	haveIP := false
	for _, field := range strings.Fields(cmdline) {
		key, value, ok := strings.Cut(field, "=")
		if !ok || !strings.HasPrefix(key, cmdlineTokenPrefix) {
			continue
		}
		switch key {
		case CmdlineIPKey:
			ip, prefixLen, err := parseIPPrefix(value)
			if err != nil {
				return nil, fmt.Errorf("kernel cmdline %s=%s: %w", key, value, err)
			}
			cfg.Address, cfg.PrefixLen, haveIP = ip, prefixLen, true
		case CmdlineGWKey:
			gw := net.ParseIP(value)
			if gw == nil || gw.To4() == nil {
				return nil, fmt.Errorf("kernel cmdline %s=%s: invalid IPv4 address", key, value)
			}
			cfg.Gateway = gw.To4()
		}
	}
	if !haveIP {
		if cfg.Gateway != nil {
			return nil, fmt.Errorf("kernel cmdline has %s but no %s", CmdlineGWKey, CmdlineIPKey)
		}
		return nil, nil
	}
	return &cfg, nil
}

func parseIPPrefix(value string) (net.IP, int, error) {
	ipPart, prefixPart, ok := strings.Cut(value, "/")
	if !ok {
		return nil, 0, errors.New("missing /<prefix>")
	}
	ip := net.ParseIP(ipPart)
	if ip == nil || ip.To4() == nil {
		return nil, 0, fmt.Errorf("invalid IPv4 address %q", ipPart)
	}
	prefixLen, err := strconv.Atoi(prefixPart)
	if err != nil || prefixLen < 0 || prefixLen > 32 {
		return nil, 0, fmt.Errorf("invalid prefix length %q", prefixPart)
	}
	return ip.To4(), prefixLen, nil
}

// ConfigureGuestNetwork brings iface up and assigns cfg.Address/cfg.PrefixLen,
// then installs a default route via cfg.Gateway when set. Values that are
// already configured are left alone, so the call is idempotent across agent
// restarts. A nil cfg is a no-op. Pure ioctls/netlink; no external tools.
func ConfigureGuestNetwork(iface string, cfg *GuestNetworkConfig) error {
	if cfg == nil {
		return nil
	}
	if iface == "" {
		return errors.New("agent: guest network: empty interface name")
	}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("agent: guest network: socket: %w", err)
	}
	defer unix.Close(fd)

	current, err := interfaceAddr(fd, iface)
	if err != nil {
		return err
	}
	want := net.IPv4(cfg.Address[0], cfg.Address[1], cfg.Address[2], cfg.Address[3])
	if current == nil || !current.Equal(want) {
		if err := setInterfaceAddr(fd, iface, unix.SIOCSIFADDR, want); err != nil {
			return err
		}
		mask := net.CIDRMask(cfg.PrefixLen, 32)
		if err := setInterfaceAddr(fd, iface, unix.SIOCSIFNETMASK, net.IP(mask)); err != nil {
			return err
		}
	}
	if err := bringInterfaceUp(fd, iface); err != nil {
		return err
	}
	if cfg.Gateway != nil {
		if err := addDefaultRoute(fd, iface, cfg.Gateway); err != nil {
			return err
		}
	}
	return nil
}

// interfaceAddr returns the current IPv4 address of iface, or nil when none is
// assigned.
func interfaceAddr(fd int, iface string) (net.IP, error) {
	ifr, err := unix.NewIfreq(iface)
	if err != nil {
		return nil, fmt.Errorf("agent: guest network: interface %q: %w", iface, err)
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFADDR, ifr); err != nil {
		if errors.Is(err, unix.EADDRNOTAVAIL) {
			return nil, nil
		}
		return nil, fmt.Errorf("agent: guest network: read %s address: %w", iface, err)
	}
	addr, err := ifr.Inet4Addr()
	if err != nil {
		return nil, fmt.Errorf("agent: guest network: read %s address: %w", iface, err)
	}
	return net.IPv4(addr[0], addr[1], addr[2], addr[3]), nil
}

func setInterfaceAddr(fd int, iface string, req uint, addr net.IP) error {
	ifr, err := unix.NewIfreq(iface)
	if err != nil {
		return fmt.Errorf("agent: guest network: interface %q: %w", iface, err)
	}
	ip4 := addr.To4()
	if ip4 == nil {
		return fmt.Errorf("agent: guest network: %v is not IPv4", addr)
	}
	if err := ifr.SetInet4Addr(ip4); err != nil {
		return fmt.Errorf("agent: guest network: set %s address: %w", iface, err)
	}
	if err := unix.IoctlIfreq(fd, req, ifr); err != nil {
		return fmt.Errorf("agent: guest network: set %s address %s: %w", iface, ip4, err)
	}
	return nil
}

func bringInterfaceUp(fd int, iface string) error {
	ifr, err := unix.NewIfreq(iface)
	if err != nil {
		return fmt.Errorf("agent: guest network: interface %q: %w", iface, err)
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return fmt.Errorf("agent: guest network: read %s flags: %w", iface, err)
	}
	flags := ifr.Uint16()
	if flags&unix.IFF_UP != 0 && flags&unix.IFF_RUNNING != 0 {
		return nil
	}
	ifr.SetUint16(flags | unix.IFF_UP | unix.IFF_RUNNING)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		return fmt.Errorf("agent: guest network: bring %s up: %w", iface, err)
	}
	return nil
}

// addDefaultRoute installs (or replaces) the IPv4 default route via gw using a
// minimal NETLINK_ROUTE message. It uses NLM_F_CREATE|NLM_F_REPLACE so repeated
// calls are harmless.
func addDefaultRoute(fd int, iface string, gw net.IP) error {
	ifr, err := unix.NewIfreq(iface)
	if err != nil {
		return fmt.Errorf("agent: guest network: interface %q: %w", iface, err)
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFINDEX, ifr); err != nil {
		return fmt.Errorf("agent: guest network: read %s index: %w", iface, err)
	}
	seq := uint32(1)
	msg, err := buildDefaultRouteRequest(seq, ifr.Uint32(), gw)
	if err != nil {
		return err
	}
	return sendNetlinkRoute(msg, seq)
}

// netlink header sizes for a RTM_NEWROUTE request.
const (
	nlmsgHdrLen = 16
	rtmsgLen    = 12
	rtattrHdr   = 4
)

// buildDefaultRouteRequest encodes an RTM_NEWROUTE request for the IPv4 default
// route via gw on ifindex.
func buildDefaultRouteRequest(seq uint32, ifindex uint32, gw net.IP) ([]byte, error) {
	gw4 := gw.To4()
	if gw4 == nil {
		return nil, fmt.Errorf("agent: guest network: gateway %v is not IPv4", gw)
	}
	var oif [4]byte
	binary.LittleEndian.PutUint32(oif[:], ifindex)

	attrs := make([]byte, 0, 2*(rtattrHdr+4))
	attrs = appendRouteAttr(attrs, syscall.RTA_GATEWAY, gw4)
	attrs = appendRouteAttr(attrs, syscall.RTA_OIF, oif[:])

	total := nlmsgHdrLen + rtmsgLen + len(attrs)
	msg := make([]byte, nlmsgHdrLen, total)
	binary.LittleEndian.PutUint32(msg[0:4], uint32(total)) // nlmsg_len
	binary.LittleEndian.PutUint16(msg[4:6], syscall.RTM_NEWROUTE)
	binary.LittleEndian.PutUint16(msg[6:8], syscall.NLM_F_REQUEST|syscall.NLM_F_ACK|syscall.NLM_F_CREATE|syscall.NLM_F_REPLACE)
	binary.LittleEndian.PutUint32(msg[8:12], seq) // nlmsg_seq
	binary.LittleEndian.PutUint32(msg[12:16], 0)  // nlmsg_pid (kernel assigns)

	rtmsg := make([]byte, rtmsgLen)
	rtmsg[0] = syscall.AF_INET // rtm_family
	// rtm_dst_len, rtm_src_len, rtm_tos stay 0 for a default route.
	rtmsg[4] = syscall.RT_TABLE_MAIN
	rtmsg[5] = syscall.RTPROT_BOOT
	rtmsg[6] = syscall.RT_SCOPE_UNIVERSE
	rtmsg[7] = syscall.RTN_UNICAST
	msg = append(msg, rtmsg...)
	msg = append(msg, attrs...)
	return msg, nil
}

func appendRouteAttr(dst []byte, attrType uint16, data []byte) []byte {
	length := rtattrHdr + len(data)
	var hdr [rtattrHdr]byte
	binary.LittleEndian.PutUint16(hdr[0:2], uint16(length))
	binary.LittleEndian.PutUint16(hdr[2:4], attrType)
	dst = append(dst, hdr[:]...)
	dst = append(dst, data...)
	for (length)%4 != 0 {
		dst = append(dst, 0)
		length++
	}
	return dst
}

// sendNetlinkRoute sends one route request and waits for its ACK.
func sendNetlinkRoute(msg []byte, seq uint32) error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return fmt.Errorf("agent: guest network: netlink socket: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("agent: guest network: netlink bind: %w", err)
	}
	// Bound the ACK wait so a lost message cannot hang the agent.
	tv := unix.Timeval{Sec: 2, Usec: 0}
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		return fmt.Errorf("agent: guest network: netlink timeout: %w", err)
	}
	if err := unix.Sendto(fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("agent: guest network: netlink send: %w", err)
	}
	buf := make([]byte, 4096)
	for {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			return fmt.Errorf("agent: guest network: netlink recv: %w", err)
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return fmt.Errorf("agent: guest network: netlink parse: %w", err)
		}
		for _, m := range msgs {
			if m.Header.Seq != seq {
				continue
			}
			switch m.Header.Type {
			case syscall.NLMSG_ERROR:
				if len(m.Data) < 4 {
					return errors.New("agent: guest network: short netlink error message")
				}
				errno := int32(binary.LittleEndian.Uint32(m.Data[:4]))
				if errno == 0 {
					return nil // ACK
				}
				if syscall.Errno(-errno) == syscall.EEXIST {
					return nil // already configured
				}
				return fmt.Errorf("agent: guest network: add default route: %w", syscall.Errno(-errno))
			case syscall.NLMSG_DONE:
				return nil
			}
		}
	}
}
