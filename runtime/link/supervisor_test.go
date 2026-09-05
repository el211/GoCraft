package link

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	abi "github.com/GoCraft-MC/gocraft-abi/abi/v1"
)

func fakeSupervisor(t *testing.T, behaviour string) *Supervisor {
	t.Helper()
	supervisor := NewSupervisor(fakeConfig(t, behaviour), fastLiveness())
	t.Cleanup(func() { supervisor.Stop(t.Context()) })
	return supervisor
}

func startedSupervisor(t *testing.T, behaviour string) *Supervisor {
	t.Helper()
	supervisor := fakeSupervisor(t, behaviour)
	if err := supervisor.Start(t.Context()); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	return supervisor
}

func helloRequest() LoadRequest {
	return LoadRequest{
		ID:         "fr.oreo.hello",
		BundlePath: "plugins/hello.gcpkg",
		Entry:      "fr.oreo.hello.HelloPlugin",
		Events:     []string{"block.break"},
	}
}

// The whole delegation surface in one pass: a runtime package that wires these
// five calls to plugin.Runtime has no transport logic of its own left to write.
func TestSupervisorLoadsDispatchesAndStops(t *testing.T) {
	supervisor := startedSupervisor(t, "ok")

	if supervisor.Name() != "fake" {
		t.Fatalf("Name() = %q", supervisor.Name())
	}
	if supervisor.Version() != "fake 1.0" {
		t.Fatalf("Version() = %q, want what the runtime called itself", supervisor.Version())
	}

	events, err := supervisor.Load(t.Context(), helloRequest())
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if len(events) != 1 || events[0] != "block.break" {
		t.Fatalf("Load() events = %v", events)
	}
	if err := supervisor.Ready(); err != nil {
		t.Fatalf("Ready() = %v", err)
	}

	verdict, err := supervisor.Dispatch(t.Context(), "fr.oreo.hello", &abi.Event{
		Type:      "block.break",
		TypeID:    7,
		Fields:    []abi.Value{abi.String("stone")},
		OnFailure: abi.FailureDeny,
	})
	if err != nil {
		t.Fatalf("Dispatch() = %v", err)
	}
	if !verdict.Cancelled {
		t.Fatal("Dispatch() verdict was not cancelled")
	}
	// Proves the conversion ran in both directions rather than merely that a
	// verdict came back: the runtime echoed the event type into a mutation.
	if len(verdict.Mutations) != 1 || verdict.Mutations[0].Value.String != "block.break" {
		t.Fatalf("Dispatch() mutations = %+v", verdict.Mutations)
	}
	if len(verdict.Effects) != 1 || verdict.Effects[0].Type != "chat.send" ||
		verdict.Effects[0].Fields[0].Int64 != 7 {
		t.Fatalf("Dispatch() effects = %+v", verdict.Effects)
	}

	if err := supervisor.Unload("fr.oreo.hello"); err != nil {
		t.Fatalf("Unload() = %v", err)
	}
	if err := supervisor.Stop(t.Context()); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
}

// The runtime knows why a plugin would not start and the host does not, so the
// reason reaches the admin unedited.
func TestSupervisorReportsALoadFailureVerbatim(t *testing.T) {
	supervisor := startedSupervisor(t, "load-fails")

	_, err := supervisor.Load(t.Context(), helloRequest())
	if err == nil {
		t.Fatal("Load() accepted a plugin the runtime refused")
	}
	if !strings.Contains(err.Error(), "config.yml:12 taxRate < 0") {
		t.Fatalf("Load() error = %v, want the runtime's own reason", err)
	}
	if !strings.Contains(err.Error(), "fr.oreo.hello") {
		t.Fatalf("Load() error = %v, want the plugin named", err)
	}
}

// The host dispatches from the manifest, so an undeclared subscription would
// never fire and its author would debug a handler that is simply never called.
func TestSupervisorRefusesAnUndeclaredSubscription(t *testing.T) {
	supervisor := startedSupervisor(t, "load-undeclared")

	_, err := supervisor.Load(t.Context(), helloRequest())
	if err == nil {
		t.Fatal("Load() accepted an event the manifest never declared")
	}
	if !strings.Contains(err.Error(), "player.join") {
		t.Fatalf("Load() error = %v, want the undeclared event named", err)
	}
	if strings.Contains(err.Error(), "block.break") {
		t.Fatalf("Load() error = %v, blamed an event that was declared", err)
	}
}

// A plugin may subscribe on its own config, which the static manifest cannot
// express, so registering fewer events than declared is legitimate.
func TestSupervisorAllowsFewerEventsThanDeclared(t *testing.T) {
	supervisor := startedSupervisor(t, "ok")

	request := helloRequest()
	request.Events = []string{"block.break", "player.join"}
	events, err := supervisor.Load(t.Context(), request)
	if err != nil {
		t.Fatalf("Load() = %v, want a partial registration accepted", err)
	}
	if len(events) != 1 {
		t.Fatalf("Load() events = %v", events)
	}
}

func TestSupervisorRefusesALoadAnsweredForAnotherPlugin(t *testing.T) {
	supervisor := startedSupervisor(t, "load-wrong-plugin")

	_, err := supervisor.Load(t.Context(), helloRequest())
	if err == nil {
		t.Fatal("Load() accepted an answer about a different plugin")
	}
	if !strings.Contains(err.Error(), "somebody.else") {
		t.Fatalf("Load() error = %v, want both ids named", err)
	}
}

func TestSupervisorRejectsRepliesThatAreNotTheOnesAsked(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		behaviour string
		call      func(*Supervisor) error
	}{
		{"load", "load-nonsense", func(s *Supervisor) error {
			_, err := s.Load(t.Context(), helloRequest())
			return err
		}},
		{"dispatch", "dispatch-nonsense", func(s *Supervisor) error {
			_, err := s.Dispatch(t.Context(), "fr.oreo.hello", &abi.Event{Type: "block.break"})
			return err
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			supervisor := startedSupervisor(t, testCase.behaviour)
			err := testCase.call(supervisor)
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("%s error = %v, want ErrProtocol", testCase.name, err)
			}
		})
	}
}

// Every call needs a running process. Reporting that plainly beats a nil
// dereference inside the transport.
func TestSupervisorRefusesWorkBeforeStart(t *testing.T) {
	supervisor := fakeSupervisor(t, "ok")

	for name, call := range map[string]func() error{
		"Load":  func() error { _, err := supervisor.Load(t.Context(), helloRequest()); return err },
		"Ready": supervisor.Ready,
		"Unload": func() error {
			return supervisor.Unload("fr.oreo.hello")
		},
		"Dispatch": func() error {
			_, err := supervisor.Dispatch(t.Context(), "fr.oreo.hello", &abi.Event{Type: "block.break"})
			return err
		},
	} {
		err := call()
		if err == nil {
			t.Fatalf("%s() succeeded with no runtime running", name)
		}
		if !strings.Contains(err.Error(), "not running") {
			t.Fatalf("%s() error = %v", name, err)
		}
	}
	if supervisor.Failed() != nil {
		t.Fatal("Failed() returned a channel before Start")
	}
	if supervisor.Version() != "" {
		t.Fatalf("Version() = %q before Start", supervisor.Version())
	}
}

// Stop on a supervisor that never started is how a rolled-back boot unwinds.
func TestSupervisorStopBeforeStartIsNotAnError(t *testing.T) {
	if err := fakeSupervisor(t, "ok").Stop(t.Context()); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
}

func TestSupervisorRefusesASecondStart(t *testing.T) {
	supervisor := startedSupervisor(t, "ok")
	if err := supervisor.Start(t.Context()); err == nil {
		t.Fatal("Start() spawned a second process for one supervisor")
	}
}

func TestSupervisorReportsARuntimeThatDiesOnItsOwn(t *testing.T) {
	supervisor := startedSupervisor(t, "quit")

	select {
	case <-supervisor.Failed():
	case <-time.After(10 * time.Second):
		t.Fatal("Failed() never closed for a runtime that exited")
	}
	if supervisor.Err() == nil {
		t.Fatal("Err() = nil after the runtime died")
	}
}

func TestSupervisorReportsAnUnresponsiveRuntime(t *testing.T) {
	supervisor := startedSupervisor(t, "deaf")

	select {
	case <-supervisor.Failed():
	case <-time.After(10 * time.Second):
		t.Fatal("Failed() never closed for a runtime that stopped answering")
	}
	if err := supervisor.Err(); !errors.Is(err, ErrUnresponsive) {
		t.Fatalf("Err() = %v, want ErrUnresponsive", err)
	}
}

func TestSupervisorStopsARuntimeThatBreaksTheProtocol(t *testing.T) {
	supervisor := startedSupervisor(t, "unsolicited")

	select {
	case <-supervisor.Failed():
	case <-time.After(10 * time.Second):
		t.Fatal("Failed() never closed for an unsolicited envelope")
	}
	if err := supervisor.Err(); !errors.Is(err, ErrProtocol) {
		t.Fatalf("Err() = %v, want ErrProtocol", err)
	}
}

// A supervisor that cannot tell a deliberate stop from a dead runtime would
// have its owner react to a shutdown it asked for — and, once respawn exists,
// restart a process it just took down.
func TestSupervisorStopIsNotReportedAsAFailure(t *testing.T) {
	supervisor := startedSupervisor(t, "ok")
	failed := supervisor.Failed()

	if err := supervisor.Stop(t.Context()); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
	select {
	case <-failed:
		t.Fatal("Failed() closed on a stop the caller asked for")
	case <-time.After(500 * time.Millisecond):
	}
	if err := supervisor.Err(); err != nil {
		t.Fatalf("Err() = %v after a clean stop", err)
	}
}

func regionInvocation() abi.CommandInvocation {
	return abi.CommandInvocation{
		Executor: 7,
		Sender: abi.CommandSender{
			Player: abi.List(abi.Bytes(make([]byte, 16)), abi.String("oreo"), abi.String("java")),
			Name:   "oreo",
			Permissions: []abi.Value{
				abi.List(abi.String("worldguard.region.define"), abi.Bool(true)),
			},
		},
		Arguments: []abi.CommandArgument{
			{Name: "name", Type: abi.CommandArgumentString, Value: abi.String("spawn")},
			{Name: "radius", Type: abi.CommandArgumentInteger, Value: abi.Int64(32)},
		},
	}
}

// The executor, the sender and every argument have to arrive intact: the host
// parsed the line once and the runtime is not going to parse it again, so
// anything dropped here is a value a handler silently reads as zero.
func TestSupervisorInvokesACommand(t *testing.T) {
	supervisor := startedSupervisor(t, "ok")

	result, err := supervisor.Invoke(t.Context(), "fr.oreo.hello", regionInvocation())
	if err != nil {
		t.Fatalf("Invoke() = %v", err)
	}
	if result.Error != "" {
		t.Fatalf("Invoke() result error = %q, want none", result.Error)
	}
	if len(result.Effects) != 1 || result.Effects[0].Type != "chat.message" {
		t.Fatalf("Invoke() effects = %+v", result.Effects)
	}
	fields := result.Effects[0].Fields
	want := []abi.Value{
		abi.String("fr.oreo.hello"),
		abi.Int64(7),
		abi.String("oreo"),
		abi.String("name"), abi.Int64(int64(abi.CommandArgumentString)), abi.String("spawn"),
		abi.String("radius"), abi.Int64(int64(abi.CommandArgumentInteger)), abi.Int64(32),
	}
	if len(fields) != len(want) {
		t.Fatalf("Invoke() echoed %d fields, want %d: %+v", len(fields), len(want), fields)
	}
	for index, value := range want {
		if !reflect.DeepEqual(fields[index], value) {
			t.Fatalf("Invoke() echoed field %d = %+v, want %+v", index, fields[index], value)
		}
	}
}

// A handler that fails is not a transport failure. Conflating the two would
// show the sender "connection reset" where the plugin said "no region named
// spawn", and would make the host treat a working runtime as a broken one.
func TestSupervisorReportsACommandThatFailed(t *testing.T) {
	supervisor := startedSupervisor(t, "command-fails")

	result, err := supervisor.Invoke(t.Context(), "fr.oreo.hello", regionInvocation())
	if err != nil {
		t.Fatalf("Invoke() = %v, want the failure in the result", err)
	}
	if result.Error != "no region named spawn" {
		t.Fatalf("Invoke() result error = %q", result.Error)
	}
}

func TestSupervisorRefusesAWrongAnswerToInvoke(t *testing.T) {
	supervisor := startedSupervisor(t, "invoke-nonsense")

	if _, err := supervisor.Invoke(t.Context(), "fr.oreo.hello", regionInvocation()); !errors.Is(err, ErrProtocol) {
		t.Fatalf("Invoke() = %v, want ErrProtocol", err)
	}
}

// An argument the host cannot name a type for never reaches the socket. It
// would arrive as UNSPECIFIED and the runtime would have to guess.
func TestSupervisorRefusesAnUntypedArgument(t *testing.T) {
	supervisor := startedSupervisor(t, "ok")

	invocation := regionInvocation()
	invocation.Arguments[1].Type = abi.CommandArgumentInvalid
	if _, err := supervisor.Invoke(t.Context(), "fr.oreo.hello", invocation); err == nil {
		t.Fatal("Invoke() accepted an argument with no type")
	}
}

// Every string LOAD carries has to arrive. The command tree path is the one
// that fails silently if it does not: the runtime binds its handlers against
// that file, so an empty path is a plugin whose commands load and never run.
func TestSupervisorSendsEveryLoadField(t *testing.T) {
	supervisor := startedSupervisor(t, "load-echo")

	_, err := supervisor.Load(t.Context(), LoadRequest{
		ID:            "fr.oreo.shop",
		BundlePath:    "plugins/shop.gcpkg",
		Entry:         "fr.oreo.shop.ShopPlugin",
		DataDirectory: "plugins/fr.oreo.shop",
		CommandTree:   "commands.pb",
	})
	if err == nil {
		t.Fatal("Load() succeeded; the fake answers FAIL with what it received")
	}
	want := "plugins/shop.gcpkg|fr.oreo.shop.ShopPlugin|plugins/fr.oreo.shop|commands.pb"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("Load() carried %v, want %q", err, want)
	}
}

// emitting starts a supervisor whose host answers every emission with answer,
// and reports each one it was handed.
func emitting(t *testing.T, answer abi.EmissionResult) (*Supervisor, chan abi.Emission) {
	t.Helper()
	emissions := make(chan abi.Emission, 4)
	config := fakeConfig(t, "emit")
	config.OnEmit = func(_ context.Context, emission abi.Emission) abi.EmissionResult {
		emissions <- emission
		if emission.PluginID == "echo" {
			return abi.EmissionResult{}
		}
		return answer
	}
	supervisor := NewSupervisor(config, fastLiveness())
	t.Cleanup(func() { supervisor.Stop(context.Background()) })
	if err := supervisor.Start(t.Context()); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	if _, err := supervisor.Load(t.Context(), helloRequest()); err != nil {
		t.Fatalf("Load() = %v", err)
	}
	return supervisor, emissions
}

func awaitEmission(t *testing.T, emissions chan abi.Emission) abi.Emission {
	t.Helper()
	select {
	case emission := <-emissions:
		return emission
	case <-time.After(10 * time.Second):
		t.Fatal("no emission reached the host")
		return abi.Emission{}
	}
}

// EMIT is the one envelope a runtime may send unasked. It must reach the host
// rather than be treated as the protocol violation everything else is.
func TestSupervisorDispatchesAnEmissionRatherThanKilling(t *testing.T) {
	supervisor, emissions := emitting(t, abi.EmissionResult{})

	emission := awaitEmission(t, emissions)
	if emission.PluginID != "fr.oreo.shop" || emission.TypeID != 1 {
		t.Fatalf("OnEmit received %+v", emission)
	}
	if len(emission.Fields) != 1 || emission.Fields[0].Double != 1500 {
		t.Fatalf("OnEmit received fields %+v", emission.Fields)
	}
	select {
	case <-supervisor.Failed():
		t.Fatalf("an emission was treated as a protocol violation: %v", supervisor.Err())
	case <-time.After(200 * time.Millisecond):
	}
}

// The reply travels back on the sequence number the runtime chose, which is the
// half of the split the host does not control.
func TestSupervisorAnswersOnTheRuntimesOwnSequence(t *testing.T) {
	_, emissions := emitting(t, abi.EmissionResult{
		Cancelled: true,
		Mutations: []abi.Mutation{{Path: []uint32{0}, Value: abi.Double(1200)}},
	})

	awaitEmission(t, emissions) // the publication
	echo := awaitEmission(t, emissions)
	if echo.PluginID != "echo" {
		t.Fatalf("the runtime was never woken with its answer, got %+v", echo)
	}
	if !echo.Fields[0].Bool {
		t.Fatal("the runtime did not see the cancellation")
	}
	if echo.Fields[1].String != "" {
		t.Fatalf("the runtime saw an error: %q", echo.Fields[1].String)
	}
	if echo.Fields[2].Int64 != 1 {
		t.Fatalf("the runtime received %d mutations, want 1", echo.Fields[2].Int64)
	}
}

// Dispatching reaches subscribers in this very runtime, and their replies come
// back on the socket the read loop is standing on. Handling EMIT there would
// deadlock, so this asks the host to talk to the runtime from inside OnEmit —
// which only completes if the emission is being handled somewhere else.
func TestSupervisorHandlesAnEmissionOffTheReadLoop(t *testing.T) {
	done := make(chan error, 1)
	config := fakeConfig(t, "emit")
	var supervisor *Supervisor
	config.OnEmit = func(ctx context.Context, emission abi.Emission) abi.EmissionResult {
		if emission.PluginID == "echo" {
			return abi.EmissionResult{}
		}
		roundTrip, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_, err := supervisor.Dispatch(roundTrip, "fr.oreo.hello", &abi.Event{Type: "block.break"})
		done <- err
		return abi.EmissionResult{}
	}
	supervisor = NewSupervisor(config, fastLiveness())
	t.Cleanup(func() { supervisor.Stop(context.Background()) })
	if err := supervisor.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Load(t.Context(), helloRequest()); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Dispatch() from inside OnEmit = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Dispatch() from inside OnEmit never returned: the read loop is blocked")
	}
}

// A host with nothing to dispatch into is a gap in this host, not a runtime
// speaking the wrong protocol. The plugin is told; the process lives.
func TestSupervisorReportsAnEmissionItCannotDispatch(t *testing.T) {
	config := fakeConfig(t, "emit")
	supervisor := NewSupervisor(config, fastLiveness())
	t.Cleanup(func() { supervisor.Stop(context.Background()) })
	if err := supervisor.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Load(t.Context(), helloRequest()); err != nil {
		t.Fatal(err)
	}

	select {
	case <-supervisor.Failed():
		t.Fatalf("an undispatchable emission killed the runtime: %v", supervisor.Err())
	case <-time.After(500 * time.Millisecond):
	}
}

// The host assigns the ids, so a table it cannot encode is a host bug and the
// plugin must not be loaded against a half-built one.
func TestSupervisorRefusesToLoadWithAnUnusableEventTable(t *testing.T) {
	supervisor := startedSupervisor(t, "ok")
	request := helloRequest()
	request.EventTypes = []abi.EventBinding{{TypeID: 0, Type: "fr.oreo.shop/purchase"}}

	if _, err := supervisor.Load(t.Context(), request); err == nil {
		t.Fatal("Load() accepted an event bound to the native type id")
	}
}
