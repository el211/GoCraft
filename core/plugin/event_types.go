package plugin

import (
	"fmt"
	"sort"
	"strings"

	abi "github.com/GoCraft-MC/gocraft-abi/abi/v1"
	"github.com/GoCraft-MC/gocraft-abi/gcpkg"
)

// ProvidedEvent is one plugin-defined event type this server knows about, and
// which plugin declared it.
type ProvidedEvent struct {
	// TypeID is what the wire carries instead of the type string, so a hot
	// custom event costs one varint rather than a name on every dispatch.
	// Numbering starts at 1: abi/v1 reserves 0 for native events.
	TypeID     uint32
	Provider   string
	Definition gcpkg.EventDefinition
}

// EventTypes is every event type the scanned bundles declare between them.
//
// It is built once, from all the manifests at once, before any plugin loads —
// which is what lets a plugin subscribe to an event whose provider loads after
// it. Load order is alphabetical today and topological eventually; neither
// should decide whether a subscription resolves.
type EventTypes struct {
	byName map[string]ProvidedEvent
	byID   map[uint32]ProvidedEvent
	names  []string
}

// newEventTypes collects the [[events.provides]] of every bundle.
//
// Two plugins providing the same type is refused rather than resolved: the
// subscribers of that type would be split between two definitions of it, and
// whichever one a subscriber got would depend on load order. The message names
// both plugins, because the admin has to remove one of them and nothing else
// will tell them which two are fighting.
func newEventTypes(bundles []Bundle) (*EventTypes, error) {
	providers := make(map[string]string)
	definitions := make(map[string]gcpkg.EventDefinition)
	for _, bundle := range bundles {
		for _, definition := range bundle.Manifest.Provides {
			if owner, taken := providers[definition.Type]; taken {
				return nil, fmt.Errorf("plugin: event %s is provided by both %s and %s",
					definition.Type, owner, bundle.Manifest.ID)
			}
			providers[definition.Type] = bundle.Manifest.ID
			definitions[definition.Type] = definition
		}
	}
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	// Sorted so an id depends on the set of installed plugins and not on the
	// order a directory happened to be read in. The protocol does not require
	// it — the host re-sends the table on every load — but a log line naming
	// type 3 should mean the same event on the next boot.
	sort.Strings(names)
	types := &EventTypes{
		byName: make(map[string]ProvidedEvent, len(names)),
		byID:   make(map[uint32]ProvidedEvent, len(names)),
		names:  names,
	}
	for index, name := range names {
		provided := ProvidedEvent{
			TypeID:     uint32(index) + 1,
			Provider:   providers[name],
			Definition: definitions[name],
		}
		types.byName[name] = provided
		types.byID[provided.TypeID] = provided
	}
	return types, nil
}

// validateSubscriptions refuses a subscription to an event nothing will emit.
//
// Until now a manifest could name any string: the bus simply never had a
// matching key, so the handler was wired, loaded cleanly and never ran. A typo
// and a working plugin looked exactly alike from the outside, and the only
// symptom was silence.
//
// The two mistakes get two messages because they are two mistakes. A name with
// no slash can only be a native event, and there are few enough to list — that
// is the "block.place instead of block.break" case, and the list is the fix. A
// namespaced name is plugin-defined, so the fix is installing the plugin that
// provides it, and listing every native event would be noise.
func validateSubscriptions(bundles []Bundle, types *EventTypes) error {
	for _, bundle := range bundles {
		for _, subscription := range bundle.Manifest.Subscriptions {
			if IsNativeEvent(subscription.Event) {
				continue
			}
			if _, provided := types.Lookup(subscription.Event); provided {
				continue
			}
			if !strings.Contains(subscription.Event, "/") {
				return fmt.Errorf("plugin %s: subscribes to unknown event %q, native events are %s",
					bundle.Manifest.ID, subscription.Event, strings.Join(NativeEvents(), ", "))
			}
			return fmt.Errorf("plugin %s: subscribes to %s, which no installed plugin provides",
				bundle.Manifest.ID, subscription.Event)
		}
	}
	return nil
}

// bindingsFor is the id table one plugin is loaded with: the events it can
// emit, plus the ones it subscribes to.
//
// Only its own. A runtime has no use for the id of an event none of its plugins
// can see, and sending the whole table would tell every plugin author what
// every other plugin on the server is called.
//
// Sorted by id so a load is reproducible and a test can read it.
func (t *EventTypes) bindingsFor(manifest gcpkg.Manifest) []abi.EventBinding {
	if t == nil {
		return nil
	}
	wanted := make(map[string]struct{}, len(manifest.Provides)+len(manifest.Subscriptions))
	for _, definition := range manifest.Provides {
		wanted[definition.Type] = struct{}{}
	}
	for _, subscription := range manifest.Subscriptions {
		// A native event has no id, so it is not in the table and looking it up
		// simply misses. No need to test the name for a slash here.
		wanted[subscription.Event] = struct{}{}
	}
	bindings := make([]abi.EventBinding, 0, len(wanted))
	for _, name := range t.names {
		if _, touched := wanted[name]; !touched {
			continue
		}
		provided := t.byName[name]
		bindings = append(bindings, abi.EventBinding{TypeID: provided.TypeID, Type: name})
	}
	if len(bindings) == 0 {
		return nil
	}
	return bindings
}

// Lookup finds a plugin-defined event by the name a manifest spells.
func (t *EventTypes) Lookup(eventType string) (ProvidedEvent, bool) {
	if t == nil {
		return ProvidedEvent{}, false
	}
	provided, ok := t.byName[eventType]
	return provided, ok
}

// ByID finds a plugin-defined event by the id the wire carries.
func (t *EventTypes) ByID(typeID uint32) (ProvidedEvent, bool) {
	if t == nil {
		return ProvidedEvent{}, false
	}
	provided, ok := t.byID[typeID]
	return provided, ok
}

// All lists every known type in id order, which is name order.
func (t *EventTypes) All() []ProvidedEvent {
	if t == nil {
		return nil
	}
	all := make([]ProvidedEvent, 0, len(t.names))
	for _, name := range t.names {
		all = append(all, t.byName[name])
	}
	return all
}

// Len reports how many plugin-defined types are registered.
func (t *EventTypes) Len() int {
	if t == nil {
		return 0
	}
	return len(t.names)
}

// EventTypes exposes the registered plugin-defined events.
//
// It is nil until Preflight has run, and a nil receiver answers every question
// with "no such type" rather than panicking: a host that dispatches before
// preflighting has a sequencing bug, and a crash inside a lookup would name the
// lookup rather than the sequence.
func (r *Registry) EventTypes() *EventTypes {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.eventTypes
}
