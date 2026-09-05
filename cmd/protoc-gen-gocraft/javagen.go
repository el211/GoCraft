package main

import (
	"fmt"
	"sort"
	"strings"
	"text/template"

	"google.golang.org/protobuf/compiler/protogen"
)

// javaPackage is where the generated event classes land. It is the package a
// plugin imports, so it is part of the contract and not a detail.
const javaPackage = "fr.gocraft.api.event"

// generateJava writes one class per event.
//
// The class is the whole reason the ABI is a schema rather than a convention: a
// handler receives named, typed accessors, while the wire format underneath
// stays positional. §20.4 settled that — field names would have to match across
// every language and renaming one would break every runtime at once — so the
// agreement about what index 3 holds lives here, generated, rather than in a
// plugin author's head.
//
// The positional payload is never exposed. That is what lets the serialization
// change later without touching a plugin.
func generateJava(plugin *protogen.Plugin, events []event) error {
	for _, declared := range events {
		model, err := javaModel(declared)
		if err != nil {
			return err
		}
		path := strings.ReplaceAll(javaPackage, ".", "/") + "/" + declared.JavaClass() + ".java"
		file := plugin.NewGeneratedFile(path, "")
		if err := javaTemplate.Execute(file, model); err != nil {
			return fmt.Errorf("%s: %w", declared.JavaClass(), err)
		}
	}
	return generateJavaRegistry(plugin, events)
}

// generateJavaRegistry emits the table the runtime routes on.
//
// A dispatch arrives naming a type; the runtime has to turn that into the class
// a handler declared as its parameter. Generating the mapping means a new event
// costs a schema entry rather than an edit in the runtime, and that a handler
// for an unknown class fails at load rather than never being called.
func generateJavaRegistry(plugin *protogen.Plugin, events []event) error {
	path := strings.ReplaceAll(javaPackage, ".", "/") + "/GeneratedEvents.java"
	file := plugin.NewGeneratedFile(path, "")
	cancellables := make([]event, 0, len(events))
	for _, declared := range events {
		if declared.Cancellable {
			cancellables = append(cancellables, declared)
		}
	}
	return registryTemplate.Execute(file, map[string]any{
		"Package":      javaPackage,
		"Events":       events,
		"Cancellables": cancellables,
	})
}

type javaAccessor struct {
	Type      string
	Name      string
	Reader    string
	FieldName string
}

type javaEffect struct {
	Method string
	Params string
	Call   string
	Values string
}

type javaEvent struct {
	Package     string
	Imports     []string
	Class       string
	Type        string
	Cancellable bool
	Since       uint32
	Accessors   []javaAccessor
	Effects     []javaEffect
	Permissions bool
	// PermissionsIndex is where the injected map sits in the payload. Emitted
	// rather than searched for at runtime: the generator knows it, and a
	// runtime guessing which field looks like permissions would eventually
	// pick a block's property list instead.
	PermissionsIndex int
}

// actorIndex is where the event carries the player it is about, which is who an
// effect is delivered to.
func actorIndex(declared event) (int, bool) {
	for _, f := range declared.Fields {
		if f.Kind == "PlayerRef" {
			return f.Index, true
		}
	}
	return 0, false
}

func javaModel(declared event) (javaEvent, error) {
	model := javaEvent{
		Package:     javaPackage,
		Class:       declared.JavaClass(),
		Type:        declared.Type,
		Cancellable: declared.Cancellable,
		Since:       declared.Since,
	}
	for _, f := range declared.Fields {
		if f.Injected {
			model.Permissions = true
			model.PermissionsIndex = f.Index
			continue
		}
		bound, err := bindingFor(f.Kind)
		if err != nil {
			return javaEvent{}, fmt.Errorf("%s.%s: %w", declared.Type, f.Name, err)
		}
		model.Accessors = append(model.Accessors, javaAccessor{
			Type:      bound.JavaType,
			Name:      f.JavaName(),
			Reader:    fmt.Sprintf(bound.JavaDecode, f.Index),
			FieldName: f.Name,
		})
	}
	for _, name := range declared.Effects {
		bound, err := effectFor(name)
		if err != nil {
			return javaEvent{}, fmt.Errorf("%s: %w", declared.Type, err)
		}
		values := bound.Values
		if bound.NeedsActor {
			actor, found := actorIndex(declared)
			if !found {
				return javaEvent{}, fmt.Errorf(
					"%s declares the %q effect but carries no PlayerRef; there is nobody "+
						"for the host to deliver it to", declared.Type, name)
			}
			values = fmt.Sprintf(values, actor)
		}
		model.Effects = append(model.Effects, javaEffect{
			Method: bound.JavaMethod,
			Params: bound.JavaParams,
			Call:   bound.HostCall,
			Values: values,
		})
	}
	model.Imports = javaImports(model)
	return model, nil
}

// javaImports lists only what the class actually names. Java tolerates an
// unused import, but generated code that carries three of them reads as
// careless and invites someone to "tidy" a file marked DO NOT EDIT.
func javaImports(model javaEvent) []string {
	needed := make(map[string]struct{})
	for _, accessor := range model.Accessors {
		switch accessor.Type {
		case "PlayerRef", "BlockPos", "Block":
			needed["fr.gocraft.api."+accessor.Type] = struct{}{}
		}
	}
	if len(model.Effects) > 0 {
		needed["fr.gocraft.api.Values"] = struct{}{}
	}
	imports := make([]string, 0, len(needed))
	for name := range needed {
		imports = append(imports, name)
	}
	sort.Strings(imports)
	return imports
}

var javaTemplate = template.Must(template.New("event").Parse(`// Code generated by protoc-gen-gocraft. DO NOT EDIT.
//
// Source: abi/v1/events.proto
package {{ .Package }};

{{ range .Imports }}import {{ . }};
{{ end }}
/// The {@code {{ .Type }}} event.
///
{{- if .Cancellable }}
/// Cancellable, and dispatched while the tick waits. Every subscriber shares
/// one budget for the whole event, so a handler that takes its time is taking
/// it from the others.
{{- else }}
/// Observational. The tick does not wait, and cancelling is not offered because
/// what it describes has already happened.
{{- end }}
///
/// Introduced in ABI {{ .Since }}.
public final class {{ .Class }} extends fr.gocraft.api.Event {

    public static final String TYPE = "{{ .Type }}";

    public {{ .Class }}(java.util.List<fr.gocraft.api.Value> fields) {
        super(TYPE, fields, {{ if .Permissions }}fr.gocraft.api.Events.permissions(fields, {{ .PermissionsIndex }}){{ else }}java.util.Map.of(){{ end }});
    }
{{ range .Accessors }}
    /// The {@code {{ .FieldName }}} the event carries.
    public {{ .Type }} {{ .Name }}() {
        return {{ .Reader }};
    }
{{ end }}
{{- if .Permissions }}
    /// Whether the acting player holds a permission.
    ///
    /// Already resolved: the host answers every node the manifest subscribed to
    /// and ships the answers inside the event, so this is a map lookup rather
    /// than a round trip taken while the tick waits.
    ///
    /// A node the manifest never declared reads as false, because the host was
    /// never asked about it. That is a manifest bug, not a denial.
    public boolean can(String node) {
        return permission(node);
    }
{{ end }}
{{- range .Effects }}
    /// Requests a side effect, batched into this event's verdict rather than
    /// sent on its own — which is what keeps one event to one round trip
    /// however much a handler asks for.
    public void {{ .Method }}({{ .Params }}) {
        effect("{{ .Call }}", {{ .Values }});
    }
{{ end }}}
`))

var registryTemplate = template.Must(template.New("registry").Parse(`// Code generated by protoc-gen-gocraft. DO NOT EDIT.
//
// Source: abi/v1/events.proto
package {{ .Package }};

import fr.gocraft.api.Event;
import fr.gocraft.api.Value;

import java.util.List;
import java.util.Map;
import java.util.function.Function;

/// Every native event, by type and by class.
///
/// The runtime routes on this: a DISPATCH arrives naming a type, and a handler
/// declared a class as its parameter. Generating the table means a new event
/// costs a schema entry rather than an edit here, and that the two views cannot
/// disagree about which is which.
public final class GeneratedEvents {

    private GeneratedEvents() {
    }

    private static final Map<String, Function<List<Value>, Event>> BY_TYPE =
            Map.ofEntries(
{{- range $index, $event := .Events }}
{{- if $index }},{{ end }}
                    Map.entry({{ $event.JavaClass }}.TYPE, {{ $event.JavaClass }}::new)
{{- end }}
            );

    private static final Map<Class<? extends Event>, String> BY_CLASS = Map.ofEntries(
{{- range $index, $event := .Events }}
{{- if $index }},{{ end }}
            Map.entry({{ $event.JavaClass }}.class, {{ $event.JavaClass }}.TYPE)
{{- end }}
    );

    /// Builds the event a dispatch names, or null if this runtime was built
    /// against an ABI that does not have it.
    public static Event create(String type, List<Value> fields) {
        Function<List<Value>, Event> factory = BY_TYPE.get(type);
        return factory == null ? null : factory.apply(fields);
    }

    /// The type a handler subscribed to, from the class it declared.
    public static String typeOf(Class<?> parameter) {
        return BY_CLASS.get(parameter);
    }

    public static boolean knows(String type) {
        return BY_TYPE.containsKey(type);
    }

    /// Whether a subscriber may refuse this event.
    ///
    /// Generated with the rest, because it decides whether a handler is allowed
    /// to ask for an EventControl — and a list kept by hand would eventually
    /// refuse a cancel the schema allows, or accept one it does not.
    public static boolean cancellable(String type) {
        return CANCELLABLE.contains(type);
    }

    private static final java.util.Set<String> CANCELLABLE = java.util.Set.of(
{{- range $index, $event := .Cancellables }}
{{- if $index }},{{ end }}
            {{ $event.JavaClass }}.TYPE
{{- end }}
    );
}
`))
