package plugin

import (
	"context"
	"testing"
	"time"

	abi "github.com/GoCraft-MC/gocraft-abi/abi/v1"
	"github.com/GoCraft-MC/gocraft-abi/gcpkg"
)

// slowFirstDispatch answers slowly once and quickly ever after, which is what a
// runtime does: the first dispatch to reach a handler initialises the plugin's
// classes and runs its code interpreted, and every one after it does neither.
func slowFirstDispatch(t *testing.T, bus *Bus, first time.Duration) *int {
	t.Helper()
	calls := new(int)
	instance := &fakeInstance{
		manifest: gcpkg.Manifest{ID: "protect", Subscriptions: []gcpkg.Subscription{
			{Event: "block.break"},
		}},
		dispatch: func(ctx context.Context, _ *abi.Event) (abi.Verdict, error) {
			*calls++
			if *calls > 1 {
				return abi.Verdict{}, nil
			}
			select {
			case <-time.After(first):
				return abi.Verdict{}, nil
			case <-ctx.Done():
				return abi.Verdict{}, ctx.Err()
			}
		},
	}
	if err := bus.Attach(instance); err != nil {
		t.Fatal(err)
	}
	return calls
}

// The first dispatch of a type to a subscriber costs milliseconds where every
// one after it costs microseconds, and none of that is the plugin being slow.
// Charging it to the shared budget logs a deadline on a healthy server once per
// restart, and cancels the first action guarded by a fail_closed event.
func TestAFirstDispatchGetsTheColdGrace(t *testing.T) {
	bus := NewBus(context.Background(), 2*time.Millisecond)
	bus.SetColdGrace(200 * time.Millisecond)
	slowFirstDispatch(t, bus, 20*time.Millisecond)

	if !bus.EmitCancellable(&abi.Event{Type: "block.break", OnFailure: abi.FailureDeny}) {
		t.Fatal("EmitCancellable() denied an event its subscriber answered inside the grace")
	}
	health, _ := bus.Health("protect")
	if health.Failures != 0 || health.Starved["block.break"] != 0 {
		t.Fatalf("a cold first dispatch was charged as a fault: %+v", health)
	}
}

// Spent once. A plugin that is slow every time is slow, and the second dispatch
// is judged by the budget §06 gives it — otherwise the grace would be a way to
// hold the tick rather than a way to start.
func TestTheColdGraceIsSpentOnce(t *testing.T) {
	bus := NewBus(context.Background(), 2*time.Millisecond)
	bus.SetColdGrace(200 * time.Millisecond)
	instance := &fakeInstance{
		manifest: gcpkg.Manifest{ID: "slow", Subscriptions: []gcpkg.Subscription{
			{Event: "block.break"},
		}},
		dispatch: func(ctx context.Context, _ *abi.Event) (abi.Verdict, error) {
			<-ctx.Done()
			return abi.Verdict{}, ctx.Err()
		},
	}
	if err := bus.Attach(instance); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	bus.EmitCancellable(&abi.Event{Type: "block.break", OnFailure: abi.FailureAllow})
	cold := time.Since(started)
	started = time.Now()
	bus.EmitCancellable(&abi.Event{Type: "block.break", OnFailure: abi.FailureAllow})
	warm := time.Since(started)

	if cold < 100*time.Millisecond {
		t.Fatalf("the first dispatch waited %v, want the grace", cold)
	}
	if warm > 50*time.Millisecond {
		t.Fatalf("the second dispatch waited %v, want only the budget", warm)
	}
}

// Zero is off, and has to be: an admin who does not want the tick to move at
// all should be able to say so and get §06 unmodified.
func TestAZeroColdGraceRestoresTheSharedBudget(t *testing.T) {
	bus := NewBus(context.Background(), 2*time.Millisecond)
	bus.SetColdGrace(0)
	slowFirstDispatch(t, bus, 50*time.Millisecond)

	started := time.Now()
	bus.EmitCancellable(&abi.Event{Type: "block.break", OnFailure: abi.FailureAllow})
	if took := time.Since(started); took > 30*time.Millisecond {
		t.Fatalf("EmitCancellable() waited %v with no grace, want the budget alone", took)
	}
}

// The grace is the event's, not each subscriber's, exactly as the budget is:
// ten plugins subscribing to one event must not turn it into ten graces.
func TestTheColdGraceIsAddedOncePerEvent(t *testing.T) {
	bus := NewBus(context.Background(), 2*time.Millisecond)
	bus.SetColdGrace(60 * time.Millisecond)
	for _, id := range []string{"one", "two", "three"} {
		instance := &fakeInstance{
			manifest: gcpkg.Manifest{ID: id, Subscriptions: []gcpkg.Subscription{
				{Event: "block.break"},
			}},
			dispatch: func(ctx context.Context, _ *abi.Event) (abi.Verdict, error) {
				<-ctx.Done()
				return abi.Verdict{}, ctx.Err()
			},
		}
		if err := bus.Attach(instance); err != nil {
			t.Fatal(err)
		}
	}

	started := time.Now()
	bus.EmitCancellable(&abi.Event{Type: "block.break", OnFailure: abi.FailureAllow})
	if took := time.Since(started); took > 150*time.Millisecond {
		t.Fatalf("three cold subscribers cost %v, want one grace", took)
	}
}
