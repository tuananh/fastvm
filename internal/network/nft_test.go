package network

import (
	"strings"
	"testing"

	"fastvm/internal/types"
)

func TestRenderNftablesEmpty(t *testing.T) {
	body, err := renderNftables(nftSpec{Table: "fastvm", Bridge: "fastvm0", CIDR: "10.77.0.0/16"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if body != "" {
		t.Fatalf("expected empty rule set, got:\n%s", body)
	}
}

func TestRenderNftablesDNATAndMasquerade(t *testing.T) {
	body, err := renderNftables(nftSpec{
		Table:  "fastvm",
		Bridge: "fastvm0",
		CIDR:   "10.77.0.0/16",
		Uplink: "eth0",
		Mappings: []nftMapping{
			{GuestIP: "10.77.0.6", HostPort: 8443, GuestPort: 443, Proto: "tcp"},
			{GuestIP: "10.77.0.2", HostPort: 8080, GuestPort: 80, Proto: "tcp"},
			{GuestIP: "10.77.0.2", HostPort: 5353, GuestPort: 53, Proto: "udp"},
		},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	wants := []string{
		"table ip fastvm {",
		"type nat hook prerouting priority dstnat; policy accept;",
		"type nat hook output priority dstnat; policy accept;",
		`iifname != "fastvm0" tcp dport 8443 dnat to 10.77.0.6:443`,
		`iifname != "fastvm0" tcp dport 8080 dnat to 10.77.0.2:80`,
		`iifname != "fastvm0" udp dport 5353 dnat to 10.77.0.2:53`,
		"type nat hook postrouting priority srcnat; policy accept;",
		`ip saddr 10.77.0.0/16 oifname "eth0" masquerade`,
		`ip saddr 127.0.0.1 oifname "fastvm0" masquerade`,
	}
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("rendered rules missing %q:\n%s", want, body)
		}
	}
	// Deterministic ordering by host port.
	if strings.Index(body, "dport 8080") > strings.Index(body, "dport 8443") {
		t.Errorf("mappings not sorted by host port:\n%s", body)
	}
	// Every mapping is emitted twice: prerouting (external) + output (host-local).
	for _, dnat := range []string{"dnat to 10.77.0.6:443", "dnat to 10.77.0.2:80", "dnat to 10.77.0.2:53"} {
		if n := strings.Count(body, dnat); n != 2 {
			t.Errorf("DNAT %q emitted %d times, want 2 (prerouting+output):\n%s", dnat, n, body)
		}
	}
}

func TestRenderNftablesNoUplinkSkipsMasquerade(t *testing.T) {
	body, err := renderNftables(nftSpec{
		Table:    "fastvm",
		Bridge:   "fastvm0",
		CIDR:     "10.77.0.0/16",
		Mappings: []nftMapping{{GuestIP: "10.77.0.2", HostPort: 8080, GuestPort: 80, Proto: "tcp"}},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// Without an uplink the pool masquerade is absent, but host-local
	// published ports still need the loopback hairpin SNAT.
	if strings.Contains(body, "ip saddr 10.77.0.0/16") {
		t.Errorf("unexpected uplink masquerade rule without uplink:\n%s", body)
	}
	if !strings.Contains(body, `ip saddr 127.0.0.1 oifname "fastvm0" masquerade`) {
		t.Errorf("missing loopback hairpin SNAT:\n%s", body)
	}
	if !strings.Contains(body, "dport 8080") {
		t.Errorf("missing DNAT rule:\n%s", body)
	}
	if !strings.Contains(body, "hook output") {
		t.Errorf("missing host-local output DNAT hook:\n%s", body)
	}
}

func TestRenderNftablesOutputNeedsNoBridge(t *testing.T) {
	body, err := renderNftables(nftSpec{
		Table:    "fastvm",
		Mappings: []nftMapping{{GuestIP: "10.77.0.2", HostPort: 8080, GuestPort: 80, Proto: "tcp"}},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(body, "type nat hook output priority dstnat; policy accept;") {
		t.Fatalf("missing output hook without bridge:\n%s", body)
	}
	if !strings.Contains(body, "tcp dport 8080 dnat to 10.77.0.2:80") {
		t.Fatalf("missing output DNAT rule:\n%s", body)
	}
}

func TestRenderNftablesRejectsInjection(t *testing.T) {
	cases := map[string]nftSpec{
		"table":  {Table: "fastvm; flush ruleset", Bridge: "fastvm0"},
		"bridge": {Table: "fastvm", Bridge: `fastvm0"; flush ruleset; #`},
		"uplink": {Table: "fastvm", Bridge: "fastvm0", Uplink: "eth0\nflush ruleset", CIDR: "10.77.0.0/16"},
		"proto": {Table: "fastvm", Mappings: []nftMapping{
			{GuestIP: "10.77.0.2", HostPort: 80, GuestPort: 80, Proto: "tcp; flush ruleset"},
		}},
		"ip": {Table: "fastvm", Mappings: []nftMapping{
			{GuestIP: "10.77.0.2; flush ruleset", HostPort: 80, GuestPort: 80, Proto: "tcp"},
		}},
		"ports": {Table: "fastvm", Mappings: []nftMapping{
			{GuestIP: "10.77.0.2", HostPort: 70000, GuestPort: 80, Proto: "tcp"},
		}},
	}
	for name, spec := range cases {
		if _, err := renderNftables(spec); err == nil {
			t.Errorf("%s: renderNftables accepted injected/invalid input", name)
		}
	}
}

func TestNftMappings(t *testing.T) {
	a := mustAllocator(t, "10.77.0.0/16")
	first, err := a.allocate("sb1", "", "")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	first.ports = []types.PortMapping{{HostPort: 8080, GuestPort: 80, Proto: "tcp"}}
	second, err := a.allocate("sb2", "", "")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	second.ports = []types.PortMapping{{HostPort: 9000, GuestPort: 9000, Proto: "udp"}}

	got := nftMappings(a.list())
	if len(got) != 2 {
		t.Fatalf("nftMappings len = %d, want 2", len(got))
	}
	if got[0].GuestIP != "10.77.0.2" || got[1].GuestIP != "10.77.0.6" {
		t.Errorf("unexpected mapping order/IPs: %+v", got)
	}
}
