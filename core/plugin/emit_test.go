package plugin

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	abi "github.com/GoCraft-MC/gocraft-abi/abi/v1"
	"github.com/GoCraft-MC/gocraft-abi/gcpkg"
)

// shopPurchase is §10's event, with a fixed list of mutable tiers.
var shopPurchase = gcpkg.EventDefinition{
	Type: "fr.oreo.shop/purchase", Cancellable: true,
	Fields: []gcpkg.EventField{
		{Name: "player", Type: "PlayerRef", Mutable: false},
		{Name: "tiers", Type: "[]fr.oreo.Tier", Mutable: false},
		{Name: "price", Type: "double", Mutable: true},
	},
}

func purchaseFields() []abi.Value {
	return []abi.Value{
		abi.String("oreo"),
		abi.List(abi.List(abi.String("gold"), abi.Double(19.99))),
		abi.Double(1500),
	}
}

func purchaseEmission() abi.Emission {
	return abi.Emission{PluginID: "fr.oreo.shop", TypeID: 1, Fields: purchaseFields()}
}

// subscriber attaches one plugin to the event, answering with verdict and
// recording what it was handed.
func subscribe(t *testing.T, bus *Bus, id string, priority gcpkg.Priority,
	answer func(*abi.Event) (abi.Verdict, error)) *[]*abi.Event {
	t.Helper()
	seen := new([]*abi.Event)
	instance := &fakeInstance{
		manifest: gcpkg.Manifest{ID: id, Subscriptions: []gcpkg.Subscription{
			{Event: shopPurchase.Type, Priority: priority},
		}},
		dispatch: func(_ context.Context, event *abi.Event) (abi.Verdict, error) {
			*seen = append(*seen, event)
			return answer(event)
		},
	}
	if err := bus.Attach(instance); err != nil {
		t.Fatal(err)
	}
	return seen
}

func allow(*abi.Event) (abi.Verdict, error) { return abi.Verdict{}, nil }

// emitBus is a bus with a budget no test can trip over.
//
// The default is 2ms — the shared budget one cancellable event gets across
// every subscriber — and a suite running under load exceeds it between two
// dispatches. A test asserting on sequencing would then fail as starvation,
// blaming the code for the machine. dispatch_test.go does the same, and the
// starvation path has its own tests that ask for a short budget on purpose.
func emitBus() *Bus { return NewBus(context.Background(), time.Second) }

// The same race as dispatch_test.go's, on the path where losing it costs most.
//
// A fail_closed event is cancelled when its budget runs out, so a verdict
// discarded for arriving on the deadline refuses a purchase nobody refused —
// once per restart, and invisibly, because the log would call it starvation.
func TestALateVerdictStillDecidesAFailClosedEvent(t *testing.T) {
	definition := shopPurchase
	definition.FailClosed = true
	bus := NewBus(context.Background(), 5*time.Millisecond)
	instance := &fakeInstance{
		manifest: gcpkg.Manifest{ID: "fr.oreo.discount", Subscriptions: []gcpkg.Subscription{
			{Event: definition.Type},
		}},
		dispatch: func(ctx context.Context, _ *abi.Event) (abi.Verdict, error) {
			<-ctx.Done()
			return abi.Verdict{Mutations: []abi.Mutation{
				{Path: []uint32{2}, Value: abi.Double(1200)},
			}}, nil
		},
	}
	if err := bus.Attach(instance); err != nil {
		t.Fatal(err)
	}

	result := bus.EmitCustom(definition, purchaseEmission())

	if result.Cancelled {
		t.Fatal("EmitCustom() cancelled a fail_closed event whose subscriber answered")
	}
	if len(result.Mutations) != 1 || result.Mutations[0].Value.Double != 1200 {
		t.Fatalf("EmitCustom() = %+v, want the discount the subscriber applied", result)
	}
}

func TestEmitCustomWithNoSubscribersChangesNothing(t *testing.T) {
	bus := emitBus()
	result := bus.EmitCustom(shopPurchase, purchaseEmission())
	if result.Cancelled || len(result.Mutations) != 0 || result.Error != "" {
		t.Fatalf("EmitCustom() = %+v, want an untouched event", result)
	}
}

// The emitter is the author of the state, not an observer of it: hearing its
// own event back would have it count the purchase twice.
func TestEmitCustomSkipsTheEmitter(t *testing.T) {
	bus := emitBus()
	own := subscribe(t, bus, "fr.oreo.shop", gcpkg.PriorityNormal, allow)
	other := subscribe(t, bus, "fr.oreo.discount", gcpkg.PriorityNormal, allow)

	bus.EmitCustom(shopPurchase, purchaseEmission())

	if len(*own) != 0 {
		t.Fatalf("the emitter received its own event %d times", len(*own))
	}
	if len(*other) != 1 {
		t.Fatalf("the other subscriber received %d events, want 1", len(*other))
	}
}

// Subscriber two sees what subscriber one left. This is the whole of §10's
// mutation model, and the reason nothing has to be merged.
func TestEmitCustomCarriesStateFromOneSubscriberToTheNext(t *testing.T) {
	bus := emitBus()
	discount := subscribe(t, bus, "fr.oreo.discount", gcpkg.PriorityHigh,
		func(*abi.Event) (abi.Verdict, error) {
			return abi.Verdict{Mutations: []abi.Mutation{
				{Path: []uint32{2}, Value: abi.Double(1200)},
			}}, nil
		})
	capChecker := subscribe(t, bus, "fr.oreo.cap", gcpkg.PriorityLow, allow)

	result := bus.EmitCustom(shopPurchase, purchaseEmission())

	if got := (*discount)[0].Fields[2].Double; got != 1500 {
		t.Fatalf("the first subscriber saw a price of %v, want the original 1500", got)
	}
	if got := (*capChecker)[0].Fields[2].Double; got != 1200 {
		t.Fatalf("the second subscriber saw a price of %v, want the discounted 1200", got)
	}
	if len(result.Mutations) != 1 || result.Mutations[0].Value.Double != 1200 {
		t.Fatalf("EmitCustom() returned %+v, want the price change to replay", result.Mutations)
	}
}

// A fixed list of mutable records is the common case, not an exotic one: the
// list may not be swapped, its contents may be written.
func TestEmitCustomAcceptsADeepWriteIntoAFixedList(t *testing.T) {
	bus := emitBus()
	subscribe(t, bus, "fr.oreo.discount", gcpkg.PriorityHigh,
		func(*abi.Event) (abi.Verdict, error) {
			return abi.Verdict{Mutations: []abi.Mutation{
				{Path: []uint32{1, 0, 1}, Value: abi.Double(15.99)},
			}}, nil
		})
	witness := subscribe(t, bus, "fr.oreo.cap", gcpkg.PriorityLow, allow)

	result := bus.EmitCustom(shopPurchase, purchaseEmission())

	if got := (*witness)[0].Fields[1].List[0].List[1].Double; got != 15.99 {
		t.Fatalf("the nested price reached the next subscriber as %v", got)
	}
	if len(result.Mutations) != 1 {
		t.Fatalf("EmitCustom() returned %+v, want the deep write to replay", result.Mutations)
	}
}

func TestEmitCustomRefusesAWriteToAReadOnlyField(t *testing.T) {
	bus := emitBus()
	subscribe(t, bus, "fr.oreo.rogue", gcpkg.PriorityHigh,
		func(*abi.Event) (abi.Verdict, error) {
			return abi.Verdict{Mutations: []abi.Mutation{
				{Path: []uint32{0}, Value: abi.String("someone-else")},
				{Path: []uint32{2}, Value: abi.Double(1)},
			}}, nil
		})
	witness := subscribe(t, bus, "fr.oreo.cap", gcpkg.PriorityLow, allow)

	result := bus.EmitCustom(shopPurchase, purchaseEmission())

	if got := (*witness)[0].Fields[0].String; got != "oreo" {
		t.Fatalf("the read-only player was rewritten to %q", got)
	}
	// Refused, not fatal: the legal write in the same verdict still lands, and
	// the event carries on.
	if len(result.Mutations) != 1 || result.Mutations[0].Value.Double != 1 {
		t.Fatalf("EmitCustom() returned %+v, want only the legal write", result.Mutations)
	}
	if result.Cancelled {
		t.Fatal("a refused write cancelled the event, which would make it a veto")
	}
}

func TestEmitCustomStopsAtACancellation(t *testing.T) {
	bus := emitBus()
	subscribe(t, bus, "fr.oreo.guard", gcpkg.PriorityHigh,
		func(*abi.Event) (abi.Verdict, error) {
			return abi.Verdict{Cancelled: true, Mutations: []abi.Mutation{
				{Path: []uint32{2}, Value: abi.Double(0)},
			}}, nil
		})
	later := subscribe(t, bus, "fr.oreo.cap", gcpkg.PriorityLow, allow)

	result := bus.EmitCustom(shopPurchase, purchaseEmission())

	if !result.Cancelled {
		t.Fatal("EmitCustom() did not report the cancellation")
	}
	if len(*later) != 0 {
		t.Fatal("a subscriber after the cancellation still ran")
	}
	// The emitter has to end up where the dispatch got to, cancelled or not.
	if len(result.Mutations) != 1 {
		t.Fatalf("EmitCustom() dropped the mutations of a cancelled event: %+v", result.Mutations)
	}
}

func TestEmitCustomIgnoresACancellationOnANonCancellableEvent(t *testing.T) {
	definition := shopPurchase
	definition.Cancellable = false
	bus := emitBus()
	subscribe(t, bus, "fr.oreo.guard", gcpkg.PriorityHigh,
		func(*abi.Event) (abi.Verdict, error) { return abi.Verdict{Cancelled: true}, nil })
	later := subscribe(t, bus, "fr.oreo.cap", gcpkg.PriorityLow, allow)

	result := bus.EmitCustom(definition, purchaseEmission())

	if result.Cancelled {
		t.Fatal("EmitCustom() cancelled an event its provider declared uncancellable")
	}
	if len(*later) != 1 {
		t.Fatal("the subscribers after the attempted cancellation were skipped")
	}
}

func TestEmitCustomSurvivesAFailingSubscriber(t *testing.T) {
	bus := emitBus()
	subscribe(t, bus, "fr.oreo.broken", gcpkg.PriorityHigh,
		func(*abi.Event) (abi.Verdict, error) { return abi.Verdict{}, fmt.Errorf("boom") })
	later := subscribe(t, bus, "fr.oreo.cap", gcpkg.PriorityLow, allow)

	result := bus.EmitCustom(shopPurchase, purchaseEmission())

	if result.Cancelled {
		t.Fatal("a failing subscriber cancelled an event that is not fail-closed")
	}
	if len(*later) != 1 {
		t.Fatal("a failing subscriber stopped the ones after it")
	}
}

// FailClosed is the on_failure = DENY of §06: for an event that guards
// something, losing a subscriber is not survivable.
func TestEmitCustomCancelsWhenAFailClosedSubscriberFails(t *testing.T) {
	definition := shopPurchase
	definition.FailClosed = true
	bus := emitBus()
	subscribe(t, bus, "fr.oreo.broken", gcpkg.PriorityHigh,
		func(*abi.Event) (abi.Verdict, error) { return abi.Verdict{}, fmt.Errorf("boom") })
	later := subscribe(t, bus, "fr.oreo.cap", gcpkg.PriorityLow, allow)

	result := bus.EmitCustom(definition, purchaseEmission())

	if !result.Cancelled {
		t.Fatal("EmitCustom() let a fail-closed event through after a subscriber failed")
	}
	if len(*later) != 0 {
		t.Fatal("a fail-closed event kept dispatching after the failure")
	}
}

// The policy travels with the event so the runtime can log an overrun against
// the right expectation.
func TestEmitCustomTellsSubscribersTheFailurePolicy(t *testing.T) {
	definition := shopPurchase
	definition.FailClosed = true
	bus := emitBus()
	seen := subscribe(t, bus, "fr.oreo.cap", gcpkg.PriorityNormal, allow)

	bus.EmitCustom(definition, purchaseEmission())

	event := (*seen)[0]
	if event.OnFailure != abi.FailureDeny {
		t.Fatalf("the subscriber was told on_failure = %v", event.OnFailure)
	}
	if event.TypeID != 1 || event.Type != shopPurchase.Type {
		t.Fatalf("the subscriber received %q / id %d", event.Type, event.TypeID)
	}
}

func registryWith(t *testing.T, bundles ...Bundle) *Registry {
	t.Helper()
	registry := NewRegistry(context.Background(), time.Second, nil, nil)
	if err := registry.RegisterRuntime(&fakeRuntime{name: "go"}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Preflight(context.Background(), bundles); err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestEmitFromRefusesWhatTheRegistryDoesNotKnow(t *testing.T) {
	registry := registryWith(t, providing("fr.oreo.shop", shopPurchase))
	cases := []struct {
		name     string
		emission abi.Emission
		want     string
	}{{
		name:     "an id nothing is bound to",
		emission: abi.Emission{PluginID: "fr.oreo.shop", TypeID: 9},
		want:     "unknown event type id 9",
	}, {
		name:     "a plugin publishing another plugin's event",
		emission: abi.Emission{PluginID: "fr.oreo.rogue", TypeID: 1, Fields: purchaseFields()},
		want:     "cannot emit",
	}, {
		name:     "a payload that is not the declared shape",
		emission: abi.Emission{PluginID: "fr.oreo.shop", TypeID: 1, Fields: []abi.Value{abi.Double(1)}},
		want:     "declares 3",
	}}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			result := registry.EmitFrom(context.Background(), test.emission)
			if !strings.Contains(result.Error, test.want) {
				t.Fatalf("EmitFrom() error = %q, want it to mention %q", result.Error, test.want)
			}
		})
	}
}

func TestEmitFromDispatchesAKnownEmission(t *testing.T) {
	registry := registryWith(t, providing("fr.oreo.shop", shopPurchase))
	seen := subscribe(t, registry.Bus(), "fr.oreo.discount", gcpkg.PriorityNormal,
		func(*abi.Event) (abi.Verdict, error) {
			return abi.Verdict{Mutations: []abi.Mutation{
				{Path: []uint32{2}, Value: abi.Double(1200)},
			}}, nil
		})

	result := registry.EmitFrom(context.Background(), purchaseEmission())

	if result.Error != "" {
		t.Fatalf("EmitFrom() = %q", result.Error)
	}
	if len(*seen) != 1 {
		t.Fatalf("the subscriber received %d events", len(*seen))
	}
	if len(result.Mutations) != 1 || result.Mutations[0].Value.Double != 1200 {
		t.Fatalf("EmitFrom() returned %+v", result.Mutations)
	}
}
