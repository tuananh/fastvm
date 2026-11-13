// Package wake implements the wake dispatcher (docs/DESIGN.md §6.2, §5.5).
//
// It consumes wake notifications from the network data plane (the eBPF switch
// ringbuf in eBPF mode, the AF_PACKET sniffer in bridge mode), coalesces a burst
// of frames for one sandbox into a single wake transition, rate-limits
// scan/wake-storm traffic, and hands the sandbox ID to a callback that drives
// the lifecycle actor.
//
// The dispatcher never holds, injects, replays or reorders frames: the frame
// that triggered the event is already flowing into the sandbox's live
// virtio-net queue when the callback runs.
package wake

import (
	"context"
	"sync"
	"time"

	"fastvm/internal/network"
)

// Defaults mirror docs/DESIGN.md §5.4.
const (
	DefaultCoalesceWindow    = 100 * time.Millisecond
	DefaultMaxWakesPerMinute = 6
)

// Options configures a Dispatcher.
type Options struct {
	// Coalesce is the per-sandbox burst window: after a wake is dispatched,
	// further events for that sandbox are dropped until the window elapses.
	// Zero selects DefaultCoalesceWindow; negative disables coalescing.
	Coalesce time.Duration
	// MaxPerMinute caps dispatched wakes per sandbox in any trailing minute.
	// Zero selects DefaultMaxWakesPerMinute; negative disables the limit.
	MaxPerMinute int
	// Clock is the time source; nil selects time.Now. Tests inject a fake.
	Clock func() time.Time
}

// Stats are dispatcher counters for observability/metrics.
type Stats struct {
	Events         uint64 // wake notifications observed
	Emitted        uint64 // callbacks dispatched
	Coalesced      uint64 // suppressed because a wake already fired in the window
	RateLimited    uint64 // suppressed by the per-minute cap
	CallbackErrors uint64 // callback returned an error (dispatch continues)
}

type sandboxState struct {
	lastEmit time.Time
	recent   []time.Time // emission times inside the trailing minute
}

// Dispatcher coalesces and rate-limits wake events. It is safe for concurrent
// use by Run and Offer, but one Run per Dispatcher is expected.
type Dispatcher struct {
	coalesce     time.Duration
	maxPerMinute int
	clock        func() time.Time

	mu         sync.Mutex
	perSandbox map[string]*sandboxState
	stats      Stats
}

// NewDispatcher builds a Dispatcher.
func NewDispatcher(opts Options) *Dispatcher {
	coalesce := opts.Coalesce
	switch {
	case coalesce == 0:
		coalesce = DefaultCoalesceWindow
	case coalesce < 0:
		coalesce = 0
	}
	maxPerMinute := opts.MaxPerMinute
	switch {
	case maxPerMinute == 0:
		maxPerMinute = DefaultMaxWakesPerMinute
	case maxPerMinute < 0:
		maxPerMinute = 0
	}
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Dispatcher{
		coalesce:     coalesce,
		maxPerMinute: maxPerMinute,
		clock:        clock,
		perSandbox:   make(map[string]*sandboxState),
	}
}

// Offer records one wake notification and reports whether it should be
// dispatched to the callback. The first qualifying event of a burst is
// dispatched immediately (fast wake); later events inside the coalesce window
// and events over the rate limit are dropped and counted.
func (d *Dispatcher) Offer(ev network.WakeEvent) bool {
	if ev.SandboxID == "" {
		return false
	}
	now := d.clock()

	d.mu.Lock()
	defer d.mu.Unlock()

	d.stats.Events++
	st, ok := d.perSandbox[ev.SandboxID]
	if !ok {
		st = &sandboxState{}
		d.perSandbox[ev.SandboxID] = st
	}
	st.prune(now)

	if d.coalesce > 0 && !st.lastEmit.IsZero() {
		if delta := now.Sub(st.lastEmit); delta >= 0 && delta < d.coalesce {
			d.stats.Coalesced++
			return false
		}
	}
	if d.maxPerMinute > 0 && len(st.recent) >= d.maxPerMinute {
		d.stats.RateLimited++
		return false
	}

	st.lastEmit = now
	if d.maxPerMinute > 0 {
		st.recent = append(st.recent, now)
	}
	d.stats.Emitted++
	return true
}

// prune drops emission timestamps older than one minute (rate-limit window).
func (st *sandboxState) prune(now time.Time) {
	if len(st.recent) == 0 {
		return
	}
	cutoff := now.Add(-time.Minute)
	i := 0
	for i < len(st.recent) && st.recent[i].Before(cutoff) {
		i++
	}
	if i > 0 {
		st.recent = append(st.recent[:0], st.recent[i:]...)
	}
}

// Run consumes wake events until the channel closes or ctx is cancelled and
// calls fn once per dispatched wake. Callback errors are counted and do not
// stop dispatch. Run returns nil on clean shutdown.
func (d *Dispatcher) Run(ctx context.Context, in <-chan network.WakeEvent, fn func(ctx context.Context, sandboxID string) error) error {
	if fn == nil {
		fn = func(context.Context, string) error { return nil }
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-in:
			if !ok {
				return nil
			}
			if !d.Offer(ev) {
				continue
			}
			if err := fn(ctx, ev.SandboxID); err != nil {
				d.mu.Lock()
				d.stats.CallbackErrors++
				d.mu.Unlock()
			}
		}
	}
}

// Forget drops all state for a sandbox (call on destroy to bound memory).
func (d *Dispatcher) Forget(sandboxID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.perSandbox, sandboxID)
}

// Stats returns a snapshot of the dispatcher counters.
func (d *Dispatcher) Stats() Stats {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stats
}
