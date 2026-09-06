package plugin

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	abi "github.com/GoCraft-MC/gocraft-abi/abi/v1"

	"github.com/GoCraft-MC/gocraft-abi/gcpkg"
)

func TestEmitCancellableUsesPriorityOrder(t *testing.T) {
	bus := NewBus(context.Background(), time.Second)
	var calls []string
	for _, tc := range []struct {
		id       string
		priority gcpkg.Priority
		cancel   bool
	}{{"last", gcpkg.PriorityLow, false}, {"first", gcpkg.PriorityHigh, false}, {"stop", gcpkg.PriorityNormal, true}} {
		tc := tc
		instance := &fakeInstance{
			manifest: gcpkg.Manifest{ID: tc.id, Subscriptions: []gcpkg.Subscription{{Event: "block.break", Priority: tc.priority}}},
			dispatch: func(context.Context, *abi.Event) (abi.Verdict, error) {
				calls = append(calls, tc.id)
				return abi.Verdict{Cancelled: tc.cancel}, nil
			},
		}
		if err := bus.Attach(instance); err != nil {
			t.Fatal(err)
		}
	}
	if bus.EmitCancellable(&abi.Event{Type: "block.break"}) {
		t.Fatal("cancelled event was allowed")
	}
	if want := []string{"first", "stop"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestEmitCancellableAppliesFailurePolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy abi.FailurePolicy
		allow  bool
	}{{"allow", abi.FailureAllow, true}, {"deny", abi.FailureDeny, false}} {
		t.Run(tc.name, func(t *testing.T) {
			bus := NewBus(context.Background(), time.Second)
			instance := &fakeInstance{
				manifest: gcpkg.Manifest{ID: "broken", Subscriptions: []gcpkg.Subscription{{Event: "block.break"}}},
				dispatch: func(context.Context, *abi.Event) (abi.Verdict, error) {
					return abi.Verdict{}, errors.New("runtime stopped")
				},
			}
			if err := bus.Attach(instance); err != nil {
				t.Fatal(err)
			}
			if got := bus.EmitCancellable(&abi.Event{Type: "block.break", OnFailure: tc.policy}); got != tc.allow {
				t.Fatalf("EmitCancellable() = %v, want %v", got, tc.allow)
			}
		})
	}
}

func TestEventDeadlineStopsRemainingSubscribers(t *testing.T) {
	bus := NewBus(context.Background(), 5*time.Millisecond)
	lateCalled := false
	slow := &fakeInstance{
		manifest: gcpkg.Manifest{ID: "slow", Subscriptions: []gcpkg.Subscription{{Event: "block.break", Priority: gcpkg.PriorityHigh}}},
		dispatch: func(ctx context.Context, _ *abi.Event) (abi.Verdict, error) {
			<-ctx.Done()
			return abi.Verdict{}, ctx.Err()
		},
	}
	late := &fakeInstance{
		manifest: gcpkg.Manifest{ID: "late", Subscriptions: []gcpkg.Subscription{{Event: "block.break", Priority: gcpkg.PriorityLow}}},
		dispatch: func(context.Context, *abi.Event) (abi.Verdict, error) {
			lateCalled = true
			return abi.Verdict{}, nil
		},
	}
	for _, instance := range []*fakeInstance{slow, late} {
		if err := bus.Attach(instance); err != nil {
			t.Fatal(err)
		}
	}
	if !bus.EmitCancellable(&abi.Event{Type: "block.break", OnFailure: abi.FailureAllow}) {
		t.Fatal("fail-open deadline denied the event")
	}
	if lateCalled {
		t.Fatal("subscriber ran after the shared budget expired")
	}
	slowHealth, _ := bus.Health("slow")
	if slowHealth.Failures != 1 || slowHealth.Starved["block.break"] != 0 {
		t.Fatalf("slow plugin health = %+v", slowHealth)
	}
	lateHealth, _ := bus.Health("late")
	if lateHealth.Failures != 0 || lateHealth.Starved["block.break"] != 1 {
		t.Fatalf("late plugin health = %+v", lateHealth)
	}
}

// A verdict that arrives as the deadline fires is still a verdict.
//
// §06 charges "only a subscriber during whose own call the deadline fires", and
// this one answered: the race between its reply landing and the timer running
// is not something it did. Deciding the event on the context instead would drop
// a cancellation that was made, which on the first dispatch into a cold runtime
// is every restart.
func TestAVerdictThatLandsOnTheDeadlineIsHonoured(t *testing.T) {
	queue := NewMutationQueue()
	bus := newBus(context.Background(), 5*time.Millisecond, queue)
	instance := &fakeInstance{
		manifest: gcpkg.Manifest{ID: "protect", Subscriptions: []gcpkg.Subscription{{Event: "block.break"}}},
		dispatch: func(ctx context.Context, _ *abi.Event) (abi.Verdict, error) {
			// The answer was already on its way when the budget ran out.
			<-ctx.Done()
			return abi.Verdict{Cancelled: true, Effects: []abi.HostCall{
				{Type: "message", Fields: []abi.Value{abi.String("Protected area.")}},
			}}, nil
		},
	}
	if err := bus.Attach(instance); err != nil {
		t.Fatal(err)
	}
	if bus.EmitCancellable(&abi.Event{Type: "block.break", OnFailure: abi.FailureAllow}) {
		t.Fatal("EmitCancellable() allowed an event its subscriber cancelled")
	}
	health, _ := bus.Health("protect")
	if health.Failures != 0 || health.Starved["block.break"] != 0 {
		t.Fatalf("EmitCancellable() charged a subscriber that answered: %+v", health)
	}
	count, err := queue.Drain(func(abi.HostCall) error { return nil })
	if err != nil || count != 1 {
		t.Fatalf("Drain() = %d, %v, want the one effect of a verdict that arrived", count, err)
	}
}

func TestEventVerdictQueuesBatchedEffects(t *testing.T) {
	queue := NewMutationQueue()
	bus := newBus(context.Background(), time.Second, queue)
	instance := &fakeInstance{
		manifest: gcpkg.Manifest{ID: "protect", Subscriptions: []gcpkg.Subscription{{Event: "block.break"}}},
		dispatch: func(context.Context, *abi.Event) (abi.Verdict, error) {
			return abi.Verdict{
				Cancelled: true,
				Effects: []abi.HostCall{
					{Type: "message", Fields: []abi.Value{abi.String("Protected area.")}},
					{Type: "sound", Fields: []abi.Value{abi.String("deny")}},
				},
			}, nil
		},
	}
	if err := bus.Attach(instance); err != nil {
		t.Fatal(err)
	}
	if bus.EmitCancellable(&abi.Event{Type: "block.break"}) {
		t.Fatal("cancelled event was allowed")
	}
	var effects []string
	count, err := queue.Drain(func(call abi.HostCall) error {
		effects = append(effects, call.Type)
		return nil
	})
	if err != nil || count != 2 {
		t.Fatalf("Drain() = %d, %v", count, err)
	}
	if want := []string{"message", "sound"}; !reflect.DeepEqual(effects, want) {
		t.Fatalf("effects = %v, want %v", effects, want)
	}
}
