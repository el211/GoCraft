package link

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	wire "github.com/GoCraft-MC/gocraft-abi/abi/v1/wire"
	"github.com/GoCraft-MC/gocraft-abi/ipc"
)

const (
	fakeRuntimeEnv = "GOCRAFT_FAKE_RUNTIME"
	fakeSocketEnv  = "GOCRAFT_FAKE_SOCKET"
)

// TestFakeRuntime is not a test. It is the child process the spawn tests start:
// `go test` re-executes its own binary with -test.run pointing here, which is
// how exec-based code is exercised without shipping a second program alongside
// the test suite.
//
// It exits rather than returning, so the test framework prints nothing from the
// child and the parent sees only the exit status it cares about.
func TestFakeRuntime(t *testing.T) {
	behaviour := os.Getenv(fakeRuntimeEnv)
	if behaviour == "" {
		t.Skip("not the child process")
	}
	runFakeRuntime(behaviour, os.Getenv(fakeSocketEnv))
}

func runFakeRuntime(behaviour, socket string) {
	if behaviour == "absent" {
		os.Exit(3) // never connects back
	}
	stream, err := net.Dial("unix", socket)
	if err != nil {
		os.Exit(4)
	}
	defer stream.Close()
	codec := ipc.NewCodec(stream)

	switch behaviour {
	case "silent":
		time.Sleep(30 * time.Second) // connects, then says nothing
		os.Exit(5)
	case "wrong-body":
		codec.Send(&wire.Envelope{Body: &wire.Envelope_Ping{Ping: &wire.Ping{}}})
		time.Sleep(30 * time.Second)
		os.Exit(6)
	}

	abi := uint32(1)
	if behaviour == "abi" {
		abi = 99
	}
	if behaviour == "quit" {
		// Greets, then leaves on its own a moment later. The loop below never
		// returns, so this cannot be a defer.
		go func() {
			time.Sleep(200 * time.Millisecond)
			os.Exit(7)
		}()
	}
	codec.Send(&wire.Envelope{Body: &wire.Envelope_Hello{
		Hello: &wire.Hello{Abi: abi, Runtime: "fake 1.0"},
	}})
	if behaviour == "unsolicited" {
		codec.Send(&wire.Envelope{Body: &wire.Envelope_Pong{Pong: &wire.Pong{}}})
	}

	for {
		envelope, err := codec.Receive()
		if err != nil {
			os.Exit(0)
		}
		if behaviour == "deaf" {
			continue // reads everything, answers nothing
		}
		switch envelope.GetBody().(type) {
		case *wire.Envelope_Shutdown:
			if behaviour == "stubborn" {
				continue // hears it, ignores it
			}
			os.Exit(0)
		case *wire.Envelope_Ping:
			if behaviour == "rude" {
				codec.Send(&wire.Envelope{Seq: envelope.GetSeq(), Body: &wire.Envelope_Fail{
					Fail: &wire.Fail{PluginId: "?", Reason: "not a pong"},
				}})
				continue
			}
			codec.Send(&wire.Envelope{Seq: envelope.GetSeq(), Body: &wire.Envelope_Pong{Pong: &wire.Pong{}}})
		case *wire.Envelope_Load:
			codec.Send(fakeLoadReply(behaviour, envelope))
			if behaviour == "emit" {
				// A plugin publishing an event, on the runtime's own even
				// numbering. After LOAD, because that is when it learns the ids.
				codec.Send(&wire.Envelope{Seq: 2, Body: &wire.Envelope_Emit{Emit: &wire.Emit{
					PluginId: "fr.oreo.shop", TypeId: 1,
					Fields: []*wire.Value{{Kind: &wire.Value_DoubleValue{DoubleValue: 1500}}},
				}}})
			}
		case *wire.Envelope_Dispatch:
			codec.Send(fakeDispatchReply(behaviour, envelope))
		case *wire.Envelope_Invoke:
			codec.Send(fakeInvokeReply(behaviour, envelope))
		case *wire.Envelope_Emitted:
			// Echo what the host answered back as a second emission, so the
			// test can assert the runtime was woken with the right reply on its
			// own sequence number.
			codec.Send(&wire.Envelope{Seq: 4, Body: &wire.Envelope_Emit{Emit: &wire.Emit{
				PluginId: "echo", TypeId: 1,
				Fields: []*wire.Value{
					{Kind: &wire.Value_BoolValue{BoolValue: envelope.GetEmitted().GetCancelled()}},
					{Kind: &wire.Value_StringValue{StringValue: envelope.GetEmitted().GetError()}},
					{Kind: &wire.Value_Int64Value{Int64Value: int64(len(envelope.GetEmitted().GetMutations()))}},
				},
			}}})
		}
	}
}

// fakeLoadReply covers the four answers a runtime may give to LOAD: the plugin
// came up, it came up registering something it never declared, it refused with
// a reason, or the runtime answered about a different plugin entirely.
func fakeLoadReply(behaviour string, envelope *wire.Envelope) *wire.Envelope {
	id := envelope.GetLoad().GetPluginId()
	switch behaviour {
	case "load-fails":
		return &wire.Envelope{Seq: envelope.GetSeq(), Body: &wire.Envelope_Fail{
			Fail: &wire.Fail{PluginId: id, Reason: "config.yml:12 taxRate < 0"},
		}}
	case "load-undeclared":
		return &wire.Envelope{Seq: envelope.GetSeq(), Body: &wire.Envelope_Loaded{
			Loaded: &wire.Loaded{PluginId: id, Events: []string{"block.break", "player.join"}},
		}}
	case "load-wrong-plugin":
		return &wire.Envelope{Seq: envelope.GetSeq(), Body: &wire.Envelope_Loaded{
			Loaded: &wire.Loaded{PluginId: "somebody.else"},
		}}
	case "load-echo":
		// Answers with what LOAD carried, so a test can prove the fields the
		// runtime binds against actually arrived rather than defaulting to "".
		load := envelope.GetLoad()
		return &wire.Envelope{Seq: envelope.GetSeq(), Body: &wire.Envelope_Fail{
			Fail: &wire.Fail{PluginId: id, Reason: strings.Join([]string{
				load.GetBundlePath(), load.GetEntry(),
				load.GetDataDirectory(), load.GetCommandTree(),
			}, "|")},
		}}
	case "load-nonsense":
		return &wire.Envelope{Seq: envelope.GetSeq(), Body: &wire.Envelope_Pong{Pong: &wire.Pong{}}}
	default:
		return &wire.Envelope{Seq: envelope.GetSeq(), Body: &wire.Envelope_Loaded{
			Loaded: &wire.Loaded{PluginId: id, Events: []string{"block.break"}},
		}}
	}
}

// fakeDispatchReply echoes the event back as a mutation so the test can prove
// the value survived the round trip in both directions, not merely that a
// verdict came back.
func fakeDispatchReply(behaviour string, envelope *wire.Envelope) *wire.Envelope {
	if behaviour == "dispatch-nonsense" {
		return &wire.Envelope{Seq: envelope.GetSeq(), Body: &wire.Envelope_Pong{Pong: &wire.Pong{}}}
	}
	event := envelope.GetDispatch().GetEvent()
	return &wire.Envelope{Seq: envelope.GetSeq(), Body: &wire.Envelope_Verdict{
		Verdict: &wire.Verdict{
			Cancelled: event.GetType() == "block.break",
			Mutations: []*wire.Mutation{{
				Path:  []uint32{0},
				Value: &wire.Value{Kind: &wire.Value_StringValue{StringValue: event.GetType()}},
			}},
			Effects: []*wire.HostCall{{
				Type:   "chat.send",
				Fields: []*wire.Value{{Kind: &wire.Value_Int64Value{Int64Value: int64(event.GetTypeId())}}},
			}},
		},
	}}
}

// fakeInvokeReply echoes back what INVOKE carried rather than answering a
// constant, so a test can prove the sender and the arguments survived the
// crossing instead of only that a reply came back.
func fakeInvokeReply(behaviour string, envelope *wire.Envelope) *wire.Envelope {
	switch behaviour {
	case "invoke-nonsense":
		return &wire.Envelope{Seq: envelope.GetSeq(), Body: &wire.Envelope_Pong{Pong: &wire.Pong{}}}
	case "command-fails":
		return &wire.Envelope{Seq: envelope.GetSeq(), Body: &wire.Envelope_Invoked{
			Invoked: &wire.Invoked{Error: "no region named spawn"},
		}}
	}
	invoke := envelope.GetInvoke()
	fields := []*wire.Value{
		{Kind: &wire.Value_StringValue{StringValue: invoke.GetPluginId()}},
		{Kind: &wire.Value_Int64Value{Int64Value: int64(invoke.GetExecutor())}},
		{Kind: &wire.Value_StringValue{StringValue: invoke.GetSender().GetName()}},
	}
	for _, argument := range invoke.GetArguments() {
		fields = append(fields,
			&wire.Value{Kind: &wire.Value_StringValue{StringValue: argument.GetName()}},
			&wire.Value{Kind: &wire.Value_Int64Value{Int64Value: int64(argument.GetType())}},
			argument.GetValue())
	}
	return &wire.Envelope{Seq: envelope.GetSeq(), Body: &wire.Envelope_Invoked{
		Invoked: &wire.Invoked{Effects: []*wire.HostCall{{Type: "chat.message", Fields: fields}}},
	}}
}

func fakeSpawn(behaviour string) Spawn {
	return func(socket string) *exec.Cmd {
		command := exec.Command(os.Args[0], "-test.run=^TestFakeRuntime$")
		command.Env = append(os.Environ(), fakeRuntimeEnv+"="+behaviour, fakeSocketEnv+"="+socket)
		return command
	}
}

func fakeConfig(t *testing.T, behaviour string) Config {
	t.Helper()
	return Config{
		Runtime:      "fake",
		Directory:    shortTempDir(t),
		ABI:          1,
		TickRate:     20,
		EventBudget:  2 * time.Millisecond,
		StartTimeout: 15 * time.Second,
		Spawn:        fakeSpawn(behaviour),
	}
}

func TestStartCompletesTheHandshakeAndCarriesRequests(t *testing.T) {
	child, err := Start(t.Context(), fakeConfig(t, "ok"))
	if err != nil {
		t.Fatal(err)
	}
	if child.Version != "fake 1.0" {
		t.Fatalf("Version = %q, want the runtime's own name", child.Version)
	}

	reply, err := child.Conn().Request(t.Context(), loadEnvelope("fr.oreo.hello"))
	if err != nil {
		t.Fatal(err)
	}
	if got := reply.GetLoaded().GetPluginId(); got != "fr.oreo.hello" {
		t.Fatalf("reply plugin = %q", got)
	}

	stopCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := child.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-child.Exited():
	case <-time.After(10 * time.Second):
		t.Fatal("the runtime never exited")
	}
}

// The socket exists only long enough for the runtime to connect back. Leaving
// it would let a second process attach to a runtime that is already serving.
func TestStartClosesTheSocketOnceConnected(t *testing.T) {
	child, err := Start(t.Context(), fakeConfig(t, "ok"))
	if err != nil {
		t.Fatal(err)
	}
	defer child.Stop(t.Context())

	if _, err := os.Stat(child.socket); !os.IsNotExist(err) {
		t.Fatalf("the socket file outlived the handshake: %v", err)
	}
}

func TestStartRefusesAnABIMismatch(t *testing.T) {
	_, err := Start(t.Context(), fakeConfig(t, "abi"))
	if err == nil {
		t.Fatal("Start() accepted a runtime speaking another ABI")
	}
	if !strings.Contains(err.Error(), "ABI 99") || !strings.Contains(err.Error(), "host speaks 1") {
		t.Fatalf("Start() error = %v, want both versions named", err)
	}
}

// A runtime that connects and then says nothing is as broken as one that never
// connects, and just as able to hold the boot open — the listeners wait on it.
func TestStartTimesOutOnASilentRuntime(t *testing.T) {
	config := fakeConfig(t, "silent")
	config.StartTimeout = 500 * time.Millisecond

	started := time.Now()
	_, err := Start(t.Context(), config)
	if err == nil {
		t.Fatal("Start() waited forever on a silent runtime")
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("Start() took %s to give up", elapsed)
	}
	if !strings.Contains(err.Error(), "handshake") {
		t.Fatalf("Start() error = %v", err)
	}
}

func TestStartFailsWhenTheRuntimeNeverConnects(t *testing.T) {
	config := fakeConfig(t, "absent")
	config.StartTimeout = 500 * time.Millisecond

	_, err := Start(t.Context(), config)
	if err == nil {
		t.Fatal("Start() succeeded without a runtime")
	}
	if !strings.Contains(err.Error(), "did not connect back") {
		t.Fatalf("Start() error = %v", err)
	}
}

func TestStartRejectsAHandshakeThatIsNotHello(t *testing.T) {
	config := fakeConfig(t, "wrong-body")
	config.StartTimeout = 2 * time.Second

	_, err := Start(t.Context(), config)
	if err == nil {
		t.Fatal("Start() accepted an opening message that was not HELLO")
	}
	if !strings.Contains(err.Error(), "instead of HELLO") {
		t.Fatalf("Start() error = %v", err)
	}
}

// SHUTDOWN is a request. A runtime stuck unloading a plugin will never act on
// it, and the server must still be able to stop.
func TestStopKillsARuntimeThatIgnoresShutdown(t *testing.T) {
	child, err := Start(t.Context(), fakeConfig(t, "stubborn"))
	if err != nil {
		t.Fatal(err)
	}

	stopCtx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()

	started := time.Now()
	if err := child.Stop(stopCtx); err != nil {
		t.Fatalf("Stop() = %v, want a kill to be treated as a clean stop", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("Stop() took %s", elapsed)
	}
	select {
	case <-child.Exited():
	case <-time.After(10 * time.Second):
		t.Fatal("the runtime survived Stop()")
	}
}

func TestStartRequiresASpawnFunction(t *testing.T) {
	config := fakeConfig(t, "ok")
	config.Spawn = nil
	if _, err := Start(t.Context(), config); err == nil {
		t.Fatal("Start() accepted a config with no spawn function")
	}
}
