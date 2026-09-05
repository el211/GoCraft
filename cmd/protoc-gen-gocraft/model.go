package main

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	wire "github.com/GoCraft-MC/gocraft-abi/abi/v1/wire"
)

// event is one native event, read out of the schema.
//
// Every runtime is generated from this, which is the whole point: a Lua event
// and a Java event come out of the same source in the same commit, so they
// cannot drift. Nothing here is inferred from a name — the semantics come from
// the gc.event option, because a field list cannot say whether an event can be
// cancelled or whether the tick waits for it.
type event struct {
	// Message is the schema name, e.g. BlockBreak. It drives the generated
	// identifiers on every side.
	Message string

	// Type is the name a manifest subscribes to, e.g. block.break.
	Type string

	Cancellable   bool
	Observational bool
	OnFailure     string
	Since         uint32

	// Fields in declaration order. The index is the position in the event
	// payload, which is the agreement the wire format relies on and never
	// carries.
	Fields []field
}

// field is one value in an event payload.
type field struct {
	// Name as declared, e.g. player.
	Name string

	// Index in the positional payload. Declaration order is load-bearing:
	// reordering fields is a wire break even though nothing moves in the file.
	Index int

	// Kind names the vocabulary type or scalar, e.g. PlayerRef or string.
	Kind string

	// Injected fields are filled in by the host before dispatch rather than
	// supplied by whoever emits the event, so they are not parameters.
	Injected bool
}

// GoName is the field name as a Go identifier.
func (f field) GoName() string {
	return exported(f.Name)
}

// JavaName is the accessor a plugin calls, e.g. pos().
func (f field) JavaName() string {
	parts := strings.Split(f.Name, "_")
	name := parts[0]
	for _, part := range parts[1:] {
		name += exported(part)
	}
	return name
}

// EmitName is the host-side emitter, e.g. EmitBlockBreak.
func (e event) EmitName() string {
	return "Emit" + e.Message
}

// ConstName is the exported type constant, e.g. EventBlockBreak.
func (e event) ConstName() string {
	return "Event" + e.Message
}

// JavaClass is the class a handler takes, e.g. BlockBreakEvent.
func (e event) JavaClass() string {
	return e.Message + "Event"
}

// SDKType is the struct a Go plugin's handler takes, e.g. BlockBreakEvent.
// The same name as the Java class on purpose: an author moving between the two
// runtimes should not have to learn the vocabulary twice.
func (e event) SDKType() string {
	return e.Message + "Event"
}

// SDKRegister is the typed registration, e.g. OnBlockBreak.
func (e event) SDKRegister() string {
	return "On" + e.Message
}

// SDKDecoder is the reader for one event's payload, e.g. blockBreakFrom.
func (e event) SDKDecoder() string {
	return lowerFirst(e.Message) + "From"
}

// Payload lists the fields an emitter supplies, which is every field the host
// does not inject.
func (e event) Payload() []field {
	var supplied []field
	for _, f := range e.Fields {
		if !f.Injected {
			supplied = append(supplied, f)
		}
	}
	return supplied
}

// Permissions reports whether the event carries an injected permission map,
// which is what makes can() answerable without a round trip.
func (e event) Permissions() (field, bool) {
	for _, f := range e.Fields {
		if f.Injected {
			return f, true
		}
	}
	return field{}, false
}

// collect reads every event out of the files being generated.
func collect(files []*protogen.File) ([]event, error) {
	var events []event
	for _, file := range files {
		if !file.Generate {
			continue
		}
		for _, message := range file.Messages {
			declared, err := read(message)
			if err != nil {
				return nil, err
			}
			if declared != nil {
				events = append(events, *declared)
			}
		}
	}
	return events, nil
}

func read(message *protogen.Message) (*event, error) {
	options, ok := message.Desc.Options().(*descriptorpb.MessageOptions)
	if !ok || options == nil {
		return nil, nil
	}
	if !proto.HasExtension(options, wire.E_Event) {
		// A message with no gc.event option is vocabulary, not an event.
		return nil, nil
	}
	declared, _ := proto.GetExtension(options, wire.E_Event).(*wire.EventOptions)
	if declared == nil || strings.TrimSpace(declared.GetType()) == "" {
		return nil, fmt.Errorf("%s: gc.event declares no type", message.Desc.FullName())
	}

	result := &event{
		Message:       string(message.Desc.Name()),
		Type:          declared.GetType(),
		Cancellable:   declared.GetCancellable(),
		Observational: declared.GetPhase() == wire.Phase_PHASE_OBSERVATIONAL,
		OnFailure:     failurePolicy(declared.GetOnFailure()),
		Since:         declared.GetSince(),
	}
	if result.Cancellable && result.Observational {
		return nil, fmt.Errorf("%s: an observational event cannot be cancellable; "+
			"the tick never waits for it, so nothing could act on the answer", result.Type)
	}
	for index, f := range message.Fields {
		kind, err := kindOf(f)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", result.Type, f.Desc.Name(), err)
		}
		result.Fields = append(result.Fields, field{
			Name:     string(f.Desc.Name()),
			Index:    index,
			Kind:     kind,
			Injected: injected(f),
		})
	}
	return result, nil
}

func injected(f *protogen.Field) bool {
	options, ok := f.Desc.Options().(*descriptorpb.FieldOptions)
	if !ok || options == nil {
		return false
	}
	value, _ := proto.GetExtension(options, wire.E_Injected).(bool)
	return value
}

// kindOf names the vocabulary type or scalar a field carries.
//
// The set is deliberately closed. A schema that reached for a type no runtime
// has a representation for would produce code that compiles on one side and
// not the other, and the failure would surface at generation rather than at
// review — which is where it belongs.
func kindOf(f *protogen.Field) (string, error) {
	if f.Desc.IsMap() {
		key := f.Desc.MapKey().Kind()
		value := f.Desc.MapValue().Kind()
		if key == protoreflect.StringKind && value == protoreflect.BoolKind {
			return "map<string,bool>", nil
		}
		return "", fmt.Errorf("unsupported map type")
	}
	if f.Desc.IsList() {
		return "", fmt.Errorf("repeated fields are not supported yet")
	}
	switch f.Desc.Kind() {
	case protoreflect.StringKind:
		return "string", nil
	case protoreflect.BoolKind:
		return "bool", nil
	case protoreflect.Int64Kind, protoreflect.Sint64Kind:
		return "int64", nil
	case protoreflect.DoubleKind:
		return "double", nil
	case protoreflect.BytesKind:
		return "bytes", nil
	case protoreflect.MessageKind:
		return string(f.Message.Desc.Name()), nil
	default:
		return "", fmt.Errorf("unsupported kind %s", f.Desc.Kind())
	}
}

func failurePolicy(policy wire.FailurePolicy) string {
	if policy == wire.FailurePolicy_FAILURE_POLICY_DENY {
		return "Deny"
	}
	return "Allow"
}

func exported(name string) string {
	if name == "" {
		return ""
	}
	return strings.ToUpper(name[:1]) + name[1:]
}
