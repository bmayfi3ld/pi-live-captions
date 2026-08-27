package stt

import (
	"sort"
	"sync"
	"time"

	"livecaption/internal/audio"
)

const (
	anchorWindow = 30 * time.Second
	maxAnchors   = 4096
)

// anchorIndex maps a media time on ONE WebSocket stream to the wall-clock
// instant those samples were captured. A recognizer's media times count the
// audio it has received on the current stream, so the mapping is only
// meaningful for one connection: a fresh index is built per connection and
// discarded with it. That is why there is no Reset — lifetime IS the reset.
type anchorIndex struct {
	format audio.Format

	mu      sync.Mutex
	entries []anchor // ascending by endByte
	written int64    // total audio bytes written on this stream
}

// anchor records the wall-clock instant a chunk of audio, ending at endByte,
// was captured, and the wall-clock instant that same chunk was handed to the
// WebSocket. Frames are stamped CapturedAt after the fact (see the audio
// package), so capturedAt always marks the LAST sample of the chunk, never
// the first — At() below accounts for that with interpolation. sentAt, in
// contrast, marks one conn.Write call covering the whole chunk at once, so it
// has no "first sample vs last sample" to interpolate between.
type anchor struct {
	startByte  int64
	endByte    int64     // one past the chunk's last byte
	capturedAt time.Time // capture instant of the sample at endByte-1; zero if unknown
	sentAt     time.Time // instant the whole chunk was handed to the socket; zero if unknown
}

func newAnchorIndex(f audio.Format) *anchorIndex {
	return &anchorIndex{format: f}
}

// Add records that n more bytes were just written to the stream, with the
// chunk's last sample captured at capturedAt (zero if unknown) and the whole
// chunk handed to the socket at sentAt (zero if unknown). Must be called in
// stream order, immediately before the corresponding conn.Write — see the
// call site in writeLoop for why "before" matters.
func (a *anchorIndex) Add(n int, capturedAt, sentAt time.Time) {
	if n <= 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	start := a.written
	a.written += int64(n)
	a.entries = append(a.entries, anchor{
		startByte:  start,
		endByte:    a.written,
		capturedAt: capturedAt,
		sentAt:     sentAt,
	})

	// Evict from the front once the index covers more than anchorWindow of
	// audio, or has accumulated more entries than maxAnchors — whichever
	// comes first. Always keep at least one entry so At() has something to
	// clamp against.
	windowBytes := int64(a.format.BytesFor(anchorWindow))
	for len(a.entries) > 1 && (a.written-a.entries[0].endByte > windowBytes || len(a.entries) > maxAnchors) {
		a.entries = a.entries[1:]
	}
}

// At resolves the wall-clock instants the sample at media time media was
// captured and the chunk covering it was sent, interpolating capturedAt
// within the covering chunk but returning sentAt verbatim (see below). ok is
// false when media falls outside what this index can answer for: before the
// oldest surviving entry (already evicted), or when the covering chunk's
// capture instant is unknown. A covering entry with a zero sentAt does NOT
// fail the lookup by itself — callers still get a usable capturedAt and
// simply lose the send-phase split for that transcript.
func (a *anchorIndex) At(media time.Duration) (capturedAt, sentAt time.Time, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if len(a.entries) == 0 {
		return time.Time{}, time.Time{}, false
	}

	want := int64(a.format.BytesFor(media))

	// Clamp to the newest entry: recognizers round start+duration to 2-3
	// decimals, so a final can land a few ms past our byte count. That is
	// not an error, just floating-point rounding on the far side.
	if want >= a.written {
		last := a.entries[len(a.entries)-1]
		if last.capturedAt.IsZero() {
			return time.Time{}, time.Time{}, false
		}
		return last.capturedAt, last.sentAt, true
	}

	if want < a.entries[0].startByte {
		return time.Time{}, time.Time{}, false
	}

	i := sort.Search(len(a.entries), func(i int) bool {
		return a.entries[i].endByte > want
	})
	if i == len(a.entries) {
		// Unreachable given the clamp above, but stay defensive.
		return time.Time{}, time.Time{}, false
	}
	e := a.entries[i]
	if e.capturedAt.IsZero() {
		return time.Time{}, time.Time{}, false
	}

	// e.capturedAt marks endByte-1, the chunk's LAST sample. want is
	// somewhere inside [startByte, endByte); walk back the byte gap to the
	// sample we actually want. Without this a 100ms chunk injects up to
	// 100ms of quantization error into a figure of the same order.
	t := e.capturedAt.Add(-a.format.Duration(int(e.endByte - want)))

	// e.sentAt, unlike e.capturedAt, is NOT interpolated: the whole chunk was
	// handed to the socket in one conn.Write call at one instant, so every
	// sample in [startByte, endByte) shares the same sentAt. Walking it back
	// by the byte gap the way capturedAt is above would invent a send time
	// that never happened.
	return t, e.sentAt, true
}
