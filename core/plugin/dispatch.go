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
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
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
		b.enqueueEffects(sub, event.Type, verdict.Effects)
		if verdict.Cancelled {
			return false
		}
	}
	return true
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
