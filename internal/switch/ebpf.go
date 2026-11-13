package l2switch

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	bpfobj "fastvm/internal/switch/bpf"
)

// eBPF backend (docs/DESIGN.md §6.6, M3). The data plane is bpf/l2switch.c: one tc
// clsact ingress program attached to every switch port, with a static
// MAC->ifindex table, per-port asleep flags and counters, and a ringbuf wake
// channel. The userspace side only manages maps, attaches/detaches the program
// and forwards ringbuf wake events.
//
// Loading needs CAP_BPF/CAP_SYS_ADMIN in the initial user namespace (plus tc
// clsact support and kernel BTF). When unavailable, Open returns
// *UnsupportedError and the daemon should use switch.mode=bridge.

// Map value layouts: must match bpf/l2switch.c exactly (including padding).
type bpfMACKey struct {
	Addr [6]byte
	Pad  uint16
}

type bpfPortCfg struct {
	IsSandbox uint8
	Asleep    uint8
	Pad       uint16
}

type bpfPortStats struct {
	RxPackets        uint64
	RxBytes          uint64
	TxPackets        uint64
	TxBytes          uint64
	Drops            uint64
	ActivityPackets  uint64
	KeepalivePackets uint64
	LastActivityNs   uint64
}

type bpfGlobalCfg struct {
	UplinkIfIndex uint32
	Isolate       uint8
	WakeNotify    uint8
	Pad           uint16
}

type bpfWakeEvent struct {
	IfIndex uint32
	Pad     uint32
	TsNs    uint64
}

// l2switchObjects is the collection of maps and programs loaded from the
// embedded object.
type l2switchObjects struct {
	MacToPort     *ebpf.Map     `ebpf:"mac_to_port"`
	PortCfg       *ebpf.Map     `ebpf:"port_cfg"`
	PortStatsMap  *ebpf.Map     `ebpf:"port_stats"`
	WakeEventsMap *ebpf.Map     `ebpf:"wake_events"`
	GlobalCfg     *ebpf.Map     `ebpf:"global_cfg"`
	L2Switch      *ebpf.Program `ebpf:"l2switch"`
}

// Close releases every map and program. Safe to call multiple times.
func (o *l2switchObjects) Close() error {
	var errs []error
	for _, m := range []*ebpf.Map{o.MacToPort, o.PortCfg, o.PortStatsMap, o.WakeEventsMap, o.GlobalCfg} {
		if m != nil {
			if err := m.Close(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if o.L2Switch != nil {
		if err := o.L2Switch.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// EBPFSwitch is the tc/eBPF backend.
type EBPFSwitch struct {
	cfg  Config
	objs l2switchObjects

	reader *ringbuf.Reader
	wake   chan WakeEvent
	done   chan struct{}

	mu        sync.Mutex
	ports     map[string]Port
	byIfindex map[int]string
	closed    bool

	closeOnce sync.Once
	closeErr  error
	wakeDrops atomic.Uint64
	readErrs  atomic.Uint64
}

// Probe reports whether the eBPF backend can probably be loaded: kernel BTF
// exists and the process holds CAP_BPF or CAP_SYS_ADMIN. A nil error is not a
// guarantee (the verifier can still reject a program), and Open performs the
// real check.
func Probe() error {
	if _, err := os.Stat("/sys/kernel/btf/vmlinux"); err != nil {
		return &UnsupportedError{fmt.Errorf("kernel BTF not available at /sys/kernel/btf/vmlinux: %w", err)}
	}
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return &UnsupportedError{fmt.Errorf("read capabilities: %w", err)}
	}
	caps, err := effectiveCaps(string(data))
	if err != nil {
		return &UnsupportedError{err}
	}
	const (
		capSysAdmin = 21
		capBPF      = 39
	)
	if caps&(1<<capBPF) == 0 && caps&(1<<capSysAdmin) == 0 {
		return &UnsupportedError{errors.New("process lacks CAP_BPF/CAP_SYS_ADMIN (kernel.unprivileged_bpf_disabled may be set)")}
	}
	return nil
}

// effectiveCaps parses CapEff from /proc/self/status.
func effectiveCaps(status string) (uint64, error) {
	for _, line := range strings.Split(status, "\n") {
		if !strings.HasPrefix(line, "CapEff:") {
			continue
		}
		var caps uint64
		if _, err := fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(line, "CapEff:")), "%x", &caps); err != nil {
			return 0, fmt.Errorf("parse CapEff: %w", err)
		}
		return caps, nil
	}
	return 0, errors.New("CapEff not found in /proc/self/status")
}

// openEBPF loads the embedded program, initialises the config map and starts
// the wake-event reader.
func openEBPF(cfg Config) (*EBPFSwitch, error) {
	if err := Probe(); err != nil {
		return nil, err
	}
	object := cfg.Object
	if len(object) == 0 {
		object = bpfobj.L2Switch
	}
	if len(object) == 0 {
		return nil, &UnsupportedError{errors.New("no embedded eBPF object (run make -C bpf)")}
	}
	spec, err := ebpf.LoadCollectionSpecFromReader(bytes.NewReader(object))
	if err != nil {
		return nil, &UnsupportedError{fmt.Errorf("parse eBPF object: %w", err)}
	}

	s := &EBPFSwitch{
		cfg:       cfg,
		wake:      make(chan WakeEvent, 256),
		done:      make(chan struct{}),
		ports:     make(map[string]Port),
		byIfindex: make(map[int]string),
	}
	if err := spec.LoadAndAssign(&s.objs, nil); err != nil {
		var verr *ebpf.VerifierError
		if errors.As(err, &verr) {
			return nil, &UnsupportedError{fmt.Errorf("verifier rejected eBPF program: %w", err)}
		}
		return nil, &UnsupportedError{fmt.Errorf("load eBPF collection: %w", err)}
	}

	global := bpfGlobalCfg{
		UplinkIfIndex: clampIfindex(cfg.UplinkIfIndex),
		Isolate:       boolToU8(cfg.IsolateSandboxes),
		WakeNotify:    boolToU8(cfg.WakeNotify),
	}
	var key uint32
	if err := s.objs.GlobalCfg.Put(&key, &global); err != nil {
		s.objs.Close()
		return nil, &UnsupportedError{fmt.Errorf("initialise global config map: %w", err)}
	}

	reader, err := ringbuf.NewReader(s.objs.WakeEventsMap)
	if err != nil {
		s.objs.Close()
		return nil, &UnsupportedError{fmt.Errorf("open wake ringbuf: %w", err)}
	}
	s.reader = reader
	go s.wakeLoop()
	return s, nil
}

// macArray converts a 6-byte MAC into the fixed-size map key array.
func macArray(mac net.HardwareAddr) [6]byte {
	var a [6]byte
	copy(a[:], mac)
	return a
}

func clampIfindex(v int) uint32 {
	if v < 0 || int64(v) > int64(^uint32(0)) {
		return 0
	}
	return uint32(v)
}

func boolToU8(v bool) uint8 {
	if v {
		return 1
	}
	return 0
}

// Mode returns ModeEBPF.
func (s *EBPFSwitch) Mode() Mode { return ModeEBPF }

// AddPort registers the port's MAC, flags and stats maps, ensures a clsact
// qdisc and attaches the classifier to the port's ingress. It is not
// idempotent: adding an existing port returns an error.
func (s *EBPFSwitch) AddPort(ctx context.Context, p Port) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.SandboxID == "" {
		return errors.New("switch: add port: sandbox ID must not be empty")
	}
	if p.IfIndex <= 0 {
		return fmt.Errorf("switch: add port %s: invalid ifindex %d", p.SandboxID, p.IfIndex)
	}
	if len(p.MAC) != 6 {
		return fmt.Errorf("switch: add port %s: invalid MAC %v", p.SandboxID, p.MAC)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("switch: closed")
	}
	if _, ok := s.ports[p.SandboxID]; ok {
		return fmt.Errorf("switch: port %s already exists", p.SandboxID)
	}

	if err := ensureClsact(ctx, p.IfIndex); err != nil {
		return err
	}

	key := bpfMACKey{Addr: macArray(p.MAC)}
	ifindex := uint32(p.IfIndex)
	if err := s.objs.MacToPort.Put(&key, &ifindex); err != nil {
		return fmt.Errorf("switch: add MAC %s to map: %w", p.MAC, err)
	}
	portCfg := bpfPortCfg{IsSandbox: boolToU8(p.IsSandbox)}
	if err := s.objs.PortCfg.Put(&ifindex, &portCfg); err != nil {
		_ = s.objs.MacToPort.Delete(&key)
		return fmt.Errorf("switch: add port cfg for %s: %w", p.SandboxID, err)
	}
	stats := bpfPortStats{}
	if err := s.objs.PortStatsMap.Put(&ifindex, &stats); err != nil {
		_ = s.objs.PortCfg.Delete(&ifindex)
		_ = s.objs.MacToPort.Delete(&key)
		return fmt.Errorf("switch: add port stats for %s: %w", p.SandboxID, err)
	}

	filter := s.portFilter(p.IfIndex)
	if err := netlink.FilterAdd(filter); err != nil {
		_ = s.objs.PortStatsMap.Delete(&ifindex)
		_ = s.objs.PortCfg.Delete(&ifindex)
		_ = s.objs.MacToPort.Delete(&key)
		return fmt.Errorf("switch: attach classifier to ifindex %d: %w", p.IfIndex, err)
	}

	s.ports[p.SandboxID] = p
	s.byIfindex[p.IfIndex] = p.SandboxID
	return nil
}

// portFilter builds the tc ingress filter that runs the L2 switch program.
func (s *EBPFSwitch) portFilter(ifindex int) *netlink.BpfFilter {
	return &netlink.BpfFilter{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: ifindex,
			Parent:    netlink.HANDLE_MIN_INGRESS,
			Handle:    netlink.MakeHandle(0, 1),
			Priority:  1,
			Protocol:  unix.ETH_P_ALL,
		},
		Fd:           s.objs.L2Switch.FD(),
		Name:         "fastvm_l2",
		DirectAction: true,
	}
}

// ensureClsact adds a clsact qdisc on the link if one is missing.
func ensureClsact(ctx context.Context, ifindex int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	link, err := netlink.LinkByIndex(ifindex)
	if err != nil {
		return fmt.Errorf("switch: look up ifindex %d: %w", ifindex, err)
	}
	qdisc := &netlink.Clsact{QdiscAttrs: netlink.QdiscAttrs{
		LinkIndex: ifindex,
		Handle:    netlink.MakeHandle(0xffff, 0),
		Parent:    netlink.HANDLE_CLSACT,
	}}
	if err := netlink.QdiscAdd(qdisc); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("switch: add clsact qdisc to %s: %w", link.Attrs().Name, err)
	}
	return nil
}

// RemovePort detaches the classifier and drops the port's map state. It is
// idempotent.
func (s *EBPFSwitch) RemovePort(ctx context.Context, sandboxID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.ports[sandboxID]
	if !ok {
		return nil
	}
	var errs []error
	if err := netlink.FilterDel(s.portFilter(p.IfIndex)); err != nil {
		errs = append(errs, fmt.Errorf("switch: detach classifier from ifindex %d: %w", p.IfIndex, err))
	}
	ifindex := uint32(p.IfIndex)
	key := bpfMACKey{Addr: macArray(p.MAC)}
	for _, m := range []struct {
		name string
		m    *ebpf.Map
		key  any
	}{
		{"mac_to_port", s.objs.MacToPort, &key},
		{"port_cfg", s.objs.PortCfg, &ifindex},
		{"port_stats", s.objs.PortStatsMap, &ifindex},
	} {
		if err := m.m.Delete(m.key); err != nil {
			errs = append(errs, fmt.Errorf("switch: delete %s entry for %s: %w", m.name, sandboxID, err))
		}
	}
	delete(s.ports, sandboxID)
	delete(s.byIfindex, p.IfIndex)
	return errors.Join(errs...)
}

// SetAsleep flips the port's asleep flag in the BPF map. While asleep, the
// program emits a ringbuf wake event for qualifying frames and still redirects
// them into the port.
func (s *EBPFSwitch) SetAsleep(ctx context.Context, sandboxID string, asleep bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.ports[sandboxID]
	if !ok {
		return fmt.Errorf("switch: port %s not found", sandboxID)
	}
	ifindex := uint32(p.IfIndex)
	var cfg bpfPortCfg
	if err := s.objs.PortCfg.Lookup(&ifindex, &cfg); err != nil {
		return fmt.Errorf("switch: read port cfg for %s: %w", sandboxID, err)
	}
	cfg.Asleep = boolToU8(asleep)
	if err := s.objs.PortCfg.Put(&ifindex, &cfg); err != nil {
		return fmt.Errorf("switch: write port cfg for %s: %w", sandboxID, err)
	}
	return nil
}

// Stats reads the exported per-port counters.
func (s *EBPFSwitch) Stats(ctx context.Context, sandboxID string) (PortStats, error) {
	if err := ctx.Err(); err != nil {
		return PortStats{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.ports[sandboxID]
	if !ok {
		return PortStats{}, fmt.Errorf("switch: port %s not found", sandboxID)
	}
	ifindex := uint32(p.IfIndex)
	var raw bpfPortStats
	if err := s.objs.PortStatsMap.Lookup(&ifindex, &raw); err != nil {
		return PortStats{}, fmt.Errorf("switch: read stats for %s: %w", sandboxID, err)
	}
	stats := PortStats{
		RxBytes:          raw.RxBytes,
		RxPackets:        raw.RxPackets,
		TxBytes:          raw.TxBytes,
		TxPackets:        raw.TxPackets,
		Drops:            raw.Drops,
		ActivityPackets:  raw.ActivityPackets,
		KeepalivePackets: raw.KeepalivePackets,
	}
	if raw.LastActivityNs > 0 {
		stats.LastActivity = time.Unix(0, int64(raw.LastActivityNs))
	}
	return stats, nil
}

// WakeEvents returns ringbuf-derived wake notifications. Closed on Close.
func (s *EBPFSwitch) WakeEvents() <-chan WakeEvent { return s.wake }

// wakeLoop drains the ringbuf and resolves ifindex -> sandbox ID.
func (s *EBPFSwitch) wakeLoop() {
	defer close(s.done)
	for {
		var rec ringbuf.Record
		err := s.reader.ReadInto(&rec)
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				return
			}
			s.readErrs.Add(1)
			continue
		}
		if len(rec.RawSample) < 16 {
			continue
		}
		ifindex := int(binary.LittleEndian.Uint32(rec.RawSample[0:4]))
		ts := int64(binary.LittleEndian.Uint64(rec.RawSample[8:16]))
		sandboxID, ok := s.lookupIfindex(ifindex)
		if !ok {
			continue
		}
		ev := WakeEvent{SandboxID: sandboxID, IfIndex: ifindex, Time: time.Unix(0, ts)}
		select {
		case s.wake <- ev:
		default:
			s.wakeDrops.Add(1)
		}
	}
}

func (s *EBPFSwitch) lookupIfindex(ifindex int) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byIfindex[ifindex]
	return id, ok
}

// WakeDrops reports ringbuf wakes dropped because the userspace channel was
// full, plus ringbuf read errors.
func (s *EBPFSwitch) WakeDrops() uint64 { return s.wakeDrops.Load() }

// ReadErrors reports ringbuf read errors.
func (s *EBPFSwitch) ReadErrors() uint64 { return s.readErrs.Load() }

// Close detaches every classifier, stops the ringbuf reader and releases the
// BPF objects. It is idempotent.
func (s *EBPFSwitch) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		ids := make([]string, 0, len(s.ports))
		for id := range s.ports {
			ids = append(ids, id)
		}
		s.mu.Unlock()

		var errs []error
		for _, id := range ids {
			if err := s.RemovePort(context.Background(), id); err != nil {
				errs = append(errs, err)
			}
		}
		if s.reader != nil {
			if err := s.reader.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				errs = append(errs, err)
			}
			<-s.done
		}
		if err := s.objs.Close(); err != nil {
			errs = append(errs, err)
		}
		close(s.wake)
		if joined := errors.Join(errs...); joined != nil {
			// Preserve the error for callers that check Close; the data plane is
			// already detached.
			s.closeErr = joined
		}
	})
	return s.closeErr
}

var _ Switch = (*EBPFSwitch)(nil)
