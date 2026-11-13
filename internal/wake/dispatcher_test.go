package wake

import (
	"context"
	"errors"
	"testing"
	"time"

	"fastvm/internal/network"
)

// fakeClock is a controllable clock for deterministic tests.
type fakeClock struct {
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func wakeEvent(id string) network.WakeEvent {
	return network.WakeEvent{SandboxID: id, Time: time.Time{}}
}

func TestCoalescesBurstPerSandbox(t *testing.T) {
	clock := newFakeClock()
	d := NewDispatcher(Options{Coalesce: 100 * time.Millisecond, MaxPerMinute: -1, Clock: clock.Now})

	if !d.Offer(wakeEvent("sb1")) {
		t.Fatal("first event was not dispatched")
	}
	for i := 0; i < 4; i++ {
		clock.Advance(10 * time.Millisecond)
		if d.Offer(wakeEvent("sb1")) {
			t.Fatalf("event %d inside coalesce window was dispatched", i)
		}
	}
	// A different sandbox is independent.
	clock.Advance(10 * time.Millisecond)
	if !d.Offer(wakeEvent("sb2")) {
		t.Fatal("event for another sandbox was coalesced")
	}
	// After the window a new wake is dispatched.
	clock.Advance(100 * time.Millisecond)
	if !d.Offer(wakeEvent("sb1")) {
		t.Fatal("event after coalesce window was not dispatched")
	}

	got := d.Stats()
	if got.Events != 7 {
		t.Errorf("Events = %d, want 7", got.Events)
	}
	if got.Emitted != 3 {
		t.Errorf("Emitted = %d, want 3", got.Emitted)
	}
	if got.Coalesced != 4 {
		t.Errorf("Coalesced = %d, want 4", got.Coalesced)
	}
}

func TestRateLimitPerSandbox(t *testing.T) {
	clock := newFakeClock()
	// Negative Coalesce disables the burst window so the rate limit is the only
	// gate.
	d := NewDispatcher(Options{Coalesce: -1, MaxPerMinute: 3, Clock: clock.Now})

	for i := 0; i < 5; i++ {
		clock.Advance(time.Second)
		d.Offer(wakeEvent("sb1"))
	}
	got := d.Stats()
	if got.Emitted != 3 {
		t.Errorf("Emitted = %d, want 3", got.Emitted)
	}
	if got.RateLimited != 2 {
		t.Errorf("RateLimited = %d, want 2", got.RateLimited)
	}

	// Events for another sandbox are not affected by sb1's limit.
	if !d.Offer(wakeEvent("sb2")) {
		t.Error("rate limit leaked across sandboxes")
	}

	// After the trailing minute the budget refills.
	clock.Advance(61 * time.Second)
	if !d.Offer(wakeEvent("sb1")) {
		t.Error("wake after rate-limit window was not dispatched")
	}
}

func TestRateLimitWindowSlides(t *testing.T) {
	clock := newFakeClock()
	d := NewDispatcher(Options{Coalesce: -1, MaxPerMinute: 2, Clock: clock.Now})

	if !d.Offer(wakeEvent("sb1")) { // t=0
		t.Fatal("event 1 not dispatched")
	}
	clock.Advance(10 * time.Second)
	if !d.Offer(wakeEvent("sb1")) { // t=10s
		t.Fatal("event 2 not dispatched")
	}
	clock.Advance(10 * time.Second)
	if d.Offer(wakeEvent("sb1")) { // t=20s, limit 2/min
		t.Fatal("event 3 dispatched over the rate limit")
	}
	// t=61.1s: only the first timestamp has left the trailing window.
	clock.Advance(41*time.Second + 100*time.Millisecond)
	if !d.Offer(wakeEvent("sb1")) {
		t.Fatal("event not dispatched after the oldest emission aged out")
	}
}

func TestCoalesceWindowDefaults(t *testing.T) {
	d := NewDispatcher(Options{})
	if d.coalesce != DefaultCoalesceWindow {
		t.Errorf("coalesce = %v, want %v", d.coalesce, DefaultCoalesceWindow)
	}
	if d.maxPerMinute != DefaultMaxWakesPerMinute {
		t.Errorf("maxPerMinute = %d, want %d", d.maxPerMinute, DefaultMaxWakesPerMinute)
	}
	if NewDispatcher(Options{Coalesce: -1, MaxPerMinute: -1}).maxPerMinute != 0 {
		t.Error("negative MaxPerMinute should disable the limit")
	}
}

func TestClockGoingBackwardsDoesNotWedge(t *testing.T) {
	clock := newFakeClock()
	d := NewDispatcher(Options{Coalesce: time.Second, MaxPerMinute: -1, Clock: clock.Now})
	if !d.Offer(wakeEvent("sb1")) {
		t.Fatal("first event not dispatched")
	}
	clock.Advance(-time.Minute)
	if !d.Offer(wakeEvent("sb1")) {
		t.Fatal("backwards clock suppressed wake forever")
	}
}

func TestForgetResetsSandboxState(t *testing.T) {
	clock := newFakeClock()
	d := NewDispatcher(Options{Coalesce: time.Hour, MaxPerMinute: -1, Clock: clock.Now})
	if !d.Offer(wakeEvent("sb1")) {
		t.Fatal("first event not dispatched")
	}
	d.Forget("sb1")
	if !d.Offer(wakeEvent("sb1")) {
		t.Fatal("wake after Forget was coalesced")
	}
}

func TestEmptySandboxIDIgnored(t *testing.T) {
	d := NewDispatcher(Options{})
	if d.Offer(wakeEvent("")) {
		t.Error("empty sandbox ID dispatched")
	}
	if d.Stats().Events != 0 {
		t.Error("empty sandbox ID counted as an event")
	}
}

func TestRunDispatchesCoalescedWakes(t *testing.T) {
	clock := newFakeClock()
	events := make(chan network.WakeEvent, 8)
	events <- wakeEvent("sb1")
	events <- wakeEvent("sb1")
	events <- wakeEvent("sb1")
	events <- wakeEvent("sb2")
	close(events)

	var got []string
	done := make(chan error, 1)
	d := NewDispatcher(Options{Coalesce: 100 * time.Millisecond, MaxPerMinute: -1, Clock: clock.Now})
	go func() {
		done <- d.Run(context.Background(), events, func(_ context.Context, id string) error {
			got = append(got, id)
			return nil
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after channel close")
	}
	if len(got) != 2 || got[0] != "sb1" || got[1] != "sb2" {
		t.Errorf("callbacks = %v, want [sb1 sb2]", got)
	}
	if st := d.Stats(); st.Emitted != 2 || st.Coalesced != 2 {
		t.Errorf("stats = %+v", st)
	}
}

func TestRunCallbackErrorDoesNotStopDispatch(t *testing.T) {
	clock := newFakeClock()
	events := make(chan network.WakeEvent, 4)
	events <- wakeEvent("sb1")
	events <- wakeEvent("sb2")
	close(events)

	var calls int
	d := NewDispatcher(Options{Coalesce: -1, MaxPerMinute: -1, Clock: clock.Now})
	err := d.Run(context.Background(), events, func(context.Context, string) error {
		calls++
		return errors.New("resume failed")
	})
	if err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
	if st := d.Stats(); st.CallbackErrors != 2 {
		t.Errorf("CallbackErrors = %d, want 2", st.CallbackErrors)
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	events := make(chan network.WakeEvent)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	d := NewDispatcher(Options{})
	go func() { done <- d.Run(ctx, events, nil) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil on cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on context cancel")
	}
}
