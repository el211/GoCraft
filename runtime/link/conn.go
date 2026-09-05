package link

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	wire "github.com/GoCraft-MC/gocraft-abi/abi/v1/wire"
	"github.com/GoCraft-MC/gocraft-abi/ipc"
)

// ErrConnClosed is returned to anyone still waiting when the connection ends
// for a reason that is not a read failure — a deliberate Close, typically.
var ErrConnClosed = errors.New("ipc: connection closed")

// Conn multiplexes request and reply over one framed stream.
//
// A single socket carries every exchange with a runtime: an event dispatched to
// one plugin, a ping in flight, a load waiting to be acknowledged. Replies
// therefore arrive in whatever order the runtime produces them, and each has to
// wake the caller that asked for it — which is what the sequence number is for.
//
// One goroutine owns reading. Callers never touch the stream: they register a
// pending sequence number, send, and wait on a channel.
type Conn struct {
	codec   *ipc.Codec
	handler func(*wire.Envelope)
	seq     atomic.Uint64
	done    chan struct{}

	mu      sync.Mutex
	pending map[uint64]chan *wire.Envelope
	closing bool
	closed  bool
	err     error
}

// NewConn starts the read loop and returns immediately.
//
// The handshake is deliberately not handled here: the host reads HELLO straight
// off the codec and answers WELCOME before there is anything to multiplex.
// Passing an already-greeted codec keeps the sequencing rules of this type
// simple — everything it sees from now on is either a reply or unsolicited.
//
// handler receives envelopes that answer no pending request. It runs on the
// read goroutine, so it must not block: while it runs, nothing is being read.
func NewConn(codec *ipc.Codec, handler func(*wire.Envelope)) *Conn {
	conn := &Conn{
		codec:   codec,
		handler: handler,
		done:    make(chan struct{}),
		pending: make(map[uint64]chan *wire.Envelope),
	}
	go conn.read()
	return conn
}

// Request sends an envelope and waits for the reply carrying the same sequence
// number. The sequence number is assigned here; any value already set on the
// envelope is overwritten.
//
// ctx is what bounds the wait — for an event that is the shared budget of §06.
// When it expires the caller gives up, but the runtime may still answer later;
// the pending entry is dropped so that reply is discarded rather than delivered
// to whoever reuses the channel next.
func (c *Conn) Request(ctx context.Context, envelope *wire.Envelope) (*wire.Envelope, error) {
	if envelope == nil {
		return nil, fmt.Errorf("ipc: missing envelope")
	}
	// The host numbers its requests odd: 1, 3, 5. A runtime numbers the only
	// exchange it starts — EMIT — even, from 2.
	//
	// Splitting the space rather than sharing a counter is what keeps a
	// runtime-initiated request from being mistaken for a reply. Both sides
	// number from their own counter, so a shared space would eventually put the
	// same number on a host request in flight and an emission just sent, and
	// the read loop below would hand the emission to whoever was waiting on
	// that number. It would look like the runtime answered the wrong question.
	//
	// Zero belongs to neither and still means "correlated with nothing".
	seq := c.seq.Add(2) - 1
	envelope.Seq = seq

	replies, err := c.expect(seq)
	if err != nil {
		return nil, err
	}
	defer c.forget(seq)

	if err := c.codec.Send(envelope); err != nil {
		return nil, err
	}
	select {
	case reply, ok := <-replies:
		if !ok {
			return nil, c.Err()
		}
		return reply, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, c.Err()
	}
}

// Send writes an envelope without expecting an answer: READY, SHUTDOWN, and the
// host side of the handshake.
func (c *Conn) Send(envelope *wire.Envelope) error {
	return c.codec.Send(envelope)
}

// Done is closed once the read loop has stopped, whatever the reason. A
// supervisor watches it to decide when to respawn.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err reports why the connection ended, or nil while it is still running. A
// clean io.EOF means the peer closed between frames.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close ends the connection and wakes everyone waiting on it. Closing the
// stream is what unblocks the read loop; the shutdown below runs anyway so the
// outcome does not depend on a stream that has nothing to close.
//
// The intent is recorded before the stream is touched, because closing it makes
// the read loop fail and race Close to report why the connection ended. Whoever
// wins, Err must say ErrConnClosed: a supervisor that cannot tell a deliberate
// stop from a dead runtime respawns processes it just asked to leave.
func (c *Conn) Close() error {
	c.mu.Lock()
	c.closing = true
	c.mu.Unlock()

	err := c.codec.Close()
	c.shutdown(ErrConnClosed)
	return err
}

func (c *Conn) read() {
	for {
		envelope, err := c.codec.Receive()
		if err != nil {
			c.shutdown(err)
			return
		}
		// Only an odd sequence number can answer this side's request, so an
		// even one skips the pending table entirely rather than relying on a
		// miss. A runtime that echoed a host seq back on an unrelated message
		// would otherwise be one collision away from waking the wrong caller.
		c.mu.Lock()
		replies, waiting := c.pending[envelope.GetSeq()]
		if envelope.GetSeq()%2 == 0 {
			replies, waiting = nil, false
		}
		if waiting {
			// Removed on delivery, so a runtime that answers the same request
			// twice cannot have its second reply mistaken for another one.
			delete(c.pending, envelope.GetSeq())
		}
		c.mu.Unlock()

		if waiting {
			// The channel is buffered, so this never blocks even when the
			// caller has already given up and stopped reading it.
			replies <- envelope
			continue
		}
		if c.handler != nil {
			c.handler(envelope)
		}
	}
}

func (c *Conn) expect(seq uint64) (chan *wire.Envelope, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, c.err
	}
	replies := make(chan *wire.Envelope, 1)
	c.pending[seq] = replies
	return replies, nil
}

func (c *Conn) forget(seq uint64) {
	c.mu.Lock()
	delete(c.pending, seq)
	c.mu.Unlock()
}

// shutdown records why the connection ended and closes every pending channel,
// so a caller blocked on a reply that will never come is woken rather than left
// parked until its context expires. It is safe to call more than once.
func (c *Conn) shutdown(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	if c.closing {
		// The read failed because Close pulled the stream out from under it.
		// That is not the runtime failing.
		err = ErrConnClosed
	}
	c.closed = true
	c.err = err
	pending := c.pending
	c.pending = nil
	c.mu.Unlock()

	for _, replies := range pending {
		close(replies)
	}
	close(c.done)
}
