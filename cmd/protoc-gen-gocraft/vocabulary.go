package main

import "fmt"

// The vocabulary, and how each side reads and writes it.
//
// §03 draws the line: roughly fifteen stable types are hand-written, because
// ergonomics matter there and generated code would be mediocre; the ~150 events
// built out of them are generated. This table is where the two meet — it says
// what a vocabulary type looks like in each language and which hand-written
// helper converts it, so the generator never has to know what a BlockPos is.
//
// Adding a vocabulary type means one row here and one hand-written type on each
// side. A schema using a type with no row fails generation by name rather than
// emitting code that compiles on one side and not the other.
type binding struct {
	// GoType is the parameter an emitter takes, e.g. *player.Player.
	GoType string

	// GoImport is what that type needs, written as it appears in an import
	// block. Empty for a builtin. Only the imports an event set actually uses
	// are emitted, so a schema that drops its last BlockPos does not leave an
	// unused import behind.
	GoImport string

	// GoEncode converts that parameter into an abi.Value. %s is the parameter.
	GoEncode string

	// JavaType is what the accessor returns, e.g. BlockPos.
	JavaType string

	// JavaDecode reads it out of the positional payload. %d is the index.
	JavaDecode string

	// SDKType is what the plugin-side Go struct holds, e.g. BlockPos.
	SDKType string

	// SDKDecode reads it out of the payload in a plugin binary. {v} is the
	// value expression and {d} a quoted description for the error message.
	//
	// Empty means no plugin-side Go representation was decided for this type.
	// A schema reaching for one fails generation by name rather than emitting a
	// SDK that compiles and a host that disagrees with it.
	SDKDecode string
}

var vocabulary = map[string]binding{
	"PlayerRef": {
		SDKType:    "*PlayerRef",
		SDKDecode:  "playerFrom({v}, sink)",
		GoImport:   `"GoCraft/core/player"`,
		GoType:     "*player.Player",
		GoEncode:   "playerReference(%s)",
		JavaType:   "PlayerRef",
		JavaDecode: "PlayerRef.of(field(%d), sink())",
	},
	"BlockPos": {
		SDKType:    "BlockPos",
		SDKDecode:  "positionFrom({v})",
		GoImport:   `"GoCraft/core/spatial"`,
		GoType:     "spatial.BlockPos",
		GoEncode:   "positionValue(%s)",
		JavaType:   "BlockPos",
		JavaDecode: "BlockPos.of(field(%d))",
	},
	"Block": {
		SDKType:    "Block",
		SDKDecode:  "blockFrom({v})",
		GoImport:   `coreworld "GoCraft/core/world"`,
		GoType:     "coreworld.Block",
		GoEncode:   "blockValue(%s)",
		JavaType:   "Block",
		JavaDecode: "Block.of(field(%d))",
	},
	"string": {
		SDKType:    "string",
		SDKDecode:  "stringFrom({v}, {d})",
		GoType:     "string",
		GoEncode:   "abi.String(%s)",
		JavaType:   "String",
		JavaDecode: "text(%d)",
	},
	"bool": {
		GoType:     "bool",
		GoEncode:   "abi.Bool(%s)",
		JavaType:   "boolean",
		JavaDecode: "flag(%d)",
	},
	"int64": {
		GoType:     "int64",
		GoEncode:   "abi.Int64(%s)",
		JavaType:   "long",
		JavaDecode: "number(%d)",
	},
	"double": {
		GoType:     "float64",
		GoEncode:   "abi.Double(%s)",
		JavaType:   "double",
		JavaDecode: "decimal(%d)",
	},
	"bytes": {
		GoType:     "[]byte",
		GoEncode:   "abi.Bytes(%s)",
		JavaType:   "byte[]",
		JavaDecode: "bytes(%d)",
	},
}

// permissionKind is the injected map. It is never a parameter and never an
// accessor: the host resolves it before dispatch and a plugin queries it
// through can(), which is a map lookup rather than the round trip it would be
// if the answer had to be fetched while the tick waits.
const permissionKind = "map<string,bool>"

func bindingFor(kind string) (binding, error) {
	if kind == permissionKind {
		return binding{}, fmt.Errorf("the permission map is injected and has no binding")
	}
	found, ok := vocabulary[kind]
	if !ok {
		return binding{}, fmt.Errorf("no vocabulary binding for %q; add one to "+
			"cmd/protoc-gen-gocraft/vocabulary.go and a hand-written type on each side", kind)
	}
	return found, nil
}

// Effects a handler may request. Each becomes a method on the generated event
// class that appends to the verdict, so a handler that sends three messages
// still costs one round trip.
type effect struct {
	JavaMethod string
	JavaParams string
	// HostCall is the type the host dispatches on when it drains the queue.
	HostCall string
	// Values are the abi values the effect carries, as Java expressions. %d is
	// the index of the event's PlayerRef when NeedsActor is set.
	Values string
	// NeedsActor marks an effect that has to say who it is for. Without it the
	// host drains a "send this message" with no recipient and can do nothing
	// with it — which is worse than the effect not existing, because the plugin
	// looks like it worked.
	NeedsActor bool
}

var effects = map[string]effect{
	"message": {
		JavaMethod: "sendMessage",
		JavaParams: "String message",
		HostCall:   "chat.message",
		// The whole PlayerRef, not just the uuid: the host already knows how to
		// read one, and passing the same vocabulary value the event carried
		// means the effect and the event cannot disagree about who acted.
		Values:     "field(%d), Values.text(message)",
		NeedsActor: true,
	},
}

func effectFor(name string) (effect, error) {
	found, ok := effects[name]
	if !ok {
		return effect{}, fmt.Errorf("no binding for effect %q; add one to "+
			"cmd/protoc-gen-gocraft/vocabulary.go", name)
	}
	return found, nil
}
