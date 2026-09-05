package jvm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"GoCraft/core/dispatch"
	"GoCraft/core/player"
	"GoCraft/core/plugin"
	abi "github.com/GoCraft-MC/gocraft-abi/abi/v1"
	"github.com/GoCraft-MC/gocraft-abi/command"
	"github.com/GoCraft-MC/gocraft-abi/gcpkg"
)

func TestNameIsWhatAManifestWrites(t *testing.T) {
	if New(Config{}).Name() != "jvm" {
		t.Fatalf("Name() = %q", New(Config{}).Name())
	}
}

// Provision resolves the JDK and Start spawns it. Reversing them would spawn
// nothing, and saying so beats an exec error naming an empty path.
func TestStartRefusesBeforeProvision(t *testing.T) {
	err := New(Config{}).Start(t.Context(), nil)
	if err == nil {
		t.Fatal("Start() ran without a provisioned JDK")
	}
	if !strings.Contains(err.Error(), "Provision") {
		t.Fatalf("Start() error = %v, want the missing step named", err)
	}
}

func TestWorkBeforeStartIsRefused(t *testing.T) {
	runtime := New(Config{})
	for name, call := range map[string]func() error{
		"Load": func() error {
			_, err := runtime.Load(t.Context(), plugin.Bundle{Bundle: gcpkg.Bundle{Manifest: gcpkg.Manifest{ID: "dev.example.shop"}}})
			return err
		},
		"Ready": func() error { return runtime.Ready(t.Context()) },
	} {
		err := call()
		if err == nil {
			t.Fatalf("%s() succeeded with no JVM running", name)
		}
		if !strings.Contains(err.Error(), "not running") {
			t.Fatalf("%s() error = %v", name, err)
		}
	}
	if runtime.Failed() != nil {
		t.Fatal("Failed() returned a channel before Start")
	}
}

// Stop on a runtime that never started is how a rolled-back boot unwinds.
func TestStopBeforeStartIsNotAnError(t *testing.T) {
	if err := New(Config{}).Stop(t.Context()); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
}

// A command tree is no longer a reason to refuse a bundle: INVOKE carries an
// executor to the JVM and Invoked brings its answer back. What is left is the
// ordinary failure — the runtime is not running — and it has to name that
// rather than the commands, or an admin chases a gap that closed.
func TestLoadNoLongerRefusesABundleWithCommands(t *testing.T) {
	_, err := New(Config{}).Load(t.Context(), plugin.Bundle{Bundle: gcpkg.Bundle{Manifest: gcpkg.Manifest{ID: "dev.example.shop"}, Commands: &command.Root{}}})
	if err == nil {
		t.Fatal("Load() succeeded with no runtime running")
	}
	if strings.Contains(err.Error(), "commands") {
		t.Fatalf("Load() error = %v, want the stopped runtime as the reason", err)
	}
}

func TestSpawnBuildsTheDocumentedCommandLine(t *testing.T) {
	runtime := New(Config{})
	spawned := runtime.spawn(filepath.FromSlash("/opt/jdk25/bin/java"), filepath.FromSlash("/cache/rt.jar"))(
		filepath.FromSlash("/tmp/gc-jvm-1.sock"))

	if spawned.Path != filepath.FromSlash("/opt/jdk25/bin/java") {
		t.Fatalf("spawn() path = %q", spawned.Path)
	}
	want := []string{
		filepath.FromSlash("/opt/jdk25/bin/java"),
		// Not decoration: without it the JVM prints four sun.misc.Unsafe
		// deprecation warnings, from protobuf, on every boot.
		"--sun-misc-unsafe-memory-access=allow",
		"-jar", filepath.FromSlash("/cache/rt.jar"),
		"--sock", filepath.FromSlash("/tmp/gc-jvm-1.sock"),
		"--abi", "1",
	}
	if len(spawned.Args) != len(want) {
		t.Fatalf("spawn() args = %v, want %v", spawned.Args, want)
	}
	for index, argument := range want {
		if spawned.Args[index] != argument {
			t.Fatalf("spawn() args[%d] = %q, want %q", index, spawned.Args[index], argument)
		}
	}
	// Not the server's own streams: inheriting those would reach the console
	// and never latest.log, because slog tees to the file while a child writes
	// to the descriptor underneath it.
	if _, ok := spawned.Stdout.(*logWriter); !ok {
		t.Fatalf("spawn() stdout = %T, want it routed through the server log", spawned.Stdout)
	}
	if _, ok := spawned.Stderr.(*logWriter); !ok {
		t.Fatalf("spawn() stderr = %T, want it routed through the server log", spawned.Stderr)
	}
}

func TestSpawnHonoursConfiguredOutput(t *testing.T) {
	var out, errs strings.Builder
	spawned := New(Config{Stdout: &out, Stderr: &errs}).spawn("java", "rt.jar")("s.sock")

	if spawned.Stdout != &out || spawned.Stderr != &errs {
		t.Fatal("spawn() ignored the configured writers")
	}
}

func TestSocketDirectoryFallsBackToTemp(t *testing.T) {
	if got := New(Config{}).socketDirectory(); got != os.TempDir() {
		t.Fatalf("socketDirectory() = %q, want the temporary directory", got)
	}
	if got := New(Config{SocketDirectory: "sock"}).socketDirectory(); got != "sock" {
		t.Fatalf("socketDirectory() = %q, want the configured one", got)
	}
}

// The assertions that matter most in this package: the host drives Java through
// these interfaces and nothing else, so a signature drifting out of line has to
// fail here rather than at the call site in core/plugin.
var (
	_ plugin.Runtime         = (*Runtime)(nil)
	_ plugin.ReadyRuntime    = (*Runtime)(nil)
	_ plugin.Instance        = (*Instance)(nil)
	_ plugin.CommandInstance = (*Instance)(nil)
)

// core/plugin refuses to load a bundle whose instance cannot answer a command,
// so this claim has to hold for a Java plugin with commands to load at all. It
// held the opposite way round until the envelope gained INVOKE.
func TestInstanceClaimsCommandSupport(t *testing.T) {
	var instance plugin.Instance = &Instance{}
	if _, ok := instance.(plugin.CommandInstance); !ok {
		t.Fatal("Instance does not implement CommandInstance")
	}
}

func TestInstanceReportsItsManifest(t *testing.T) {
	manifest := gcpkg.Manifest{ID: "dev.example.shop", Version: "1.2.0", Runtime: "jvm"}
	instance := &Instance{manifest: manifest}
	if instance.Manifest().ID != manifest.ID {
		t.Fatalf("Manifest() = %+v", instance.Manifest())
	}
}

// The whole command path in one pass: the host converts, the transport carries,
// the JVM answers, and what comes back is a result the registry can queue.
func TestInvokeCommandReachesTheJVM(t *testing.T) {
	runtime := fakeRuntime(t, "ok", filepath.Join(t.TempDir(), "lives"), nil)
	if err := runtime.Start(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	loaded, err := runtime.Load(t.Context(), plugin.Bundle{Bundle: gcpkg.Bundle{Path: "plugins/shop.gcpkg", Manifest: gcpkg.Manifest{
		ID: "dev.example.shop", Entry: "dev.example.shop.Shop",
		Permissions: []string{"shop.admin"},
	}, Commands: &command.Root{Children: []command.Node{command.Literal{Name: "shop", Exec: 4}}}}})
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	commands, ok := loaded.(plugin.CommandInstance)
	if !ok {
		t.Fatal("Load() returned an instance that cannot answer a command")
	}

	sender := &stubSender{
		name:   "oreo",
		held:   map[string]bool{"shop.admin": true},
		player: player.New([16]byte{9}, "oreo", player.ClientEditionJava),
	}
	result, err := commands.InvokeCommand(t.Context(), 4, sender, dispatch.Values{
		"item": {Type: command.ArgString, String: "bread"},
	})
	if err != nil {
		t.Fatalf("InvokeCommand() = %v", err)
	}
	if result.Error != "" {
		t.Fatalf("InvokeCommand() result error = %q", result.Error)
	}
	// Not applied here: the effect is the runtime's answer, and queueing it is
	// core/plugin's job. What this proves is that it came back at all.
	if len(result.Effects) != 1 || result.Effects[0].Type != "chat.message" {
		t.Fatalf("InvokeCommand() effects = %+v", result.Effects)
	}
	fields := result.Effects[0].Fields
	if len(fields) != 2 || fields[1].String != "ran 4" {
		t.Fatalf("InvokeCommand() effect fields = %+v", fields)
	}
	if _, ok := plugin.PlayerUUIDFrom(fields[0]); !ok {
		t.Fatalf("InvokeCommand() lost the sender: %+v", fields[0])
	}
}

// A command typed while the JVM is down is refused, not queued. The sender is
// waiting on the answer, so an error they can read beats a silence.
func TestInvokeCommandWithoutARuntimeFails(t *testing.T) {
	instance := &Instance{runtime: New(Config{}), manifest: gcpkg.Manifest{ID: "dev.example.shop"}}
	if _, err := instance.InvokeCommand(t.Context(), 1, nil, nil); err == nil {
		t.Fatal("InvokeCommand() succeeded with no runtime running")
	}
}

type stubSender struct {
	name   string
	held   map[string]bool
	player *player.Player
}

func (s *stubSender) Name() string                   { return s.name }
func (s *stubSender) UUID() [16]byte                 { return [16]byte{} }
func (s *stubSender) SendMessage(string) error       { return nil }
func (s *stubSender) Has(permission string) bool     { return s.held[permission] }
func (s *stubSender) Player() (*player.Player, bool) { return s.player, s.player != nil }

// The respawn path builds its own supervisor, so a transport field added to
// only one of the two call sites would give a replacement JVM a quietly
// different connection. One constructor is what keeps them the same; this
// checks the field that would fail silently — a plugin that came back able to
// subscribe but no longer able to emit.
func TestLinkConfigCarriesTheEmissionHook(t *testing.T) {
	called := false
	runtime := New(Config{OnEmit: func(context.Context, abi.Emission) abi.EmissionResult {
		called = true
		return abi.EmissionResult{}
	}})

	config := runtime.linkConfig("java", "runtime.jar")
	if config.OnEmit == nil {
		t.Fatal("linkConfig() dropped OnEmit, so a plugin could not publish an event")
	}
	config.OnEmit(context.Background(), abi.Emission{})
	if !called {
		t.Fatal("linkConfig() carried some other function")
	}
	if config.Runtime != RuntimeName || config.Spawn == nil {
		t.Fatalf("linkConfig() = %+v", config)
	}
}
