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

	// GoBlank is one value of this shape carrying nothing, as a Go expression.
	//
	// It is what the host sends in a warm dispatch, so the first real event of a
	// type meets no cold code on either side of the socket: the marshal here,
	// the protobuf parse over there, and the conversion back into a payload a
	// handler could read. Measured, that path costs about 2 ms once per process
	// and lands on whichever event carries values first — which is why the blank
	// starts here rather than being built by the runtime that receives it.
	//
	// Shape and never content: an empty name, a zero, a player who is nobody.
	// The shape has to be right, though — every decoder on the far side refuses
	// a kind it does not expect and falls back, so a blank of the wrong shape
	// warms the refusal and leaves the real branch cold.
	GoBlank string

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
		GoBlank:    `abi.List(abi.Bytes(make([]byte, 16)), abi.String(""), abi.String(""))`,
	},
	"BlockPos": {
		SDKType:    "BlockPos",
		SDKDecode:  "positionFrom({v})",
		GoImport:   `"GoCraft/core/spatial"`,
		GoType:     "spatial.BlockPos",
		GoEncode:   "positionValue(%s)",
		JavaType:   "BlockPos",
		JavaDecode: "BlockPos.of(field(%d))",
		GoBlank:    `abi.List(abi.Int64(0), abi.Int64(0), abi.Int64(0))`,
	},
	"Block": {
		SDKType:    "Block",
		SDKDecode:  "blockFrom({v})",
		GoImport:   `coreworld "GoCraft/core/world"`,
		GoType:     "coreworld.Block",
		GoEncode:   "blockValue(%s)",
		JavaType:   "Block",
		JavaDecode: "Block.of(field(%d))",
		GoBlank:    `abi.List(abi.String(""), abi.List())`,
	},
	"string": {
		SDKType:    "string",
		SDKDecode:  "stringFrom({v}, {d})",
		GoType:     "string",
		GoEncode:   "abi.String(%s)",
		JavaType:   "String",
		JavaDecode: "text(%d)",
		GoBlank:    `abi.String("")`,
	},
	"bool": {
		GoType:     "bool",
		GoEncode:   "abi.Bool(%s)",
		JavaType:   "boolean",
		JavaDecode: "flag(%d)",
		GoBlank:    `abi.Bool(false)`,
	},
	"int64": {
		GoType:     "int64",
		GoEncode:   "abi.Int64(%s)",
		JavaType:   "long",
		JavaDecode: "number(%d)",
		GoBlank:    `abi.Int64(0)`,
	},
	"double": {
		GoType:     "float64",
		GoEncode:   "abi.Double(%s)",
		JavaType:   "double",
		JavaDecode: "decimal(%d)",
		GoBlank:    `abi.Double(0)`,
	},
	"bytes": {
		GoType:     "[]byte",
		GoEncode:   "abi.Bytes(%s)",
		JavaType:   "byte[]",
		JavaDecode: "bytes(%d)",
		GoBlank:    `abi.Bytes(nil)`,
	},
}

// permissionKind is the injected map. It is never a parameter and never an
// accessor: the host resolves it before dispatch and a plugin queries it
// through can(), which is a map lookup rather than the round trip it would be
// if the answer had to be fetched while the tick waits.
const permissionKind = "map<string,bool>"

// permissionsBlank is that map's placeholder for a warm dispatch. One pair
// rather than none, so the loop that reads them runs a round instead of being
// skipped.
//
// It has no vocabulary row because the map has no row: it is never a parameter
// and never an accessor, so there is nothing else about it for a row to say.
const permissionsBlank = `abi.List(abi.List(abi.String(""), abi.Bool(false)))`

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
