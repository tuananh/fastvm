package lifecycle

import (
	"testing"
	"time"

	"fastvm/internal/config"
	"fastvm/internal/types"
)

func testIdleConfig() IdleConfig {
	d := config.Default()
	return IdleConfig{
		Window:          d.Lifecycle.IdleWindow.D(),
		SampleInterval:  d.Idle.SampleInterval.D(),
		ExitHold:        d.Idle.ExitHold.D(),
		Warmup:          d.Idle.Warmup.D(),
		CPUThresholdPct: d.Idle.CPUThresholdPct,
		CPUWakePct:      d.Idle.CPUWakePct,
		BlockIOBytes:    d.Idle.BlockIOBytes,
		KeepaliveAware:  d.Idle.KeepaliveAware,
	}
}

func TestEvaluateIdle(t *testing.T) {
	cfg := testIdleConfig()
	now := time.Unix(1_700_000_000, 0)
	base := Observation{
		Now:            now,
		LastActivityAt: now.Add(-45 * time.Second),
		CPUAvgPct:      0.4,
	}

	tests := []struct {
		name   string
		mutate func(*Observation)
		idle   bool
	}{
		{"quiet sandbox", func(o *Observation) {}, true},
		{"auto sleep disabled", func(o *Observation) { o.NoAutoSleep = true }, false},
		{"warmup", func(o *Observation) { o.WarmupUntil = now.Add(5 * time.Second) }, false},
		{"exec attached", func(o *Observation) { o.ExecActive = true }, false},
		{"frame in flight", func(o *Observation) { o.InFlightFrame = true }, false},
		{"guest busy", func(o *Observation) { o.GuestBusy = true }, false},
		{"activity inside window", func(o *Observation) { o.LastActivityAt = now.Add(-5 * time.Second) }, false},
		{"cpu too high", func(o *Observation) { o.CPUAvgPct = 2 }, false},
		{"cpu exactly at threshold", func(o *Observation) { o.CPUAvgPct = 1.0 }, false},
		{"block io over budget", func(o *Observation) { o.BlockIOBytes = 1 << 20 }, false},
		{"block io under budget", func(o *Observation) { o.BlockIOBytes = 1024 }, true},
		{"no baseline", func(o *Observation) { o.LastActivityAt = time.Time{} }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := base
			tc.mutate(&o)
			got := EvaluateIdle(cfg, o)
			if got.Idle != tc.idle {
				t.Fatalf("Idle = %v (%s), want %v", got.Idle, got.Reason, tc.idle)
			}
			if got.Reason == "" {
				t.Fatal("empty reason")
			}
		})
	}
}

func TestCPUWindowAverageAndPrune(t *testing.T) {
	cfg := testIdleConfig()
	w := NewCPUWindow(cfg)
	start := time.Unix(1_700_000_000, 0)
	for i := 0; i < 40; i++ {
		w.Add(start.Add(time.Duration(i)*time.Second), float64(i))
	}
	now := start.Add(39 * time.Second)
	// window is 30s inclusive: samples 9..39 remain (31 samples)
	if got := w.Samples(now); got != 31 {
		t.Fatalf("samples = %d, want 31", got)
	}
	// Mean of 9..39 inclusive = 24
	if got := w.Average(now); got != 24 {
		t.Fatalf("average = %v, want 24", got)
	}
	// Advance beyond all samples: window empties.
	if got := w.Average(now.Add(2 * time.Minute)); got != 0 {
		t.Fatalf("average after expiry = %v, want 0", got)
	}
}

func TestCPUWindowAboveFor(t *testing.T) {
	cfg := testIdleConfig()
	cfg.ExitHold = 3 * time.Second
	start := time.Unix(1_700_000_000, 0)
	w := NewCPUWindow(cfg)
	// Sustained 8% for 5 samples.
	for i := 0; i < 5; i++ {
		w.Add(start.Add(time.Duration(i)*time.Second), 8)
	}
	now := start.Add(4 * time.Second)
	if !w.AboveFor(now, 5, 3*time.Second) {
		t.Fatal("expected sustained CPU spike to be detected")
	}
	// One dip makes it false.
	w.Add(start.Add(5*time.Second), 1)
	if w.AboveFor(start.Add(5*time.Second), 5, 3*time.Second) {
		t.Fatal("dip should reset the hold")
	}
	// Not enough coverage of the hold window.
	w2 := NewCPUWindow(cfg)
	w2.Add(now.Add(-2*time.Second), 8)
	w2.Add(now.Add(-1*time.Second), 8)
	if w2.AboveFor(now, 5, 3*time.Second) {
		t.Fatal("hold not covered should be false")
	}
}

func TestTransitions(t *testing.T) {
	ok := [][2]types.State{
		{types.StateCreating, types.StateStarting},
		{types.StateStarting, types.StateRunning},
		{types.StateRunning, types.StateIdle},
		{types.StateIdle, types.StateRunning},
		{types.StateIdle, types.StatePaused},
		{types.StatePaused, types.StateRunning},
		{types.StatePaused, types.StateDormant},
		{types.StateDormant, types.StateRunning},
		{types.StateRunning, types.StateStopping},
		{types.StateStopping, types.StateStopped},
		{types.StateFailed, types.StateStopping},
		{types.StateRunning, types.StateRunning},
	}
	for _, p := range ok {
		if err := ValidateTransition(p[0], p[1]); err != nil {
			t.Errorf("%s -> %s: %v", p[0], p[1], err)
		}
	}
	bad := [][2]types.State{
		{types.StateRunning, types.StatePaused},   // must pass through Idle (sleep barrier)
		{types.StateDormant, types.StatePaused},   // must wake through Running
		{types.StateStopped, types.StateRunning},  // terminal
		{types.StateCreating, types.StateRunning}, // must start first
		{types.StatePaused, types.StateIdle},
	}
	for _, p := range bad {
		if err := ValidateTransition(p[0], p[1]); err == nil {
			t.Errorf("%s -> %s: expected error", p[0], p[1])
		}
	}
}

func TestWorkloadExitTarget(t *testing.T) {
	for policy, want := range map[string]types.State{
		"":        types.StateIdle,
		"idle":    types.StateIdle,
		"pause":   types.StatePaused,
		"offload": types.StateDormant,
		"stop":    types.StateStopping,
	} {
		got, err := WorkloadExitTarget(policy)
		if err != nil {
			t.Fatalf("%q: %v", policy, err)
		}
		if got != want {
			t.Fatalf("%q => %s, want %s", policy, got, want)
		}
	}
	if _, err := WorkloadExitTarget("bogus"); err == nil {
		t.Fatal("expected error for unknown policy")
	}
}
