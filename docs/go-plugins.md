# Native Go plugins

Native Go plugins are experimental. Each plugin runs as its own process and
communicates with GoCraft through the versioned, protocol-neutral Plugin API.
A panic or process crash therefore does not directly crash the server.

## Install a plugin

Put its `.gcpkg` file in `plugins/` and restart GoCraft. The server discovers
bundles before opening its network listeners. A failed plugin prevents startup
instead of leaving gameplay partly protected.

Configuration defaults stored under `config/` inside the bundle are copied on
first load to `plugins/<plugin-id>/`. Existing administrator files are never
overwritten. `Context.DataDirectory()` returns that directory.

## Lifecycle

Implement `gocraft.Plugin`. The import path does not end in the package name,
so name it:

```go
import gocraft "github.com/GoCraft-MC/gocraft-api-go"

type Plugin struct{}

func (*Plugin) OnLoad(ctx gocraft.Context) error { return nil }
func (*Plugin) OnEnable() error                  { return nil }
func (*Plugin) OnDisable() error                 { return nil }
```

`OnLoad` receives the logger, event registry, command registry, scheduler, and
data directory. Register callbacks there. `OnEnable` runs after loading and
`OnDisable` runs during orderly shutdown or failed enable cleanup.

Listeners and command callbacks run synchronously and serially for one plugin.
Do not block them. Scheduler callbacks run asynchronously. When a plugin is
disabled, GoCraft unregisters its listeners and commands and cancels its tasks.
Panics at every callback boundary are recovered and logged with a stack trace.

## Events

The first API version exposes:

| Event | Timing | Cancellable |
| --- | --- | --- |
| `PlayerJoinEvent` | after the player is reachable | no |
| `BlockBreakEvent` | before the block mutation | yes |

Java and Bedrock actions produce these same event types. No packet or numeric
protocol IDs are exposed. Cancelling `BlockBreakEvent` keeps the block intact
for either edition.

```go
ctx.Events().OnBlockBreak(func(event *gocraft.BlockBreakEvent) {
    if event.Block.ID == "minecraft:diamond_block" {
        event.Cancel()
    }
})
```

## Commands

Commands are declared in the bundle's generated `commands.pb`. Register a
callback against the path through that tree during `OnLoad`:

```go
ctx.Commands().Register("shop sell <price>", func(call *gocraft.CommandContext) error {
    call.Reply("Hello, " + call.SenderName)
    return nil
})
```

The path, not the executor ID the tree assigns. IDs are chosen by whatever built
the tree, so naming one in plugin source would write it down a second time —
free to disagree with the first the day a command is inserted above it. A path
that names nothing in the bundle is refused at load, listing the paths the
bundle does declare, rather than becoming a handler that silently never runs.
Literals appear as written and arguments in angle brackets, the same spelling a
Java plugin uses.

The host owns everything before the callback. It matches the line against the
tree, resolves each argument to the declared type, and checks the permissions
guarding the path; the plugin never sees the raw line. Typed values arrive
through `CommandContext.Args`, and `call.Can` answers from permissions the host
resolved before sending the invocation.

Replies are queued as effects and delivered on the next tick, the same path an
event handler's effects take, so they reach players on either edition and the
console alike.

Both editions are told about the command. They are told differently — Java
receives a Brigadier graph, Bedrock a flat signature per way of running it —
because the host renders one neutral tree twice rather than asking a plugin to
describe itself once per edition. Each player is sent only the branches their
permissions allow, and the list is resent when a plugin loads or unloads while
they are online.

## Build a bundle

See [gocraft-plugin-examples](https://github.com/GoCraft-MC/gocraft-plugin-examples) for a
complete plugin — one per runtime, and the only examples that exist. Build
its executable for the server operating system, place it at the manifest's
`entry`, then package it:

```sh
go install github.com/GoCraft-MC/gocraft-cli@latest
gocraft-cli build -o my-plugin.gcpkg ./my-plugin
```

The build tool is its own module: a plugin author compiles, they never run a
server, so installing one to package the other would be absurd. It reads the
bundle format from the same code the server does.

Native binaries are operating-system and architecture specific. Hot reload and
in-process unloading are not supported. Rebuild plugins after a Plugin API
version change; the host rejects incompatible manifests before executing code.
