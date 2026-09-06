package plugin

import (
	"context"
	"errors"
	"log/slog"
	"time"

	abi "github.com/GoCraft-MC/gocraft-abi/abi/v1"
)

// EmitCancellable blocks for subscriber verdicts under one shared event budget.
func (b *Bus) EmitCancellable(event *abi.Event) bool {
	if event == nil {
		return true
	}
	subscribers := b.subscribers(event.Type)
	if len(subscribers) == 0 {
		return true
	}
	ctx, cancel := context.WithTimeout(b.ctx, b.budget)
	defer cancel()
	for index, sub := range subscribers {
		if sub.health.isDisabled() {
			continue
		}
		if ctx.Err() != nil {
			b.recordStarved(subscribers[index:], event.Type)
			return failureAllows(event)
		}
		started := time.Now()
		verdict, err := sub.instance.Dispatch(ctx, event)
		took := time.Since(started)
		if budgetEnded(err) {
			sub.health.record(time.Now(), true, took)
			b.recordStarved(subscribers[index+1:], event.Type)
			slog.Warn("plugin event deadline exceeded", "plugin", sub.id,
				"event", event.Type, "took", took, "budget", b.budget)
			return failureAllows(event)
		}
		if err != nil {
			sub.health.record(time.Now(), true, took)
			slog.Warn("plugin event dispatch failed", "plugin", sub.id, "event", event.Type, "err", err)
			if !failureAllows(event) {
				return false
			}
			continue
		}
		sub.health.record(time.Now(), false, took)
		b.reportLateVerdict(ctx, sub, event.Type, took)
		b.enqueueEffects(sub, event.Type, verdict.Effects)
		if verdict.Cancelled {
			return false
		}
	}
	return true
}

// budgetEnded reports a dispatch that came back with nothing because the shared
// budget ran out, or because the host itself is going away.
//
// It is asked of the error and never of the context, and that distinction is
// the whole point. A dispatch that answered inside the budget answered, even if
// the deadline fired between the reply arriving and this line running — the
// verdict is in hand, and throwing it away would charge a subscriber for a race
// it won. §06 puts it as "only a subscriber during whose own call the deadline
// fires is charged", and a context consulted after the call cannot tell those
// two apart: it says the same thing either way.
//
// Cancellation counts as well as expiry. A parent context that ended took the
// verdict away for a reason that is not the plugin's, which is the same reason
// starvation is counted apart from failure.
func budgetEnded(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// reportLateVerdict names a subscriber that answered as its budget ran out.
//
// The verdict counts — it arrived — but everyone after it is starved, so this
// is the line that says who spent the event. Without it the starvation counters
// name the victims and nothing names the cause.
func (b *Bus) reportLateVerdict(ctx context.Context, sub *subscriber, event string, took time.Duration) {
	if ctx.Err() == nil {
		return
	}
	slog.Warn("plugin event verdict arrived as the budget ran out", "plugin", sub.id,
		"event", event, "took", took, "budget", b.budget)
}

func (b *Bus) enqueueEffects(sub *subscriber, event string, effects []abi.HostCall) {
	for _, effect := range effects {
		if err := b.host.Enqueue(effect); err != nil {
			slog.Error("queue plugin event effect", "plugin", sub.id, "event", event, "effect", effect.Type, "err", err)
		}
	}
}

func (b *Bus) recordStarved(subscribers []*subscriber, event string) {
	for _, sub := range subscribers {
		if !sub.health.isDisabled() {
			sub.health.recordStarved(event)
		}
	}
}

func (b *Bus) subscribers(event string) []*subscriber {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]*subscriber(nil), b.subs[event]...)
}

func failureAllows(event *abi.Event) bool {
	return event.OnFailure != abi.FailureDeny
}
