package network

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

// nftables rule generation and application (docs/DESIGN.md §6.6: NAT/DNAT stay in
// nftables; the switch only does L2 forwarding).
//
// Rules are rendered as text and applied atomically with `nft -f <tempfile>`.
// Rendering is a pure function so it can be unit-tested without privileges.

const nftTable = "fastvm"

var nftNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)

// nftMapping is one rendered published-port DNAT rule.
type nftMapping struct {
	GuestIP   string
	HostPort  int
	GuestPort int
	Proto     string
}

// nftSpec is everything needed to render the fastvm nftables table.
type nftSpec struct {
	Table    string
	Bridge   string
	CIDR     string
	Uplink   string
	Mappings []nftMapping
}

// renderNftables returns the nft rule file body, or "" when there is nothing
// to install (no published ports and no uplink to masquerade).
//
// The table is keyed on IPv4 only (docs/DESIGN.md: IPv6 out of scope for v0.1).
// DNAT is installed in both the prerouting hook (traffic arriving from outside)
// and the output hook (host-local traffic such as `curl 127.0.0.1:<port>`).
// The prerouting rule excludes traffic arriving from the sandbox bridge so
// guest traffic is never re-DNATed; output traffic is by definition locally
// generated. Masquerade applies only to sandbox source addresses leaving the
// configured uplink.
func renderNftables(spec nftSpec) (string, error) {
	if spec.Table == "" {
		spec.Table = nftTable
	}
	if !nftNameRE.MatchString(spec.Table) {
		return "", fmt.Errorf("network: invalid nft table name %q", spec.Table)
	}
	if spec.Bridge != "" {
		if err := validateIfaceName(spec.Bridge); err != nil {
			return "", err
		}
	}
	if spec.Uplink != "" {
		if err := validateIfaceName(spec.Uplink); err != nil {
			return "", fmt.Errorf("network: invalid uplink: %w", err)
		}
	}
	if spec.CIDR != "" {
		if _, _, err := net.ParseCIDR(spec.CIDR); err != nil {
			return "", fmt.Errorf("network: invalid pool CIDR %q: %w", spec.CIDR, err)
		}
	}

	mappings := append([]nftMapping(nil), spec.Mappings...)
	sort.Slice(mappings, func(i, j int) bool {
		if mappings[i].HostPort != mappings[j].HostPort {
			return mappings[i].HostPort < mappings[j].HostPort
		}
		return mappings[i].Proto < mappings[j].Proto
	})

	hasMasquerade := spec.Uplink != "" && spec.CIDR != ""
	// Host-local published ports (curl 127.0.0.1:<port>) need hairpin SNAT so
	// the guest replies to the bridge gateway rather than to 127.0.0.1.
	hasHairpin := len(mappings) > 0 && spec.Bridge != ""
	if len(mappings) == 0 && !hasMasquerade {
		return "", nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "table ip %s {\n", spec.Table)
	if len(mappings) > 0 {
		// Validate and render each mapping once, then emit the same rules into
		// the prerouting (external) and output (host-local) hooks.
		type rendered struct {
			proto, guestIP      string
			hostPort, guestPort int
		}
		var rules []rendered
		for _, m := range mappings {
			proto := strings.ToLower(m.Proto)
			if proto != "tcp" && proto != "udp" {
				return "", fmt.Errorf("network: invalid DNAT proto %q", m.Proto)
			}
			guestIP, err := validateIPv4(m.GuestIP)
			if err != nil {
				return "", err
			}
			if m.HostPort < 1 || m.HostPort > 65535 || m.GuestPort < 1 || m.GuestPort > 65535 {
				return "", fmt.Errorf("network: DNAT port out of range: %d -> %d", m.HostPort, m.GuestPort)
			}
			rules = append(rules, rendered{proto, guestIP, m.HostPort, m.GuestPort})
		}
		b.WriteString("\tchain prerouting {\n")
		b.WriteString("\t\ttype nat hook prerouting priority dstnat; policy accept;\n")
		for _, r := range rules {
			if spec.Bridge != "" {
				fmt.Fprintf(&b, "\t\tiifname != %q %s dport %d dnat to %s:%d\n",
					spec.Bridge, r.proto, r.hostPort, r.guestIP, r.guestPort)
			} else {
				fmt.Fprintf(&b, "\t\t%s dport %d dnat to %s:%d\n",
					r.proto, r.hostPort, r.guestIP, r.guestPort)
			}
		}
		b.WriteString("\t}\n")
		// Host-local traffic (curl 127.0.0.1:host_port) traverses the output
		// hook, not prerouting. Same mappings, no bridge interface filter.
		b.WriteString("\tchain output {\n")
		b.WriteString("\t\ttype nat hook output priority dstnat; policy accept;\n")
		for _, r := range rules {
			fmt.Fprintf(&b, "\t\t%s dport %d dnat to %s:%d\n",
				r.proto, r.hostPort, r.guestIP, r.guestPort)
		}
		b.WriteString("\t}\n")
	}
	if hasMasquerade || hasHairpin {
		b.WriteString("\tchain postrouting {\n")
		b.WriteString("\t\ttype nat hook postrouting priority srcnat; policy accept;\n")
		if hasMasquerade {
			fmt.Fprintf(&b, "\t\tip saddr %s oifname %q masquerade\n", spec.CIDR, spec.Uplink)
		}
		if hasHairpin {
			fmt.Fprintf(&b, "\t\tip saddr 127.0.0.1 oifname %q masquerade\n", spec.Bridge)
		}
		b.WriteString("\t}\n")
	}
	b.WriteString("}\n")
	return b.String(), nil
}

func validateIPv4(s string) (string, error) {
	ip := net.ParseIP(s).To4()
	if ip == nil {
		return "", fmt.Errorf("network: invalid IPv4 address %q", s)
	}
	return ip.String(), nil
}

// nftApplier applies/removes the rendered rule set.
type nftApplier struct {
	bin   string
	table string
}

// apply installs body as the complete fastvm table. An empty body removes the
// table. Missing tables are ignored so apply/delete are idempotent.
func (n *nftApplier) apply(ctx context.Context, body string) error {
	if strings.TrimSpace(body) == "" {
		return n.delete(ctx)
	}
	// Two-step replace: nft has no portable "replace table" verb, and a
	// leading `delete table` aborts the whole file when the table is absent,
	// so remove first (best effort) and then install the new table.
	if err := n.delete(ctx); err != nil {
		return err
	}
	f, err := os.CreateTemp("", "fastvm-nft-*.rules")
	if err != nil {
		return fmt.Errorf("network: create nft rule file: %w", err)
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		return fmt.Errorf("network: write nft rule file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("network: close nft rule file: %w", err)
	}
	if out, err := exec.CommandContext(ctx, n.bin, "-f", path).CombinedOutput(); err != nil {
		return fmt.Errorf("network: apply nft rules: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// delete removes the fastvm table, ignoring "table does not exist".
func (n *nftApplier) delete(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, n.bin, "delete", "table", "ip", n.table).CombinedOutput()
	if err == nil {
		return nil
	}
	msg := strings.ToLower(string(out))
	if strings.Contains(msg, "no such file or directory") || strings.Contains(msg, "does not exist") {
		return nil
	}
	return fmt.Errorf("network: delete nft table %s: %w: %s", n.table, err, strings.TrimSpace(string(out)))
}

// nftMappings flattens published ports for rendering.
func nftMappings(allocs []*allocation) []nftMapping {
	var out []nftMapping
	for _, alloc := range allocs {
		for _, p := range alloc.ports {
			out = append(out, nftMapping{
				GuestIP:   alloc.ip.String(),
				HostPort:  p.HostPort,
				GuestPort: p.GuestPort,
				Proto:     p.Proto,
			})
		}
	}
	return out
}
