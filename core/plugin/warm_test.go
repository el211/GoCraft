package plugin

import (
	"context"
	"errors"
	"strings"
	"testing"

	abi "github.com/GoCraft-MC/gocraft-abi/abi/v1"
	"github.com/GoCraft-MC/gocraft-abi/gcpkg"
)

// warmingInstance records every warm-up it is asked for, in order.
type warmingInstance struct {
	manifest gcpkg.Manifest
	order    *[]string
	err      error
}

func (i *warmingInstance) Manifest() gcpkg.Manifest { return i.manifest }
func (i *warmingInstance) Dispatch(context.Context, *abi.Event) (abi.Verdict, error) {
	return abi.Verdict{}, nil
}
func (i *warmingInstance) Unload(context.Context) error { return nil }
func (i *warmingInstance) Warm(_ context.Context, event string) error {
	*i.order = append(*i.order, "warm:"+i.manifest.ID+":"+event)
	return i.err
}

type warmingRuntime struct {
	readyRuntime
	warmErr error
}

func (r *warmingRuntime) Load(_ context.Context, bundle Bundle) (Instance, error) {
	*r.order = append(*r.order, "load:"+bundle.Manifest.ID)
	return &warmingInstance{manifest: bundle.Manifest, order: r.order, err: r.warmErr}, nil
}

func subscribingBundle(id string, events ...string) Bundle {
	bundle := testBundle(id)
	for _, event := range events {
		bundle.Manifest.Subscriptions = append(bundle.Manifest.Subscriptions,
			gcpkg.Subscription{Event: event})
	}
	return bundle
}

func warmingRegistry(order *[]string, warmErr error) *Registry {
	registry := NewRegistry(context.Background(), 0, nil, nil)
	registry.RegisterRuntime(&warmingRuntime{
		readyRuntime: readyRuntime{recordingRuntime: recordingRuntime{order: order}},
		warmErr:      warmErr,
	})
	return registry
}

// Every subscription is warmed after the last plugin is loaded and before any
// runtime is told READY.
//
// After the last load, so a runtime hosting several plugins warms with all of
// them present. Before READY, because that is the final moment the host is
// still waiting on the runtimes without a budget — a warm-up that ran after it
// would be racing the first player through the door.
func TestLoadAllWarmsEverySubscriptionBeforeReady(t *testing.T) {
	var order []string
	registry := warmingRegistry(&order, nil)
	bundles := []Bundle{
		subscribingBundle("shop", "block.break"),
		subscribingBundle("bank", "player.join", "fr.oreo.shop/purchase"),
	}

	if err := registry.LoadAll(context.Background(), bundles); err != nil {
		t.Fatalf("LoadAll() = %v", err)
	}

	want := []string{
		"start", "load:bank", "load:shop",
		"warm:bank:player.join", "warm:bank:fr.oreo.shop/purchase",
		"warm:shop:block.break",
		"ready",
	}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

// A runtime that will not warm is a runtime whose first event is slow, which is
// what every runtime was before this existed. Refusing to start a server over
// it would trade a warning for an outage.
func TestAWarmUpThatFailsDoesNotFailTheBoot(t *testing.T) {
	var order []string
	registry := warmingRegistry(&order, errors.New("runtime said no"))

	if err := registry.LoadAll(context.Background(), []Bundle{
		subscribingBundle("shop", "block.break"),
	}); err != nil {
		t.Fatalf("LoadAll() = %v, want a boot that carried on", err)
	}
	if got := strings.Join(order, ","); !strings.HasSuffix(got, "ready") {
		t.Fatalf("order = %v, want the load phase to have ended anyway", order)
	}
}

// Warming is optional, and not warming is the right answer for a runtime with
// nothing to warm: a Go plugin is a compiled binary with no classes to load and
// no bytecode to compile.
func TestAnInstanceThatDoesNotWarmIsSkipped(t *testing.T) {
	var order []string
	registry := NewRegistry(context.Background(), 0, nil, nil)
	if err := registry.RegisterRuntime(&readyRuntime{
		recordingRuntime: recordingRuntime{order: &order},
	}); err != nil {
		t.Fatal(err)
	}

	if err := registry.LoadAll(context.Background(), []Bundle{
		subscribingBundle("shop", "block.break"),
	}); err != nil {
		t.Fatalf("LoadAll() = %v", err)
	}
	for _, step := range order {
		if strings.HasPrefix(step, "warm:") {
			t.Fatalf("an instance with no Warm was warmed anyway: %v", order)
		}
	}
}
