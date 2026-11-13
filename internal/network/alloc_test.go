package network

import (
	"strings"
	"testing"

	"fastvm/internal/types"
)

func mustAllocator(t *testing.T, cidr string) *allocator {
	t.Helper()
	a, err := newAllocator(cidr)
	if err != nil {
		t.Fatalf("newAllocator(%q): %v", cidr, err)
	}
	return a
}

func TestNewAllocatorRejectsBadPools(t *testing.T) {
	for _, cidr := range []string{"", "not-a-cidr", "10.0.0.0/31", "fd00::/64", "10.0.0.0/33"} {
		if _, err := newAllocator(cidr); err == nil {
			t.Errorf("newAllocator(%q) succeeded, want error", cidr)
		}
	}
}

func TestAllocatePointToPointBlocks(t *testing.T) {
	a := mustAllocator(t, "10.77.0.0/16")

	first, err := a.allocate("sb1", "", "")
	if err != nil {
		t.Fatalf("allocate sb1: %v", err)
	}
	if got, want := first.ip.String(), "10.77.0.2"; got != want {
		t.Errorf("sb1 ip = %s, want %s", got, want)
	}
	if got, want := first.gateway.String(), "10.77.0.1"; got != want {
		t.Errorf("sb1 gateway = %s, want %s", got, want)
	}
	if first.prefix != 30 {
		t.Errorf("sb1 prefix = %d, want 30", first.prefix)
	}
	if first.mac[0]&0x01 != 0 || first.mac[0]&0x02 == 0 {
		t.Errorf("sb1 MAC %s is not locally administered unicast", first.mac)
	}

	second, err := a.allocate("sb2", "", "")
	if err != nil {
		t.Fatalf("allocate sb2: %v", err)
	}
	if got, want := second.ip.String(), "10.77.0.6"; got != want {
		t.Errorf("sb2 ip = %s, want %s", got, want)
	}
	if got, want := second.gateway.String(), "10.77.0.5"; got != want {
		t.Errorf("sb2 gateway = %s, want %s", got, want)
	}
	if second.mac.String() == first.mac.String() {
		t.Errorf("sb1 and sb2 share MAC %s", second.mac)
	}
}

func TestAllocateDeterministicMACAndIfname(t *testing.T) {
	a := mustAllocator(t, "10.77.0.0/16")
	first, err := a.allocate("sandbox-abc", "", "")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	a.release("sandbox-abc")
	second, err := a.allocate("sandbox-abc", "", "")
	if err != nil {
		t.Fatalf("re-allocate: %v", err)
	}
	if first.mac.String() != second.mac.String() {
		t.Errorf("MAC not deterministic: %s then %s", first.mac, second.mac)
	}
	if first.tap != second.tap {
		t.Errorf("TAP name not deterministic: %s then %s", first.tap, second.tap)
	}
	if len(first.tap) > 15 {
		t.Errorf("TAP name %q longer than IFNAMSIZ", first.tap)
	}
}

func TestAllocateLongAndUnsafeSandboxIDs(t *testing.T) {
	a := mustAllocator(t, "10.77.0.0/16")
	for i, id := range []string{
		"a-very-long-sandbox-identifier-that-exceeds-ifnamsiz",
		"weird/id:with spaces",
		strings.Repeat("x", 200),
	} {
		alloc, err := a.allocate(id, "", "")
		if err != nil {
			t.Fatalf("allocate %d: %v", i, err)
		}
		if len(alloc.tap) > 15 || !ifaceNameRE.MatchString(alloc.tap) {
			t.Errorf("bad TAP name %q for id %q", alloc.tap, id)
		}
	}
	if len(a.list()) != 3 {
		t.Fatalf("expected 3 allocations, got %d", len(a.list()))
	}
}

func TestRequestedMACAndIPAreReused(t *testing.T) {
	a := mustAllocator(t, "10.77.0.0/16")
	first, err := a.allocate("sb1", "", "")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	wantMAC := first.mac.String()
	wantIP := first.ip.String()
	wantGateway := first.gateway.String()
	a.release("sb1")

	adopted, err := a.allocate("sb1-recovered", wantMAC, wantIP)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if adopted.mac.String() != wantMAC || adopted.ip.String() != wantIP || adopted.gateway.String() != wantGateway {
		t.Errorf("adoption changed allocation: %+v", adopted)
	}
}

func TestRequestedIPValidation(t *testing.T) {
	a := mustAllocator(t, "10.77.0.0/16")
	if _, err := a.allocate("sb1", "", "192.168.1.2"); err == nil {
		t.Error("requested IP outside pool accepted")
	}
	if _, err := a.allocate("sb1", "", "10.77.0.3"); err == nil {
		t.Error("requested non-guest-aligned IP accepted")
	}
	if _, err := a.allocate("sb1", "", "10.77.0.2"); err != nil {
		t.Fatalf("valid requested IP rejected: %v", err)
	}
	if _, err := a.allocate("sb2", "", "10.77.0.2"); err == nil {
		t.Error("duplicate requested IP accepted")
	}
}

func TestRequestedMACValidation(t *testing.T) {
	a := mustAllocator(t, "10.77.0.0/16")
	if _, err := a.allocate("sb1", "01:02:03:04:05:06", ""); err == nil {
		t.Error("multicast requested MAC accepted")
	}
	if _, err := a.allocate("sb2", "garbage", ""); err == nil {
		t.Error("invalid requested MAC accepted")
	}
	alloc, err := a.allocate("sb3", "02:11:22:33:44:55", "")
	if err != nil {
		t.Fatalf("valid requested MAC rejected: %v", err)
	}
	if alloc.mac.String() != "02:11:22:33:44:55" {
		t.Errorf("MAC = %s", alloc.mac)
	}
}

func TestAllocationExhaustion(t *testing.T) {
	a := mustAllocator(t, "10.77.0.0/30") // exactly one /30 block
	if _, err := a.allocate("sb1", "", ""); err != nil {
		t.Fatalf("first allocate: %v", err)
	}
	if _, err := a.allocate("sb2", "", ""); err == nil {
		t.Fatal("second allocate succeeded on exhausted pool")
	}
}

func TestDuplicateSandboxRejected(t *testing.T) {
	a := mustAllocator(t, "10.77.0.0/16")
	if _, err := a.allocate("sb1", "", ""); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if _, err := a.allocate("sb1", "", ""); err == nil {
		t.Fatal("duplicate sandbox accepted")
	}
}

func TestValidatePorts(t *testing.T) {
	valid := []types.PortMapping{
		{HostPort: 8080, GuestPort: 80, Proto: "tcp"},
		{HostPort: 5353, GuestPort: 53, Proto: "UDP"},
	}
	if err := validatePorts(valid); err != nil {
		t.Fatalf("valid ports rejected: %v", err)
	}
	bad := map[string][]types.PortMapping{
		"proto":       {{HostPort: 1, GuestPort: 1, Proto: "sctp"}},
		"host range":  {{HostPort: 0, GuestPort: 1, Proto: "tcp"}},
		"guest range": {{HostPort: 1, GuestPort: 70000, Proto: "tcp"}},
		"duplicate": {
			{HostPort: 80, GuestPort: 80, Proto: "tcp"},
			{HostPort: 80, GuestPort: 8080, Proto: "tcp"},
		},
	}
	for name, ports := range bad {
		if err := validatePorts(ports); err == nil {
			t.Errorf("%s: validatePorts accepted invalid mappings", name)
		}
	}
}
