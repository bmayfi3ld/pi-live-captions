package audio

import (
	"context"
	"io"
	"log/slog"
	"sync"
)

// audioChunk is the read size off the MP3 pipe. At 128 kbit/s this is ~0.5 s
// of audio, small enough that a fresh listener starts hearing the room
// promptly and large enough to keep the fan-out cheap.
const audioChunk = 8 * 1024

// audioBacklog is how many chunks a listener may fall behind by before its
// chunks are dropped — roughly two seconds, matching the monitor's buffer
// depth for the same reason: deep enough to ride out a scheduling hiccup,
// shallow enough that a wedged listener is noticed rather than buffered.
const audioBacklog = 4

// Broadcaster fans one MP3 byte stream out to any number of HTTP listeners.
//
// Shaped after Monitor: a slow or dead listener is dropped, never allowed to
// stall the read loop, because the same ffmpeg producing these bytes is
// producing the PCM the recognizer needs. There is no backlog by design — a
// listener joins at the live edge and its decoder resyncs at the next frame
// header.
type Broadcaster struct {
	log     *slog.Logger
	onDrop  func()
	onAlive func(bool)

	// mu guards subs and closed. Only a sender may close a channel, so
	// unsubscribe and Close take this lock to make themselves one.
	mu     sync.Mutex
	subs   map[chan []byte]struct{}
	closed bool
}

func NewBroadcaster(log *slog.Logger) *Broadcaster {
	if log == nil {
		log = slog.Default()
	}
	return &Broadcaster{log: log, subs: make(map[chan []byte]struct{})}
}

// SetCallbacks registers metric hooks. Set before Run.
func (b *Broadcaster) SetCallbacks(onDrop func(), onAlive func(bool)) {
	b.onDrop, b.onAlive = onDrop, onAlive
}

// Run reads r until EOF, publishing to every subscriber. It returns when the
// stream ends — a live capture restart ends one Run and starts the next, and
// listeners never notice because their connections outlive it.
func (b *Broadcaster) Run(ctx context.Context, r io.Reader) {
	b.setAlive(true)
	defer b.setAlive(false)

	buf := make([]byte, audioChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			b.publish(buf[:n])
		}
		if err != nil {
			if ctx.Err() == nil && err != io.EOF {
				b.log.Debug("audio stream read ended", "err", err)
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func (b *Broadcaster) publish(chunk []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || len(b.subs) == 0 {
		return
	}
	// One copy shared by every subscriber: buf is reused on the next Read,
	// and nobody mutates a published chunk.
	c := append([]byte(nil), chunk...)
	for ch := range b.subs {
		select {
		case ch <- c:
		default:
			if b.onDrop != nil {
				b.onDrop()
			}
		}
	}
}

// Subscribe returns a channel of MP3 chunks and the func that ends the
// subscription. The channel is closed when the broadcaster shuts down.
func (b *Broadcaster) Subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, audioBacklog)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if _, ok := b.subs[ch]; ok {
				delete(b.subs, ch)
				close(ch)
			}
		})
	}
}

func (b *Broadcaster) setAlive(v bool) {
	if b.onAlive != nil {
		b.onAlive(v)
	}
}

// Close ends every subscription. Safe to call more than once.
func (b *Broadcaster) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for ch := range b.subs {
		delete(b.subs, ch)
		close(ch)
	}
}
