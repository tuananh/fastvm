package network

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"fastvm/internal/types"
)

// Gated data-plane integration test.
//
// The initial namespaces on a developer box have no CAP_NET_ADMIN, so the
// parent test re-executes the compiled test binary inside `unshare -Urn
// --map-root-user` (a new user+network namespace where veth/bridge/TAP/nftables
// are available). Run it with:
//
//	FASTVM_NET_TESTS=1 go test ./internal/network/ -run TestManagerIntegration -v
//
// It skips cleanly when the env var is unset, when unshare is unavailable, or
// inside the child when the child marker is absent.

func TestManagerIntegration(t *testing.T) {
	if os.Getenv("FASTVM_NET_CHILD") == "1" {
		t.Skip("data-plane body runs in the child invocation")
	}
	if os.Getenv("FASTVM_NET_TESTS") != "1" {
		t.Skip("set FASTVM_NET_TESTS=1 to run the network data-plane integration test")
	}
	if hasCapNetAdmin() {
		testManagerDataPlane(t)
		return
	}
	unshare, err := exec.LookPath("unshare")
	if err != nil {
		t.Skipf("unshare not available and no CAP_NET_ADMIN: %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	cmd := exec.Command(unshare, "-Urn", "--map-root-user", exe,
		"-test.run=^TestManagerIntegrationChild$", "-test.v")
	cmd.Env = append(os.Environ(), "FASTVM_NET_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child under unshare failed: %v\n%s", err, out)
	}
	t.Logf("child output:\n%s", out)
}

func TestManagerIntegrationChild(t *testing.T) {
	if os.Getenv("FASTVM_NET_CHILD") != "1" {
		t.Skip("only runs inside the unshare re-exec")
	}
	testManagerDataPlane(t)
	testEBPFManagerTopology(t)
}

const (
	itBridge = "fvtest0"
	itCIDR   = "10.99.0.0/24"
)

func testManagerDataPlane(t *testing.T) {
	ctx := context.Background()
	mgr, err := NewManager(Options{
		Bridge:          itBridge,
		CIDR:            itCIDR,
		SwitchMode:      "bridge",
		Isolate:         true,
		WakeNotify:      true,
		NFTBin:          "nft",
		UplinkInterface: "-",
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	// --- Create with published ports -----------------------------------
	port, err := mgr.Create(ctx, CreateRequest{
		SandboxID: "it-sb1",
		Ports: []types.PortMapping{
			{HostPort: 18080, GuestPort: 80, Proto: "tcp"},
			{HostPort: 18081, GuestPort: 8080, Proto: "udp"},
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if port.SandboxID != "it-sb1" || port.TAPName == "" || port.MAC == "" {
		t.Fatalf("unexpected port: %+v", port)
	}
	if got, want := port.IP, "10.99.0.2"; got != want {
		t.Errorf("IP = %s, want %s", got, want)
	}
	if got, want := port.Gateway, "10.99.0.1"; got != want {
		t.Errorf("Gateway = %s, want %s", got, want)
	}
	if port.NetnsPath != "" {
		t.Logf("note: bridge mode returned NetnsPath %q (expected empty)", port.NetnsPath)
	}
	mac, err := net.ParseMAC(port.MAC)
	if err != nil {
		t.Fatalf("parse MAC: %v", err)
	}

	sh := &itShell{t: t}
	br := sh.link(itBridge)
	tap := sh.link(port.TAPName)
	if tap.Attrs().MasterIndex != br.Attrs().Index {
		t.Errorf("TAP master = %d, want bridge %d", tap.Attrs().MasterIndex, br.Attrs().Index)
	}
	if !sh.hasAddr(itBridge, port.Gateway+"/30") {
		t.Errorf("bridge %s missing gateway address %s/30", itBridge, port.Gateway)
	}
	if !sh.hasNeighbor(itBridge, port.IP, port.MAC) {
		t.Errorf("bridge %s missing permanent neighbor %s -> %s", itBridge, port.IP, port.MAC)
	}
	if tap.Attrs().HardwareAddr.String() != mac.String() {
		t.Errorf("TAP MAC = %s, want %s", tap.Attrs().HardwareAddr, mac)
	}

	// Stats and activity must work in bridge mode (needed by the idle logic).
	if _, err := mgr.Stats(ctx, "it-sb1"); err != nil {
		t.Errorf("Stats: %v", err)
	}
	if _, err := mgr.(ActivitySource).Activity(ctx, "it-sb1"); err != nil {
		t.Errorf("Activity: %v", err)
	}

	// --- nftables DNAT rules installed --------------------------------
	nftOut := sh.nftList()
	for _, want := range []string{
		"hook output",
		"ip saddr 127.0.0.1",
		"dnat to 10.99.0.2:80",
		"dnat to 10.99.0.2:8080",
		"tcp dport 18080",
		"udp dport 18081",
	} {
		if !strings.Contains(nftOut, want) {
			t.Errorf("nft rules missing %q:\n%s", want, nftOut)
		}
	}

	// Hold the TAP open like the VMM does; this brings the carrier up and makes
	// the bridge path usable, and the guest queue live.
	holdTAP(t, port.TAPName)
	time.Sleep(150 * time.Millisecond)

	// --- wake while asleep --------------------------------------------
	drainWake(mgr)
	if err := mgr.SetAsleep(ctx, "it-sb1", true); err != nil {
		t.Fatalf("SetAsleep(true): %v", err)
	}
	sendToGuest(t, port.IP, 4242, []byte("wake-fastvm-please"))
	ev := waitWake(t, mgr, 3*time.Second)
	if ev.SandboxID != "it-sb1" {
		t.Errorf("wake event for %q, want it-sb1", ev.SandboxID)
	}

	// --- keepalive-ish traffic while awake must not wake ---------------
	if err := mgr.SetAsleep(ctx, "it-sb1", false); err != nil {
		t.Fatalf("SetAsleep(false): %v", err)
	}
	drainWake(mgr)
	sendToGuest(t, port.IP, 4242, []byte("awake"))
	select {
	case ev := <-mgr.WakeEvents():
		t.Errorf("unexpected wake event while awake: %+v", ev)
	case <-time.After(400 * time.Millisecond):
	}

	act, err := mgr.(ActivitySource).Activity(ctx, "it-sb1")
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	if act.ActivityPackets == 0 {
		t.Errorf("activity counters did not record traffic: %+v", act)
	}
	if act.RxPackets == 0 || act.RxBytes == 0 {
		t.Errorf("rx counters empty after injected frames: %+v", act)
	}
	if act.LastActivity.IsZero() {
		t.Errorf("LastActivity not set: %+v", act)
	}
	stats, err := mgr.Stats(ctx, "it-sb1")
	if err != nil || stats.RxPackets == 0 {
		t.Errorf("Stats = %+v, err = %v", stats, err)
	}

	// --- Delete is idempotent and cleans the data plane ----------------
	if err := mgr.Delete(ctx, "it-sb1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := mgr.Delete(ctx, "it-sb1"); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if _, err := netlink.LinkByName(port.TAPName); err == nil {
		t.Errorf("TAP %s still present after Delete", port.TAPName)
	}
	if sh.hasAddr(itBridge, port.Gateway+"/30") {
		t.Errorf("gateway address %s still on bridge after Delete", port.Gateway)
	}
	if sh.hasNeighbor(itBridge, port.IP, port.MAC) {
		t.Errorf("neighbor %s still present after Delete", port.IP)
	}
	if out := sh.nftList(); strings.Contains(out, "10.99.0.2") {
		t.Errorf("DNAT rules still present after Delete:\n%s", out)
	}

	// --- Create without ports works with no DNAT or masquerade --------
	port2, err := mgr.Create(ctx, CreateRequest{SandboxID: "it-sb2"})
	if err != nil {
		t.Fatalf("Create without ports: %v", err)
	}
	if port2.IP != "10.99.0.6" || port2.Gateway != "10.99.0.5" {
		t.Errorf("second sandbox allocation = %s/%s, want 10.99.0.6/10.99.0.5", port2.IP, port2.Gateway)
	}
	if err := mgr.SetAsleep(ctx, "it-sb2", true); err != nil {
		t.Errorf("SetAsleep on second sandbox: %v", err)
	}
	if err := mgr.Delete(ctx, "it-sb2"); err != nil {
		t.Errorf("Delete second sandbox: %v", err)
	}
}

// itShell wraps privileged link/rule inspection for the child test.
type itShell struct{ t *testing.T }

func (s *itShell) link(name string) netlink.Link {
	s.t.Helper()
	link, err := netlink.LinkByName(name)
	if err != nil {
		s.t.Fatalf("link %q: %v", name, err)
	}
	return link
}

func (s *itShell) hasAddr(linkName, cidr string) bool {
	s.t.Helper()
	link := s.link(linkName)
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		s.t.Fatalf("list addrs on %q: %v", linkName, err)
	}
	for _, a := range addrs {
		if a.IPNet.String() == cidr {
			return true
		}
	}
	return false
}

func (s *itShell) hasNeighbor(linkName, ip, mac string) bool {
	s.t.Helper()
	link := s.link(linkName)
	neighs, err := netlink.NeighList(link.Attrs().Index, unix.AF_INET)
	if err != nil {
		s.t.Fatalf("list neighbors on %q: %v", linkName, err)
	}
	for _, n := range neighs {
		if n.IP.String() == ip && n.HardwareAddr.String() == mac && n.State&netlink.NUD_PERMANENT != 0 {
			return true
		}
	}
	return false
}

func (s *itShell) nftList() string {
	s.t.Helper()
	out, err := exec.Command("nft", "list", "table", "ip", "fastvm").CombinedOutput()
	if err != nil {
		// No fastvm table (e.g. after Delete) is the expected empty state.
		return ""
	}
	return string(out)
}

// holdTAP opens the persistent TAP the way the VMM does and keeps the fd for
// the duration of the test (the bridge port only forwards while it has a
// userspace owner).
func holdTAP(t *testing.T, name string) *os.File {
	t.Helper()
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open /dev/net/tun: %v", err)
	}
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		unix.Close(fd)
		t.Fatalf("ifreq %q: %v", name, err)
	}
	ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		unix.Close(fd)
		t.Fatalf("attach to TAP %q: %v", name, err)
	}
	f := os.NewFile(uintptr(fd), name)
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// sendToGuest sends a UDP datagram from the host stack through the bridge to
// the guest IP. The static neighbor entry means no ARP is needed.
func sendToGuest(t *testing.T, guestIP string, port int, payload []byte) {
	t.Helper()
	conn, err := net.Dial("udp", net.JoinHostPort(guestIP, fmt.Sprint(port)))
	if err != nil {
		t.Fatalf("dial guest %s: %v", guestIP, err)
	}
	defer conn.Close()
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("send to guest: %v", err)
	}
}

func drainWake(mgr Manager) {
	for {
		select {
		case <-mgr.WakeEvents():
		default:
			return
		}
	}
}

func waitWake(t *testing.T, mgr Manager, timeout time.Duration) WakeEvent {
	t.Helper()
	select {
	case ev, ok := <-mgr.WakeEvents():
		if !ok {
			t.Fatal("wake channel closed unexpectedly")
		}
		return ev
	case <-time.After(timeout):
		t.Fatal("timed out waiting for wake event")
		return WakeEvent{}
	}
}

// hasCapNetAdmin reports whether the current process has CAP_NET_ADMIN in its
// effective capability set.
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
		const capNetAdmin = 12
		return caps&(1<<capNetAdmin) != 0
	}
	return false
}

// testEBPFManagerTopology exercises the eBPF-mode network plumbing (TAP as a
// switch port plus the shared uplink veth pair) without loading the BPF program.
func testEBPFManagerTopology(t *testing.T) {
	ctx := context.Background()
	mgr, err := NewManager(Options{
		CIDR:            itCIDR,
		SwitchMode:      "ebpf",
		UplinkInterface: "-",
		Isolate:         true,
		WakeNotify:      true,
		NFTBin:          "nft",
	})
	if err != nil {
		t.Fatalf("NewManager(ebpf): %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	up, ok := mgr.(UplinkProvider)
	if !ok {
		t.Fatal("eBPF manager does not expose its uplink")
	}
	uplink, err := up.UplinkPort()
	if err != nil {
		t.Fatalf("UplinkPort: %v", err)
	}
	if uplink.IfIndex == 0 || uplink.Name == "" || uplink.MAC == "" {
		t.Fatalf("bad uplink: %+v", uplink)
	}
	if _, err := netlink.LinkByName(uplink.Name); err != nil {
		t.Fatalf("uplink link %q: %v", uplink.Name, err)
	}
	if _, err := netlink.LinkByName("fvup"); err != nil {
		t.Fatalf("host uplink side fvup: %v", err)
	}

	port, err := mgr.Create(ctx, CreateRequest{SandboxID: "it-ebpf1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if port.SwitchIfIndex == 0 {
		t.Fatal("eBPF port has no switch ifindex")
	}
	tap, err := netlink.LinkByName(port.TAPName)
	if err != nil {
		t.Fatalf("TAP %q: %v", port.TAPName, err)
	}
	if tap.Attrs().MasterIndex != 0 {
		t.Errorf("eBPF TAP is enslaved to bridge %d", tap.Attrs().MasterIndex)
	}
	if tap.Attrs().Index != port.SwitchIfIndex {
		t.Errorf("SwitchIfIndex = %d, want TAP index %d", port.SwitchIfIndex, tap.Attrs().Index)
	}
	if got := tap.Attrs().HardwareAddr.String(); got != port.MAC {
		t.Errorf("TAP MAC = %s, want %s", got, port.MAC)
	}

	// Gateway address and static neighbor live on the host uplink side.
	fvup, err := netlink.LinkByName("fvup")
	if err != nil {
		t.Fatal(err)
	}
	addrs, err := netlink.AddrList(fvup, unix.AF_INET)
	if err != nil {
		t.Fatal(err)
	}
	foundGW := false
	for _, a := range addrs {
		if a.IP.String() == port.Gateway {
			foundGW = true
		}
	}
	if !foundGW {
		t.Errorf("gateway %s not on fvup: %+v", port.Gateway, addrs)
	}
	neighs, err := netlink.NeighList(fvup.Attrs().Index, unix.AF_INET)
	if err != nil {
		t.Fatal(err)
	}
	foundNeigh := false
	for _, n := range neighs {
		if n.IP.String() == port.IP && n.HardwareAddr.String() == port.MAC {
			foundNeigh = true
		}
	}
	if !foundNeigh {
		t.Errorf("static neighbor %s -> %s not on fvup: %+v", port.IP, port.MAC, neighs)
	}

	if err := mgr.Delete(ctx, "it-ebpf1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := netlink.LinkByName(port.TAPName); err == nil {
		t.Errorf("TAP %q still exists after Delete", port.TAPName)
	}
}
