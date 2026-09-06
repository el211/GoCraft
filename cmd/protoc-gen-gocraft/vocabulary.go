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

	// JavaBlank is one value of this shape carrying nothing, as a Java
	// expression. It is what the generated warm() walks at load, so the first
	// real event of a type does not decode it cold while the tick waits.
	//
	// Shape and never content. It has to match what JavaDecode expects — a
	// PlayerRef is sixteen bytes and two strings — because a decoder handed the
	// wrong kind takes its fallback branch and leaves the real one interpreted,
	// which warms nothing.
	//
	// Fully qualified: the generated class imports only the vocabulary types its
	// accessors return, and adding Value to that list for the warm-up alone
	// would put an import in every event whether it needs one or not.
	JavaBlank string

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
		JavaBlank:  "new fr.gocraft.api.Value.List(java.util.List.of(new fr.gocraft.api.Value.Bytes(new byte[16]), new fr.gocraft.api.Value.Text(\"\"), new fr.gocraft.api.Value.Text(\"\")))",
	},
	"BlockPos": {
		SDKType:    "BlockPos",
		SDKDecode:  "positionFrom({v})",
		GoImport:   `"GoCraft/core/spatial"`,
		GoType:     "spatial.BlockPos",
		GoEncode:   "positionValue(%s)",
		JavaType:   "BlockPos",
		JavaDecode: "BlockPos.of(field(%d))",
		JavaBlank:  "new fr.gocraft.api.Value.List(java.util.List.of(new fr.gocraft.api.Value.Int(0), new fr.gocraft.api.Value.Int(0), new fr.gocraft.api.Value.Int(0)))",
	},
	"Block": {
		SDKType:    "Block",
		SDKDecode:  "blockFrom({v})",
		GoImport:   `coreworld "GoCraft/core/world"`,
		GoType:     "coreworld.Block",
		GoEncode:   "blockValue(%s)",
		JavaType:   "Block",
		JavaDecode: "Block.of(field(%d))",
		JavaBlank:  "new fr.gocraft.api.Value.List(java.util.List.of(new fr.gocraft.api.Value.Text(\"\"), new fr.gocraft.api.Value.List(java.util.List.of())))",
	},
	"string": {
		SDKType:    "string",
		SDKDecode:  "stringFrom({v}, {d})",
		GoType:     "string",
		GoEncode:   "abi.String(%s)",
		JavaType:   "String",
		JavaDecode: "text(%d)",
		JavaBlank:  "new fr.gocraft.api.Value.Text(\"\")",
	},
	"bool": {
		GoType:     "bool",
		GoEncode:   "abi.Bool(%s)",
		JavaType:   "boolean",
		JavaDecode: "flag(%d)",
		JavaBlank:  "new fr.gocraft.api.Value.Bool(false)",
	},
	"int64": {
		GoType:     "int64",
		GoEncode:   "abi.Int64(%s)",
		JavaType:   "long",
		JavaDecode: "number(%d)",
		JavaBlank:  "new fr.gocraft.api.Value.Int(0)",
	},
	"double": {
		GoType:     "float64",
		GoEncode:   "abi.Double(%s)",
		JavaType:   "double",
		JavaDecode: "decimal(%d)",
		JavaBlank:  "new fr.gocraft.api.Value.Decimal(0)",
	},
	"bytes": {
		GoType:     "[]byte",
		GoEncode:   "abi.Bytes(%s)",
		JavaType:   "byte[]",
		JavaDecode: "bytes(%d)",
		JavaBlank:  "new fr.gocraft.api.Value.Bytes(new byte[0])",
	},
}

// permissionKind is the injected map. It is never a parameter and never an
// accessor: the host resolves it before dispatch and a plugin queries it
// through can(), which is a map lookup rather than the round trip it would be
// if the answer had to be fetched while the tick waits.
const permissionKind = "map<string,bool>"

// permissionsBlank is that map's placeholder for the generated warm-up. One
// pair rather than none, so the loop that reads them runs a round instead of
// being skipped.
//
// It has no vocabulary row because the map has no row: it is never a parameter
// and never an accessor, so there is nothing else about it for a row to say.
const permissionsBlank = `new fr.gocraft.api.Value.List(java.util.List.of(` +
	`new fr.gocraft.api.Value.List(java.util.List.of(` +
	`new fr.gocraft.api.Value.Text(""), new fr.gocraft.api.Value.Bool(false)))))`

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
