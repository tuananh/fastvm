package network

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net"
	"sort"
	"strings"

	"fastvm/internal/types"
)

// allocation is one sandbox's network allocation inside the daemon namespace.
type allocation struct {
	sandboxID string
	block     uint32 // /30 block index inside the pool
	ip        net.IP // guest address, block base + 2
	gateway   net.IP // block base + 1, hosted on the bridge
	prefix    int
	mac       net.HardwareAddr
	tap       string
	ports     []types.PortMapping
}

// allocator hands out point-to-point /30 blocks and MACs from fixed pools.
// It is not safe for concurrent use; Manager serialises access.
type allocator struct {
	network net.IP // first address of the pool
	prefix  int
	blocks  uint32 // number of /30 blocks in the pool

	next   uint32 // next block index to try
	byID   map[string]*allocation
	byIP   map[string]string // guest IP -> sandbox ID
	byMAC  map[string]string // MAC string -> sandbox ID
	byName map[string]string // TAP name -> sandbox ID
}

func newAllocator(cidr string) (*allocator, error) {
	ip, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("network: parse pool %q: %w", cidr, err)
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("network: pool %q must be IPv4 (docs/DESIGN.md v0.1 is IPv4-only)", cidr)
	}
	prefix, bits := network.Mask.Size()
	if bits != 32 {
		return nil, fmt.Errorf("network: pool %q must be IPv4", cidr)
	}
	if prefix > 30 {
		return nil, fmt.Errorf("network: pool %q must have prefix <= /30 to allocate point-to-point pairs", cidr)
	}
	blocks := uint32(1) << (30 - prefix) // 2^(30-prefix) /30 blocks
	if blocks == 0 {
		blocks = 1
	}
	return &allocator{
		network: append(net.IP(nil), network.IP.To4()...),
		prefix:  prefix,
		blocks:  blocks,
		byID:    make(map[string]*allocation),
		byIP:    make(map[string]string),
		byMAC:   make(map[string]string),
		byName:  make(map[string]string),
	}, nil
}

// blockBase returns the first address of block idx.
func (a *allocator) blockBase(idx uint32) net.IP {
	base := binary.BigEndian.Uint32(a.network)
	addr := make(net.IP, 4)
	binary.BigEndian.PutUint32(addr, base+4*idx)
	return addr
}

func ipKey(ip net.IP) string { return ip.To4().String() }

// allocate assigns an address, MAC and TAP name to sandboxID. requestedMAC and
// requestedIP are optional (adoption path); empty means auto-allocate.
func (a *allocator) allocate(sandboxID, requestedMAC, requestedIP string) (*allocation, error) {
	if sandboxID == "" {
		return nil, fmt.Errorf("network: sandbox ID must not be empty")
	}
	if _, ok := a.byID[sandboxID]; ok {
		return nil, fmt.Errorf("%w: %s", ErrExists, sandboxID)
	}

	var (
		block uint32
		ip    net.IP
	)
	if requestedIP != "" {
		req := net.ParseIP(requestedIP).To4()
		if req == nil {
			return nil, fmt.Errorf("network: invalid requested IP %q", requestedIP)
		}
		if !a.contains(req) {
			return nil, fmt.Errorf("network: requested IP %s is outside pool %s/%d", req, a.network, a.prefix)
		}
		base := binary.BigEndian.Uint32(a.network)
		offset := binary.BigEndian.Uint32(req) - base
		block = offset / 4
		if offset%4 != 2 {
			return nil, fmt.Errorf("network: requested IP %s is not a guest address (expected block base + 2)", req)
		}
		if owner, ok := a.byIP[ipKey(req)]; ok {
			return nil, fmt.Errorf("%w: IP %s in use by %s", ErrExists, req, owner)
		}
		ip = append(net.IP(nil), req...)
	} else {
		var ok bool
		block, ip, ok = a.freeBlock()
		if !ok {
			return nil, fmt.Errorf("network: pool %s/%d exhausted (%d blocks in use)", a.network, a.prefix, a.blocks)
		}
	}

	mac, err := a.allocateMAC(sandboxID, requestedMAC)
	if err != nil {
		return nil, err
	}
	tap := a.allocateIfname(sandboxID)

	base := binary.BigEndian.Uint32(a.network) + 4*block
	gw := make(net.IP, 4)
	binary.BigEndian.PutUint32(gw, base+1)

	alloc := &allocation{
		sandboxID: sandboxID,
		block:     block,
		ip:        ip,
		gateway:   gw,
		prefix:    30,
		mac:       mac,
		tap:       tap,
	}
	a.byID[sandboxID] = alloc
	a.byIP[ipKey(ip)] = sandboxID
	a.byMAC[mac.String()] = sandboxID
	a.byName[tap] = sandboxID
	return alloc, nil
}

// contains reports whether ip is inside the pool.
func (a *allocator) contains(ip net.IP) bool {
	base := binary.BigEndian.Uint32(a.network)
	host := binary.BigEndian.Uint32(ip.To4())
	last := base + a.blocks*4 - 1
	return host >= base && host <= last
}

// freeBlock picks the next unused /30 block, wrapping around the pool.
func (a *allocator) freeBlock() (uint32, net.IP, bool) {
	used := make(map[uint32]bool, len(a.byID))
	for _, alloc := range a.byID {
		used[alloc.block] = true
	}
	for i := uint32(0); i < a.blocks; i++ {
		idx := (a.next + i) % a.blocks
		if used[idx] {
			continue
		}
		a.next = (idx + 1) % a.blocks
		base := a.blockBase(idx)
		ip := make(net.IP, 4)
		binary.BigEndian.PutUint32(ip, binary.BigEndian.Uint32(base)+2)
		return idx, ip, true
	}
	return 0, nil, false
}

// allocateMAC validates requestedMAC or derives a stable MAC from sandboxID.
func (a *allocator) allocateMAC(sandboxID, requestedMAC string) (net.HardwareAddr, error) {
	if requestedMAC != "" {
		mac, err := net.ParseMAC(requestedMAC)
		if err != nil {
			return nil, fmt.Errorf("network: invalid requested MAC %q: %w", requestedMAC, err)
		}
		if len(mac) != 6 {
			return nil, fmt.Errorf("network: requested MAC %q must be 6 bytes", requestedMAC)
		}
		if mac[0]&0x01 != 0 {
			return nil, fmt.Errorf("network: requested MAC %q is multicast", requestedMAC)
		}
		key := mac.String()
		if owner, ok := a.byMAC[key]; ok {
			return nil, fmt.Errorf("%w: MAC %s in use by %s", ErrExists, mac, owner)
		}
		return mac, nil
	}
	for salt := 0; salt < 64; salt++ {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s#%d", sandboxID, salt)))
		mac := net.HardwareAddr(sum[0:6])
		mac[0] = (mac[0] & 0xfc) | 0x02 // locally administered, unicast
		if _, taken := a.byMAC[mac.String()]; !taken {
			return mac, nil
		}
	}
	return nil, fmt.Errorf("network: could not derive a unique MAC for %s", sandboxID)
}

// allocateIfname derives a unique, valid Linux interface name for a sandbox
// TAP. IFNAMSIZ is 16 (15 visible bytes).
func (a *allocator) allocateIfname(sandboxID string) string {
	candidate := "tap-" + ifnameFragment(sandboxID)
	for salt := 1; salt < 64; salt++ {
		if _, taken := a.byName[candidate]; !taken {
			return candidate
		}
		candidate = "tap-" + ifnameFragment(fmt.Sprintf("%s#%d", sandboxID, salt))
	}
	// Astronomically unlikely; fall back to a hash of the full ID.
	sum := sha256.Sum256([]byte(sandboxID + "#ifname"))
	return "tap-" + fmt.Sprintf("%x", sum[:4])
}

// ifnameFragment returns up to 11 characters (tap- leaves 4 for the prefix)
// that are safe in an interface name.
func ifnameFragment(id string) string {
	if len(id) <= 11 && isSimpleIfaceFragment(id) {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return fmt.Sprintf("%x", sum[:6])[:11]
}

func isSimpleIfaceFragment(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

// release drops an allocation and returns it, if present.
func (a *allocator) release(sandboxID string) (*allocation, bool) {
	alloc, ok := a.byID[sandboxID]
	if !ok {
		return nil, false
	}
	delete(a.byID, sandboxID)
	delete(a.byIP, ipKey(alloc.ip))
	delete(a.byMAC, alloc.mac.String())
	delete(a.byName, alloc.tap)
	return alloc, true
}

func (a *allocator) get(sandboxID string) (*allocation, bool) {
	alloc, ok := a.byID[sandboxID]
	return alloc, ok
}

// setPorts replaces the published-port list for an allocation.
func (a *allocator) setPorts(sandboxID string, ports []types.PortMapping) {
	if alloc, ok := a.byID[sandboxID]; ok {
		alloc.ports = append([]types.PortMapping(nil), ports...)
	}
}

// list returns allocations sorted by block index for deterministic rule files.
func (a *allocator) list() []*allocation {
	out := make([]*allocation, 0, len(a.byID))
	for _, alloc := range a.byID {
		out = append(out, alloc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].block < out[j].block })
	return out
}

// validatePorts rejects unusable published-port requests and duplicate
// host port/proto pairs.
func validatePorts(ports []types.PortMapping) error {
	seen := make(map[string]bool, len(ports))
	for i, p := range ports {
		proto := strings.ToLower(p.Proto)
		if proto != "tcp" && proto != "udp" {
			return fmt.Errorf("network: port mapping %d: proto %q must be tcp or udp", i, p.Proto)
		}
		if p.HostPort < 1 || p.HostPort > 65535 {
			return fmt.Errorf("network: port mapping %d: host_port %d out of range", i, p.HostPort)
		}
		if p.GuestPort < 1 || p.GuestPort > 65535 {
			return fmt.Errorf("network: port mapping %d: guest_port %d out of range", i, p.GuestPort)
		}
		key := fmt.Sprintf("%s/%d", proto, p.HostPort)
		if seen[key] {
			return fmt.Errorf("network: duplicate host port mapping %s", key)
		}
		seen[key] = true
	}
	return nil
}
