package plugin

import (
	"context"
	"strings"
	"testing"

	"github.com/GoCraft-MC/gocraft-abi/gcpkg"
)

func providing(id string, definitions ...gcpkg.EventDefinition) Bundle {
	return Bundle{Bundle: gcpkg.Bundle{Manifest: gcpkg.Manifest{
		ID: id, Version: "1.0.0", APIVersion: gcpkg.CurrentAPIVersion,
		Runtime: "go", Provides: definitions,
	}}}
}

var purchase = gcpkg.EventDefinition{
	Type: "fr.oreo.shop/purchase", Cancellable: true, FailClosed: true,
	Fields: []gcpkg.EventField{
		{Name: "player", Type: "PlayerRef", Mutable: false},
		{Name: "price", Type: "double", Mutable: true},
	},
}

func TestNewEventTypesNumbersFromOneInNameOrder(t *testing.T) {
	// Declared out of order, and split across two plugins, so the result can
	// only come from the sort and not from the order they were scanned in.
	types, err := newEventTypes([]Bundle{
		providing("fr.oreo.zulu", gcpkg.EventDefinition{Type: "fr.oreo/zulu"}),
		providing("fr.oreo.shop", purchase, gcpkg.EventDefinition{Type: "fr.oreo/alpha"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		name string
		id   uint32
		by   string
	}{
		{"fr.oreo.shop/purchase", 1, "fr.oreo.shop"},
		{"fr.oreo/alpha", 2, "fr.oreo.shop"},
		{"fr.oreo/zulu", 3, "fr.oreo.zulu"},
	}
	if types.Len() != len(want) {
		t.Fatalf("newEventTypes() registered %d types, want %d", types.Len(), len(want))
	}
	for _, expected := range want {
		provided, ok := types.Lookup(expected.name)
		if !ok {
			t.Fatalf("Lookup(%q) found nothing", expected.name)
		}
		if provided.TypeID != expected.id || provided.Provider != expected.by {
			t.Fatalf("Lookup(%q) = id %d by %s, want id %d by %s",
				expected.name, provided.TypeID, provided.Provider, expected.id, expected.by)
		}
		if byID, ok := types.ByID(expected.id); !ok || byID.Definition.Type != expected.name {
			t.Fatalf("ByID(%d) = %+v, want %s", expected.id, byID, expected.name)
		}
	}
}

func TestNewEventTypesNeverAssignsZero(t *testing.T) {
	// Zero is what abi/v1 puts in Event.type_id for a native event, so handing
	// it to a plugin-defined type would make the two indistinguishable on the
	// wire.
	types, err := newEventTypes([]Bundle{providing("fr.oreo.shop", purchase)})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := types.ByID(0); ok {
		t.Fatal("ByID(0) resolved a plugin-defined type, which native events use")
	}
}

func TestNewEventTypesKeepsTheWholeDefinition(t *testing.T) {
	types, err := newEventTypes([]Bundle{providing("fr.oreo.shop", purchase)})
	if err != nil {
		t.Fatal(err)
	}
	provided, _ := types.Lookup(purchase.Type)
	if !provided.Definition.Cancellable || !provided.Definition.FailClosed {
		t.Fatalf("Lookup(%q).Definition = %+v, want both flags kept", purchase.Type, provided.Definition)
	}
	if !provided.Definition.MutablePath([]uint32{1}) || provided.Definition.MutablePath([]uint32{0}) {
		t.Fatalf("Lookup(%q).Definition lost its field mutability: %+v", purchase.Type, provided.Definition.Fields)
	}
}

func TestNewEventTypesRefusesTwoProvidersAndNamesBoth(t *testing.T) {
	_, err := newEventTypes([]Bundle{
		providing("fr.oreo.shop", purchase),
		providing("fr.other.shop", purchase),
	})
	if err == nil {
		t.Fatal("newEventTypes() accepted the same event from two plugins")
	}
	for _, mentioned := range []string{purchase.Type, "fr.oreo.shop", "fr.other.shop"} {
		if !strings.Contains(err.Error(), mentioned) {
			t.Fatalf("newEventTypes() error = %v, want it to name %s", err, mentioned)
		}
	}
}

func TestNewEventTypesAcceptsNoBundles(t *testing.T) {
	types, err := newEventTypes(nil)
	if err != nil {
		t.Fatal(err)
	}
	if types.Len() != 0 || len(types.All()) != 0 {
		t.Fatalf("newEventTypes(nil) = %+v, want an empty registry", types.All())
	}
}

func TestNilEventTypesAnswersRatherThanPanics(t *testing.T) {
	var types *EventTypes
	if _, ok := types.Lookup("fr.oreo/anything"); ok {
		t.Fatal("Lookup() on a nil registry found a type")
	}
	if _, ok := types.ByID(1); ok {
		t.Fatal("ByID() on a nil registry found a type")
	}
	if types.Len() != 0 || types.All() != nil {
		t.Fatalf("nil registry reported %d types", types.Len())
	}
}

func TestPreflightRegistersEventTypesBeforeProvisioning(t *testing.T) {
	registry := NewRegistry(context.Background(), 0, nil, nil)
	if registry.EventTypes().Len() != 0 {
		t.Fatal("EventTypes() reported types before Preflight ran")
	}
	runtime := &fakeRuntime{name: "go"}
	if err := registry.RegisterRuntime(runtime); err != nil {
		t.Fatal(err)
	}
	if err := registry.Preflight(context.Background(), []Bundle{providing("fr.oreo.shop", purchase)}); err != nil {
		t.Fatal(err)
	}
	provided, ok := registry.EventTypes().Lookup(purchase.Type)
	if !ok || provided.TypeID != 1 {
		t.Fatalf("EventTypes().Lookup(%q) = %+v, %v after Preflight", purchase.Type, provided, ok)
	}
}

func subscribing(id string, events ...string) Bundle {
	bundle := providing(id)
	for _, event := range events {
		bundle.Manifest.Subscriptions = append(bundle.Manifest.Subscriptions,
			gcpkg.Subscription{Event: event, Priority: gcpkg.PriorityNormal})
	}
	return bundle
}

func TestValidateSubscriptionsAcceptsWhatSomethingEmits(t *testing.T) {
	provider := providing("fr.oreo.shop", purchase)
	consumer := subscribing("fr.oreo.discount", "block.break", "player.join", purchase.Type)
	bundles := []Bundle{provider, consumer}
	types, err := newEventTypes(bundles)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSubscriptions(bundles, types); err != nil {
		t.Fatalf("validateSubscriptions() rejected native and provided events: %v", err)
	}
}

func TestValidateSubscriptionsRefusesAMistypedNativeEvent(t *testing.T) {
	// The live example: WorldGuard-GO subscribes to block.place, which loads
	// cleanly today and never fires.
	bundles := []Bundle{subscribing("fr.oreo.guard", "block.place")}
	types, err := newEventTypes(bundles)
	if err != nil {
		t.Fatal(err)
	}
	err = validateSubscriptions(bundles, types)
	if err == nil {
		t.Fatal("validateSubscriptions() accepted a subscription to block.place")
	}
	for _, mentioned := range []string{"fr.oreo.guard", "block.place", "block.break", "player.join"} {
		if !strings.Contains(err.Error(), mentioned) {
			t.Fatalf("validateSubscriptions() error = %v, want it to name %s", err, mentioned)
		}
	}
}

func TestValidateSubscriptionsRefusesAnUnprovidedPluginEvent(t *testing.T) {
	bundles := []Bundle{subscribing("fr.oreo.discount", "fr.oreo.shop/purchase")}
	types, err := newEventTypes(bundles)
	if err != nil {
		t.Fatal(err)
	}
	err = validateSubscriptions(bundles, types)
	if err == nil {
		t.Fatal("validateSubscriptions() accepted a subscription nobody provides")
	}
	if !strings.Contains(err.Error(), "no installed plugin provides") {
		t.Fatalf("validateSubscriptions() error = %v, want the missing-provider wording", err)
	}
	// A namespaced name is not a typo for a native event, so listing the native
	// vocabulary here would be noise.
	if strings.Contains(err.Error(), "block.break") {
		t.Fatalf("validateSubscriptions() error = %v, want no native event listing", err)
	}
}

func TestValidateSubscriptionsResolvesRegardlessOfBundleOrder(t *testing.T) {
	// The consumer comes first, so this passes only because the registry is
	// built from every manifest before any of them loads.
	bundles := []Bundle{
		subscribing("fr.oreo.discount", purchase.Type),
		providing("fr.oreo.shop", purchase),
	}
	types, err := newEventTypes(bundles)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSubscriptions(bundles, types); err != nil {
		t.Fatalf("validateSubscriptions() made resolution depend on order: %v", err)
	}
}

func TestNativeEventsCannotBeExtendedByACaller(t *testing.T) {
	first := NativeEvents()
	if len(first) == 0 {
		t.Fatal("NativeEvents() is empty")
	}
	first[0] = "block.mutated"
	if second := NativeEvents(); second[0] == "block.mutated" {
		t.Fatal("NativeEvents() handed out the host's own slice")
	}
	if !IsNativeEvent(EventBlockBreak) || IsNativeEvent("fr.oreo.shop/purchase") {
		t.Fatal("IsNativeEvent() disagrees with NativeEvents()")
	}
	for _, native := range NativeEvents() {
		if !IsNativeEvent(native) {
			t.Fatalf("IsNativeEvent(%q) = false while NativeEvents() lists it", native)
		}
	}
}

func TestPreflightRefusesAnUnknownSubscriptionWithoutProvisioning(t *testing.T) {
	registry := NewRegistry(context.Background(), 0, nil, nil)
	runtime := &fakeRuntime{name: "go"}
	if err := registry.RegisterRuntime(runtime); err != nil {
		t.Fatal(err)
	}
	err := registry.Preflight(context.Background(), []Bundle{subscribing("fr.oreo.guard", "block.place")})
	if err == nil {
		t.Fatal("Preflight() accepted a subscription to an event nothing emits")
	}
	if runtime.provisions.Load() != 0 {
		t.Fatal("Preflight() provisioned a runtime despite refusing the manifests")
	}
}

func TestPreflightRefusesConflictingTypesWithoutProvisioning(t *testing.T) {
	registry := NewRegistry(context.Background(), 0, nil, nil)
	runtime := &fakeRuntime{name: "go"}
	if err := registry.RegisterRuntime(runtime); err != nil {
		t.Fatal(err)
	}
	bundles := []Bundle{
		providing("fr.oreo.shop", purchase),
		providing("fr.other.shop", purchase),
	}
	if err := registry.Preflight(context.Background(), bundles); err == nil {
		t.Fatal("Preflight() accepted two providers of one event")
	}
	// The point of ordering the checks: provisioning a JVM may download 45 MB,
	// and a manifest conflict is knowable before any of it.
	if runtime.provisions.Load() != 0 {
		t.Fatal("Preflight() provisioned a runtime despite refusing the manifests")
	}
}

func TestBindingsForCarryOnlyThePluginsOwnTypes(t *testing.T) {
	shop := providing("fr.oreo.shop", purchase, gcpkg.EventDefinition{Type: "fr.oreo.shop/refund"})
	// Subscribes to one of the shop's events and to a native one, and provides
	// nothing itself.
	discount := subscribing("fr.oreo.discount", "block.break", purchase.Type)
	other := providing("fr.other.bank", gcpkg.EventDefinition{Type: "fr.other/transfer"})
	types, err := newEventTypes([]Bundle{shop, discount, other})
	if err != nil {
		t.Fatal(err)
	}

	bindings := types.bindingsFor(discount.Manifest)
	if len(bindings) != 1 {
		t.Fatalf("bindingsFor() = %+v, want only the one event it subscribes to", bindings)
	}
	if bindings[0].Type != purchase.Type {
		t.Fatalf("bindingsFor() bound %q", bindings[0].Type)
	}
	if expected, _ := types.Lookup(purchase.Type); bindings[0].TypeID != expected.TypeID {
		t.Fatalf("bindingsFor() used id %d, the registry assigned %d",
			bindings[0].TypeID, expected.TypeID)
	}

	// The provider gets both of its own and neither of anyone else's.
	provider := types.bindingsFor(shop.Manifest)
	if len(provider) != 2 {
		t.Fatalf("bindingsFor(provider) = %+v, want its two events", provider)
	}
	for _, binding := range provider {
		if binding.Type == "fr.other/transfer" {
			t.Fatal("bindingsFor() leaked another plugin's event")
		}
	}
}

func TestBindingsForAPluginThatTouchesNoneIsEmpty(t *testing.T) {
	native := subscribing("fr.oreo.hello", "block.break", "player.join")
	types, err := newEventTypes([]Bundle{native})
	if err != nil {
		t.Fatal(err)
	}
	if bindings := types.bindingsFor(native.Manifest); bindings != nil {
		t.Fatalf("bindingsFor() = %+v, want nothing for a plugin using native events only", bindings)
	}
}

func TestBindingsForAreSortedByID(t *testing.T) {
	shop := providing("fr.oreo.shop",
		gcpkg.EventDefinition{Type: "fr.oreo/zulu"},
		gcpkg.EventDefinition{Type: "fr.oreo/alpha"},
	)
	types, err := newEventTypes([]Bundle{shop})
	if err != nil {
		t.Fatal(err)
	}
	bindings := types.bindingsFor(shop.Manifest)
	if len(bindings) != 2 || bindings[0].TypeID >= bindings[1].TypeID {
		t.Fatalf("bindingsFor() = %+v, want ascending ids", bindings)
	}
}

// The ids come from preflight, which saw every manifest. Assigning them at load
// from one bundle alone would give a provider different numbers from the
// subscribers holding them.
func TestLoadAllHandsEachRuntimeItsEventTable(t *testing.T) {
	var order []string
	var loaded []Bundle
	registry := NewRegistry(context.Background(), 0, nil, nil)
	if err := registry.RegisterRuntime(&recordingRuntime{order: &order, loaded: &loaded}); err != nil {
		t.Fatal(err)
	}
	shop := providing("fr.oreo.shop", purchase)
	shop.Manifest.Runtime = "recording"
	discount := subscribing("fr.oreo.discount", purchase.Type)
	discount.Manifest.Runtime = "recording"
	bundles := []Bundle{shop, discount}

	if err := registry.Preflight(context.Background(), bundles); err != nil {
		t.Fatal(err)
	}
	if err := registry.LoadAll(context.Background(), bundles); err != nil {
		t.Fatal(err)
	}

	if len(loaded) != 2 {
		t.Fatalf("the runtime was handed %d bundles", len(loaded))
	}
	for _, bundle := range loaded {
		if len(bundle.EventTypes) != 1 || bundle.EventTypes[0].Type != purchase.Type {
			t.Fatalf("plugin %s was loaded with %+v", bundle.Manifest.ID, bundle.EventTypes)
		}
		if bundle.EventTypes[0].TypeID == 0 {
			t.Fatalf("plugin %s was bound to the native type id", bundle.Manifest.ID)
		}
	}
	// Provider and subscriber must hold the same number for the same event,
	// which is the whole point of assigning them before either one loads.
	if loaded[0].EventTypes[0].TypeID != loaded[1].EventTypes[0].TypeID {
		t.Fatal("the provider and its subscriber were given different ids")
	}
}
