// Package lifecycle implements the sandbox state machine and the debounced
// idle predicate from docs/DESIGN.md §5.2–§5.3.
package lifecycle

import (
	"fmt"
	"time"

	"fastvm/internal/types"
)

// IdleConfig are the thresholds for the idle predicate.
type IdleConfig struct {
	Window          time.Duration
	SampleInterval  time.Duration
	ExitHold        time.Duration
	Warmup          time.Duration
	CPUThresholdPct float64
	CPUWakePct      float64
	BlockIOBytes    int64
	KeepaliveAware  bool
	IgnorePorts     []int
}

// Observation is one evaluation of the idle inputs.
type Observation struct {
	Now            time.Time
	LastActivityAt time.Time
	CPUAvgPct      float64
	// BlockIOBytes is block-device bytes attributable to the current window.
	BlockIOBytes  int64
	ExecActive    bool
	GuestBusy     bool
	InFlightFrame bool
	WarmupUntil   time.Time
	NoAutoSleep   bool
}

// IdleDecision is the predicate result plus a human-readable cause for events
// and `fastvm activity`.
type IdleDecision struct {
	Idle   bool
	Reason string
}

// EvaluateIdle applies the idle predicate. A sandbox is Idle only when every
// veto is clear for the whole window; the reason names the first blocker.
func EvaluateIdle(cfg IdleConfig, o Observation) IdleDecision {
	switch {
	case o.NoAutoSleep:
		return IdleDecision{false, "auto-sleep disabled"}
	case !o.WarmupUntil.IsZero() && o.Now.Before(o.WarmupUntil):
		return IdleDecision{false, "warmup"}
	case o.ExecActive:
		return IdleDecision{false, "exec session attached"}
	case o.InFlightFrame:
		return IdleDecision{false, "frame delivery in flight"}
	case o.GuestBusy:
		return IdleDecision{false, "guest reports workload_busy"}
	case o.LastActivityAt.IsZero():
		return IdleDecision{false, "no activity baseline yet"}
	}
	idleFor := o.Now.Sub(o.LastActivityAt)
	if idleFor < cfg.Window {
		return IdleDecision{false, fmt.Sprintf("quiet for %s < %s", idleFor.Round(time.Millisecond), cfg.Window)}
	}
	if o.CPUAvgPct >= cfg.CPUThresholdPct {
		return IdleDecision{false, fmt.Sprintf("cpu %.2f%% >= %.2f%%", o.CPUAvgPct, cfg.CPUThresholdPct)}
	}
	if cfg.BlockIOBytes > 0 && o.BlockIOBytes >= cfg.BlockIOBytes {
		return IdleDecision{false, fmt.Sprintf("block io %d B >= %d B", o.BlockIOBytes, cfg.BlockIOBytes)}
	}
	return IdleDecision{true, fmt.Sprintf("quiet for %s, cpu %.2f%%, block io %d B",
		idleFor.Round(time.Millisecond), o.CPUAvgPct, o.BlockIOBytes)}
}

type cpuSample struct {
	at  time.Time
	pct float64
}

// CPUWindow keeps a sliding window of CPU utilization samples and answers the
// two questions the lifecycle needs: the windowed average (idle veto) and
// whether CPU has stayed above a threshold for the exit-hold duration (leaving
// Idle without guest activity).
type CPUWindow struct {
	cfg     IdleConfig
	samples []cpuSample
}

// NewCPUWindow returns a window using cfg.Window and cfg.SampleInterval.
func NewCPUWindow(cfg IdleConfig) *CPUWindow {
	window := cfg.Window
	if window <= 0 {
		window = 30 * time.Second
	}
	cfg.Window = window
	return &CPUWindow{cfg: cfg}
}

// Add records a sample and prunes samples older than the window.
func (w *CPUWindow) Add(at time.Time, pct float64) {
	w.samples = append(w.samples, cpuSample{at: at, pct: pct})
	w.prune(at)
}

func (w *CPUWindow) prune(now time.Time) {
	cutoff := now.Add(-w.cfg.Window)
	i := 0
	for i < len(w.samples) && w.samples[i].at.Before(cutoff) {
		i++
	}
	if i > 0 {
		w.samples = append([]cpuSample(nil), w.samples[i:]...)
	}
}

// Average returns the mean CPU percentage over the window. It returns 0 when
// there are no samples; callers should treat "no data" as quiet only after the
// sandbox is Running (which guarantees samples).
func (w *CPUWindow) Average(now time.Time) float64 {
	w.prune(now)
	if len(w.samples) == 0 {
		return 0
	}
	var sum float64
	for _, s := range w.samples {
		sum += s.pct
	}
	return sum / float64(len(w.samples))
}

// Samples returns the number of samples currently in the window.
func (w *CPUWindow) Samples(now time.Time) int {
	w.prune(now)
	return len(w.samples)
}

// AboveFor reports whether CPU has been at or above pct for the whole
// exit-hold period, with enough samples to cover it.
func (w *CPUWindow) AboveFor(now time.Time, pct float64, hold time.Duration) bool {
	if hold <= 0 || len(w.samples) < 2 {
		return false
	}
	start := now.Add(-hold)
	var first time.Time
	for _, s := range w.samples {
		if s.at.Before(start) {
			continue
		}
		if first.IsZero() {
			first = s.at
		}
		if s.pct < pct {
			return false
		}
	}
	if first.IsZero() || first.After(start) {
		return false
	}
	return true
}

// CanTransition reports whether from -> to is a legal lifecycle transition.
// See docs/DESIGN.md §5.2; transitions are deliberately strict so a bug cannot skip
// the sleep barrier or wake path.
func CanTransition(from, to types.State) bool {
	if from == to {
		return true
	}
	allowed := map[types.State]map[types.State]bool{
		types.StateCreating: {types.StateStarting: true, types.StateFailed: true},
		types.StateStarting: {types.StateRunning: true, types.StateFailed: true},
		types.StateRunning:  {types.StateIdle: true, types.StateStopping: true, types.StateFailed: true},
		types.StateIdle:     {types.StateRunning: true, types.StatePaused: true, types.StateStopping: true, types.StateFailed: true},
		types.StatePaused:   {types.StateRunning: true, types.StateDormant: true, types.StateStopping: true, types.StateFailed: true},
		types.StateDormant:  {types.StateRunning: true, types.StateStopping: true, types.StateFailed: true},
		types.StateResuming: {types.StateRunning: true, types.StateFailed: true},
		types.StateStopping: {types.StateStopped: true, types.StateFailed: true},
		types.StateStopped:  {},
		types.StateFailed:   {types.StateStopping: true},
	}
	return allowed[from][to]
}

// ValidateTransition returns an error for an illegal transition.
func ValidateTransition(from, to types.State) error {
	if CanTransition(from, to) {
		return nil
	}
	return fmt.Errorf("illegal transition %s -> %s", from, to)
}

// WorkloadExitTarget maps the on_workload_exit policy to a target state.
func WorkloadExitTarget(policy string) (types.State, error) {
	switch policy {
	case "", "idle":
		return types.StateIdle, nil
	case "pause":
		return types.StatePaused, nil
	case "offload":
		return types.StateDormant, nil
	case "stop":
		return types.StateStopping, nil
	default:
		return "", fmt.Errorf("unknown on_workload_exit policy %q", policy)
	}
}
