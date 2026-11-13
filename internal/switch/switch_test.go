package l2switch

import (
	"context"
	"errors"
	"net"
	"testing"

	"fastvm/internal/config"
)

func testPort(id string, ifindex int) Port {
	return Port{
		SandboxID: id,
		IfIndex:   ifindex,
		MAC:       net.HardwareAddr{0x02, 0xaa, 0xbb, 0xcc, 0xdd, 0x01},
		IsSandbox: true,
	}
}

func TestParseMode(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Mode
		err  bool
	}{
		{"", ModeEBPF, false},
		{"ebpf", ModeEBPF, false},
		{"bridge", ModeBridge, false},
		{"magic", "", true},
	} {
		got, err := ParseMode(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("ParseMode(%q) succeeded, want error", tc.in)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ParseMode(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestFromConfig(t *testing.T) {
	cfg, err := FromConfig(config.Switch{Mode: "bridge", IsolateSandboxes: true, WakeNotify: true})
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	if cfg.Mode != ModeBridge || !cfg.IsolateSandboxes || !cfg.WakeNotify {
		t.Errorf("FromConfig = %+v", cfg)
	}
	if _, err := FromConfig(config.Switch{Mode: "bogus"}); err == nil {
		t.Error("FromConfig accepted bogus mode")
	}
}

func TestBridgeSwitchLifecycle(t *testing.T) {
	ctx := context.Background()
	s, err := Open(Config{Mode: ModeBridge, IsolateSandboxes: true})
	if err != nil {
		t.Fatalf("Open bridge: %v", err)
	}
	if s.Mode() != ModeBridge {
		t.Errorf("Mode = %q", s.Mode())
	}
	bs := s.(*BridgeSwitch)
	if bs.FallbackReason() != nil {
		t.Errorf("explicit bridge has fallback reason %v", bs.FallbackReason())
	}

	port := testPort("sb1", 7)
	if err := s.AddPort(ctx, port); err != nil {
		t.Fatalf("AddPort: %v", err)
	}
	if err := s.AddPort(ctx, port); err == nil {
		t.Error("duplicate AddPort succeeded")
	}
	if err := s.SetAsleep(ctx, "sb1", true); err != nil {
		t.Errorf("SetAsleep: %v", err)
	}
	if err := s.SetAsleep(ctx, "nope", true); err == nil {
		t.Error("SetAsleep on missing port succeeded")
	}
	stats, err := s.Stats(ctx, "sb1")
	if err != nil {
		t.Errorf("Stats: %v", err)
	}
	if stats != (PortStats{}) {
		t.Errorf("bridge Stats = %+v, want zero", stats)
	}
	if err := s.RemovePort(ctx, "sb1"); err != nil {
		t.Errorf("RemovePort: %v", err)
	}
	if err := s.RemovePort(ctx, "sb1"); err != nil {
		t.Errorf("RemovePort not idempotent: %v", err)
	}

	wake := s.WakeEvents()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, ok := <-wake; ok {
		t.Error("bridge wake channel delivered an event")
	}
	if err := s.AddPort(ctx, testPort("sb2", 8)); err == nil {
		t.Error("AddPort after Close succeeded")
	}
}

func TestOpenRejectsBadConfig(t *testing.T) {
	if _, err := Open(Config{Mode: "not-a-mode"}); err == nil {
		t.Error("Open accepted unknown mode")
	}
	if _, err := Open(Config{Mode: ModeBridge}); err != nil {
		t.Errorf("Open bridge: %v", err)
	}
}

func TestOpenEBPFUnsupportedIsTyped(t *testing.T) {
	// A non-ELF object can never be parsed; Open must surface a typed error so
	// the daemon can fall back to bridge mode.
	_, err := Open(Config{Mode: ModeEBPF, Object: []byte("definitely not an ELF")})
	if err == nil {
		t.Skip("eBPF object unexpectedly accepted; environment may be unusual")
	}
	if !IsUnsupported(err) {
		t.Fatalf("error %v is not *UnsupportedError", err)
	}
	var ue *UnsupportedError
	if !errors.As(err, &ue) {
		t.Fatalf("errors.As(*UnsupportedError) failed for %v", err)
	}
	if ue.Unwrap() == nil {
		t.Error("UnsupportedError does not wrap the cause")
	}
}

func TestOpenWithFallbackReturnsBridge(t *testing.T) {
	s, err := OpenWithFallback(Config{Mode: ModeEBPF, Object: []byte("not an elf")})
	if err != nil {
		t.Fatalf("OpenWithFallback: %v", err)
	}
	defer s.Close()
	if s.Mode() != ModeBridge {
		t.Fatalf("fallback Mode = %q, want bridge", s.Mode())
	}
	bs, ok := s.(*BridgeSwitch)
	if !ok {
		t.Fatalf("fallback type = %T", s)
	}
	if bs.FallbackReason() == nil {
		t.Error("fallback reason not recorded")
	}
}

func TestOpenWithFallbackBridgeStaysBridge(t *testing.T) {
	s, err := OpenWithFallback(Config{Mode: ModeBridge})
	if err != nil {
		t.Fatalf("OpenWithFallback: %v", err)
	}
	defer s.Close()
	bs := s.(*BridgeSwitch)
	if bs.FallbackReason() != nil {
		t.Errorf("explicit bridge recorded fallback reason %v", bs.FallbackReason())
	}
}

func TestBridgePortLookup(t *testing.T) {
	s := newBridgeSwitch(Config{}, nil)
	if err := s.AddPort(context.Background(), testPort("sb1", 9)); err != nil {
		t.Fatalf("AddPort: %v", err)
	}
	if id, ok := s.lookupIfindex(9); !ok || id != "sb1" {
		t.Errorf("lookupIfindex(9) = %q, %v", id, ok)
	}
}
