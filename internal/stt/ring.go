package stt

import (
	"sync"
	"time"

	"livecaption/internal/audio"
	"livecaption/internal/metrics"
)

// chunk is one ring entry: PCM plus the wall time it was captured, which is
// what latency is ultimately measured against.
type chunk struct {
	pcm        []byte
	capturedAt time.Time
	active     bool
}

// ring holds PCM chunks while the connection is down or catching up,
// dropping the oldest chunk once full so a blip never backpressures capture.
type ring struct {
	mu       sync.Mutex
	chunks   []chunk
	bytes    int
	capBytes int
	met      *metrics.Metrics
	gate     *Gate

	// notify wakes writeLoop when data arrives; buffered 1 and non-blocking
	// to push so a slow or absent reader of it never stalls push.
	notify chan struct{}
}

func newRing(capBytes int, met *metrics.Metrics, gate *Gate) *ring {
	return &ring{capBytes: capBytes, met: met, gate: gate, notify: make(chan struct{}, 1)}
}

func (r *ring) push(f audio.Frame) {
	r.mu.Lock()
	r.chunks = append(r.chunks, chunk{pcm: f.PCM, capturedAt: f.CapturedAt, active: r.gate.Active()})
	r.bytes += len(f.PCM)
	for r.bytes > r.capBytes && len(r.chunks) > 1 {
		dropped := r.chunks[0]
		r.chunks = r.chunks[1:]
		r.bytes -= len(dropped.pcm)
		// Classify evictions by the discarded chunk's admission-time gate
		// state: paused pre-roll rotation is expected even after a resume,
		// while active-period audio loss remains degradation.
		if dropped.active {
			r.met.STTBufferDrop()
		}
	}
	r.mu.Unlock()

	select {
	case r.notify <- struct{}{}:
	default:
	}
}

func (r *ring) pop() (chunk, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.chunks) == 0 {
		return chunk{}, false
	}
	c := r.chunks[0]
	r.chunks = r.chunks[1:]
	r.bytes -= len(c.pcm)
	return c, true
}

func (r *ring) empty() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.chunks) == 0
}
