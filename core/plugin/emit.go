package plugin

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	abi "github.com/GoCraft-MC/gocraft-abi/abi/v1"
	"github.com/GoCraft-MC/gocraft-abi/gcpkg"
)

// EmitCustom dispatches one plugin-defined event and reports what the
// subscribers did to it.
//
// The host runs this rather than the emitting runtime for the same reason it
// runs native events: subscribers span runtimes, they run in priority order,
// and cancellation has to be arbitrated by something that is not one of them.
//
// Subscribers are strictly sequential, so there is nothing to merge. Subscriber
// two receives the state subscriber one left — the priority semantics nobody
// has ever found surprising in Bukkit — and the mutations come back in the
// order they were applied, so replaying them into the emitter's own object
// reproduces exactly the state this returns.
func (b *Bus) EmitCustom(definition gcpkg.EventDefinition, emission abi.Emission) abi.EmissionResult {
	subscribers := b.otherSubscribers(definition.Type, emission.PluginID)
	if len(subscribers) == 0 {
		return abi.EmissionResult{}
	}
	budget, _ := b.budgetFor(subscribers, definition.Type)
	ctx, cancel := context.WithTimeout(b.ctx, budget)
	defer cancel()

	state := emission.Fields
	var applied []abi.Mutation
	for index, sub := range subscribers {
		if sub.health.isDisabled() {
			continue
		}
		if ctx.Err() != nil {
			b.recordStarved(subscribers[index:], definition.Type)
			return b.starvedResult(definition, applied)
		}
		// One per subscriber, not one for the loop: the fields change between
		// them, and a subscriber that keeps the event it was handed would
		// otherwise read the state somebody after it left.
		event := &abi.Event{
			Type: definition.Type, TypeID: emission.TypeID, Fields: state,
			OnFailure: failurePolicy(definition),
		}
		started := time.Now()
		firstOfItsKind := sub.cold.cold(definition.Type)
		verdict, err := sub.instance.Dispatch(ctx, event)
		took := time.Since(started)
		sub.cold.ran(definition.Type)
		if budgetEnded(err) {
			sub.health.record(time.Now(), true, took)
			b.recordStarved(subscribers[index+1:], definition.Type)
			// How long it actually took, and what it was allowed. "Exceeded"
			// alone cannot tell a handler that is slightly too slow from one
			// that spent a hundred milliseconds on its first dispatch, and
			// those two are not the same problem.
			slog.Warn("plugin event deadline exceeded", "plugin", sub.id,
				"event", definition.Type, "took", took, "budget", budget)
			return b.starvedResult(definition, applied)
		}
		if err != nil {
			sub.health.record(time.Now(), true, took)
			slog.Warn("plugin event dispatch failed", "plugin", sub.id, "event", definition.Type, "err", err)
			if definition.FailClosed {
				return abi.EmissionResult{Cancelled: true, Mutations: applied}
			}
			continue
		}
		sub.health.record(time.Now(), false, took)
		b.reportColdStart(firstOfItsKind, sub, definition.Type, took)
		b.reportLateVerdict(ctx, sub, definition.Type, took)
		b.enqueueEffects(sub, definition.Type, verdict.Effects)
		state, applied = b.applyMutations(sub, definition, state, applied, verdict.Mutations)
		if verdict.Cancelled {
			if !definition.Cancellable {
				slog.Warn("plugin cancelled an event that is not cancellable",
					"plugin", sub.id, "event", definition.Type)
				continue
			}
			return abi.EmissionResult{Cancelled: true, Mutations: applied}
		}
	}
	return abi.EmissionResult{Mutations: applied}
}

// applyMutations writes one subscriber's changes into the running state.
//
// A refused write is logged against that subscriber and skipped, not fatal.
// The alternatives are worse: cancelling the event would let a plugin veto by
// writing where it may not, and applying it anyway would hand the next
// subscriber a shape its own codec cannot read.
func (b *Bus) applyMutations(sub *subscriber, definition gcpkg.EventDefinition,
	state []abi.Value, applied []abi.Mutation, mutations []abi.Mutation) ([]abi.Value, []abi.Mutation) {
	for _, mutation := range mutations {
		if !definition.MutablePath(mutation.Path) {
			slog.Warn("plugin wrote to a read-only event field",
				"plugin", sub.id, "event", definition.Type, "path", mutation.Path)
			continue
		}
		updated, err := abi.ApplyPath(state, mutation)
		if err != nil {
			slog.Warn("plugin event mutation refused",
				"plugin", sub.id, "event", definition.Type, "path", mutation.Path, "err", err)
			continue
		}
		state = updated
		applied = append(applied, mutation)
	}
	return state, applied
}

// starvedResult decides an event whose budget ran out before every subscriber
// had run, using the policy the provider declared.
//
// The mutations that did land are returned either way. They were applied to the
// state the subscribers after them would have seen, and the emitter's object has
// to end up where the dispatch actually got to — not where it started.
func (b *Bus) starvedResult(definition gcpkg.EventDefinition, applied []abi.Mutation) abi.EmissionResult {
	return abi.EmissionResult{Cancelled: definition.FailClosed, Mutations: applied}
}

// otherSubscribers lists who receives this event, skipping the plugin that
// published it.
//
// A plugin hearing its own event back would count an arrival twice, and §10
// makes the emitter's own object the thing the mutations are replayed into: it
// is the author of that state, not an observer of it.
func (b *Bus) otherSubscribers(event, emitter string) []*subscriber {
	all := b.subscribers(event)
	kept := all[:0]
	for _, sub := range all {
		if sub.id == emitter {
			continue
		}
		kept = append(kept, sub)
	}
	return kept
}

// failurePolicy is the provider's on_failure, as the wire spells it.
func failurePolicy(definition gcpkg.EventDefinition) abi.FailurePolicy {
	if definition.FailClosed {
		return abi.FailureDeny
	}
	return abi.FailureAllow
}

// EmitFrom dispatches an emission a runtime published, resolving the type id
// against the registry built at preflight.
//
// This is what a runtime's OnEmit is wired to. The id, not the name, because
// the name is what the wire stopped carrying once the table was negotiated.
func (r *Registry) EmitFrom(_ context.Context, emission abi.Emission) abi.EmissionResult {
	provided, known := r.EventTypes().ByID(emission.TypeID)
	if !known {
		return abi.EmissionResult{Error: fmt.Sprintf(
			"unknown event type id %d", emission.TypeID)}
	}
	if provided.Provider != emission.PluginID {
		// The id is real but belongs to someone else. Refused rather than
		// dispatched: an event carries the authority of whoever declared it,
		// and a plugin publishing another's event is impersonating it.
		return abi.EmissionResult{Error: fmt.Sprintf(
			"plugin %s cannot emit %s, which %s provides",
			emission.PluginID, provided.Definition.Type, provided.Provider)}
	}
	if fields, declared := len(emission.Fields), len(provided.Definition.Fields); fields != declared {
		return abi.EmissionResult{Error: fmt.Sprintf(
			"event %s carries %d fields, its manifest declares %d",
			provided.Definition.Type, fields, declared)}
	}
	return r.bus.EmitCustom(provided.Definition, emission)
}
