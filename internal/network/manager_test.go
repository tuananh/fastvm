package network

import (
	"context"
	"errors"
	"testing"
	"time"

	"fastvm/internal/types"
)

// newTestManager builds a Manager without touching the host data plane; only
// methods that need CAP_NET_ADMIN are exercised in the gated integration test.
func newTestManager(t *testing.T) Manager {
	t.Helper()
	m, err := NewManager(Options{
		Bridge:          "fastvm0",
		CIDR:            "10.77.0.0/16",
		SwitchMode:      "bridge",
		Isolate:         true,
		WakeNotify:      true,
		NFTBin:          "nft",
		UplinkInterface: "-",
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

func TestNewManagerDefaults(t *testing.T) {
	m, err := NewManager(Options{UplinkInterface: "-"})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer m.Close()
	if got, want := m.(*manager).opts.Bridge, "fastvm0"; got != want {
		t.Errorf("bridge = %q, want %q", got, want)
	}
	if got, want := m.(*manager).opts.CIDR, "10.77.0.0/16"; got != want {
		t.Errorf("cidr = %q, want %q", got, want)
	}
	if !m.(*manager).sniff {
		t.Error("bridge mode should sniff")
	}
}

func TestNewManagerRejectsBadOptions(t *testing.T) {
	cases := map[string]Options{
		"bridge": {Bridge: "fastvm0; rm -rf /"},
		"cidr":   {CIDR: "not-a-cidr"},
		"mode":   {SwitchMode: "magic"},
		"uplink": {UplinkInterface: "eth 0"},
	}
	for name, opts := range cases {
		opts.UplinkInterface = orDefault(opts.UplinkInterface, "-")
		if _, err := NewManager(opts); err == nil {
			t.Errorf("%s: NewManager accepted invalid options", name)
		}
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func TestEBPFModeSelectsEBPFManager(t *testing.T) {
	m, err := NewManager(Options{SwitchMode: "ebpf", UplinkInterface: "-"})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer m.Close()
	if _, ok := m.(*ebpfManager); !ok {
		t.Fatalf("NewManager(ebpf) returned %T, want *ebpfManager", m)
	}
	if _, ok := m.(UplinkProvider); !ok {
		t.Error("eBPF manager should expose its switch uplink")
	}
}

func TestCreateValidationNeedsNoPrivileges(t *testing.T) {
	m := newTestManager(t)
	defer m.Close()
	ctx := context.Background()

	if _, err := m.Create(ctx, CreateRequest{}); err == nil {
		t.Error("Create with empty sandbox ID succeeded")
	}
	_, err := m.Create(ctx, CreateRequest{
		SandboxID: "sb1",
		Ports:     []types.PortMapping{{HostPort: 80, GuestPort: 80, Proto: "sctp"}},
	})
	if err == nil {
		t.Fatal("Create with invalid ports succeeded")
	}
	if errors.Is(err, ErrExists) || errors.Is(err, ErrClosed) {
		t.Fatalf("unexpected error class: %v", err)
	}
}

func TestDeleteUnknownIsIdempotent(t *testing.T) {
	m := newTestManager(t)
	defer m.Close()
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := m.Delete(ctx, "does-not-exist"); err != nil {
			t.Fatalf("Delete(%d): %v", i, err)
		}
	}
}

func TestNotFoundErrors(t *testing.T) {
	m := newTestManager(t)
	defer m.Close()
	ctx := context.Background()

	if err := m.SetAsleep(ctx, "missing", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetAsleep error = %v, want ErrNotFound", err)
	}
	if _, err := m.Stats(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stats error = %v, want ErrNotFound", err)
	}
	act, err := m.(ActivitySource).Activity(ctx, "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Activity error = %v, want ErrNotFound", err)
	}
	_ = act
}

func TestCloseClosesWakeChannelAndIsIdempotent(t *testing.T) {
	m := newTestManager(t)
	wake := m.WakeEvents()
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	select {
	case _, ok := <-wake:
		if ok {
			t.Error("wake channel delivered an event after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("wake channel not closed by Close")
	}
	ctx := context.Background()
	if _, err := m.Create(ctx, CreateRequest{SandboxID: "sb1"}); !errors.Is(err, ErrClosed) {
		t.Errorf("Create after Close error = %v, want ErrClosed", err)
	}
	if err := m.Delete(ctx, "sb1"); !errors.Is(err, ErrClosed) {
		t.Errorf("Delete after Close error = %v, want ErrClosed", err)
	}
}

func TestIgnorePortSet(t *testing.T) {
	if got := ignorePortSet(nil); got != nil {
		t.Errorf("ignorePortSet(nil) = %v", got)
	}
	got := ignorePortSet([]int{8000, 9000})
	if !got[8000] || !got[9000] || got[1] {
		t.Errorf("ignorePortSet = %v", got)
	}
}
