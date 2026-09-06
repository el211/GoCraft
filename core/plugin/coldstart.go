package plugin

import (
	"sync"
	"time"
)

// defaultColdGrace is what an event gets on top of its budget the first time a
// subscriber sees it.
//
// The number comes from measurement, not from taste. A dispatch into a warm JVM
// costs about 100 µs; the first one to reach a given handler costs 2 to 4 ms,
// because the plugin's own classes are initialised and its code runs
// interpreted for the first time. The host warms everything it owns before
// READY — the socket, protobuf on both sides, the codecs, the event class — and
// what is left is the author's code, which a warm-up must not run: the values
// would be placeholders, and a protection handler would be deciding about a
// block nobody broke.
//
// So the cost is real, unavoidable, and paid once per subscriber per event
// type. Charging it to the shared budget means one "deadline exceeded" per
// restart on a perfectly healthy server, and an event whose provider declared
// fail_closed cancelled the first action after every boot. Twenty milliseconds
// is far above what was measured and well under a 50 ms tick, so it is invisible
// where it lands and generous where it is needed.
//
// This is a departure from §06, which gives an event one budget and no
// exceptions. §06 is right about the steady state and does not describe a cold
// one; the invariant it actually protects — that a plugin cannot hold the tick
// — is kept, because the grace is bounded and spent once.
const defaultColdGrace = 20 * time.Millisecond

// coldStarts remembers which event types a subscriber has already answered.
//
// Per subscriber and per event type, because that is how the cost behaves: the
// first handler to run in a process pays for the plugin's classes, and a second
// handler that logs pays for the stream it writes to. Both are once.
//
// A respawned runtime is a new process with everything cold again, and it
// arrives as a new subscriber — so this resets by being replaced, without
// anything having to notice.
type coldStarts struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

func newColdStarts() *coldStarts {
	return &coldStarts{seen: make(map[string]struct{})}
}

// cold reports whether this subscriber has yet to answer this event type.
func (c *coldStarts) cold(event string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ran := c.seen[event]
	return !ran
}

// ran records that a dispatch actually reached the subscriber, whatever it
// answered. A failure warms a process exactly as well as a verdict does.
func (c *coldStarts) ran(event string) {
	c.mu.Lock()
	c.seen[event] = struct{}{}
	c.mu.Unlock()
}

// SetColdGrace replaces the allowance a first dispatch gets. Zero turns it off.
//
// A setter rather than a constructor parameter: every caller of NewRegistry
// would otherwise have to name a number it has no opinion about, and the
// default is right for all of them but a server reading a config file.
func (b *Bus) SetColdGrace(grace time.Duration) {
	if grace < 0 {
		grace = 0
	}
	b.coldGrace = grace
}

// budgetFor is the deadline one event gets, which is the shared budget plus the
// grace when any subscriber is about to see this type for the first time.
//
// One deadline for the whole event either way — §06's rule, and the reason N
// plugins cannot multiply the cost. The grace is added once and not per cold
// subscriber, so a server that loads ten plugins on the same event does not get
// two hundred milliseconds of tick.
func (b *Bus) budgetFor(subscribers []*subscriber, event string) (time.Duration, bool) {
	for _, sub := range subscribers {
		if sub.cold.cold(event) {
			return b.budget + b.coldGrace, true
		}
	}
	return b.budget, false
}
