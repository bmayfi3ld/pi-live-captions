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
// instant those samples were captured and to their position on the audio
// source's own clock. A recognizer's media times count the audio it has
// received on the current stream, so the mapping is only meaningful for one
// connection: a fresh index is built per connection and discarded with it.
// That is why there is no Reset — lifetime IS the reset.
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
//
// sourceEnd is where that same chunk ends on the audio source's own clock:
// the Frame.Offset the chunk was built from, i.e. one past its last sample's
// position in the source session. Unlike the wall-clock fields it does NOT
// interpolate within the chunk from anything — it IS the chunk's end — so
// SourceAt() walks it back by the byte gap the same way At() walks capturedAt.
// Zero means the frame carried no offset, and nothing may fabricate one.
type anchor struct {
	startByte  int64
	endByte    int64         // one past the chunk's last byte
	capturedAt time.Time     // capture instant of the sample at endByte-1; zero if unknown
	sentAt     time.Time     // instant the whole chunk was handed to the socket; zero if unknown
	sourceEnd  time.Duration // source-session offset of endByte; zero if unknown
}

func newAnchorIndex(f audio.Format) *anchorIndex {
	return &anchorIndex{format: f}
}

// Add records that n more bytes were just written to the stream, with the
// chunk's last sample captured at capturedAt (zero if unknown), the whole
// chunk handed to the socket at sentAt (zero if unknown), and the chunk
// ending at sourceEnd on the source-session clock (zero if the frame
// carried no offset). Must be called in stream order, immediately before the
// corresponding conn.Write — see the call site in writeLoop for why
// "before" matters.
func (a *anchorIndex) Add(n int, capturedAt, sentAt time.Time, sourceEnd time.Duration) {
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
		sourceEnd:  sourceEnd,
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

// covering finds the entry that covers byte position want, clamping a want
// at or past everything written to the newest entry: recognizers round
// start+duration to 2-3 decimals, so a final can land a few ms past our byte
// count, and that is floating-point rounding on the far side rather than an
// error. ok is false when want precedes the oldest surviving entry (already
// evicted) or the index is empty. A want that lands exactly on a chunk
// boundary resolves to the FOLLOWING chunk — the boundary byte is that
// chunk's first sample — which is what keeps a word edge straddling two
// chunks nondecreasing.
//
// Callers must hold a.mu.
func (a *anchorIndex) covering(want int64) (anchor, bool) {
	if len(a.entries) == 0 {
		return anchor{}, false
	}
	if want >= a.written {
		return a.entries[len(a.entries)-1], true
	}
	if want < a.entries[0].startByte {
		return anchor{}, false
	}
	i := sort.Search(len(a.entries), func(i int) bool {
		return a.entries[i].endByte > want
	})
	if i == len(a.entries) {
		// Unreachable given the clamp above, but stay defensive.
		return anchor{}, false
	}
	return a.entries[i], true
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

	want := int64(a.format.BytesFor(media))
	e, ok := a.covering(want)
	if !ok || e.capturedAt.IsZero() {
		return time.Time{}, time.Time{}, false
	}

	// Clamped to the newest entry (want at or past everything written): its
	// stamps verbatim, no interpolation — there is nothing ahead to
	// interpolate towards.
	if want >= e.endByte {
		return e.capturedAt, e.sentAt, true
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

// SourceAt resolves the source-session offset of the sample at provider
// media time media, through the same covering-entry lookup At uses: the
// connection's byte clock maps onto the source clock chunk by chunk, so
// pre-roll, a reconnect, or audio the ring discarded mid-stream shows up as a
// real discontinuity between adjacent entries rather than one constant
// offset for the whole session. ok is false when media falls outside the
// retained entries, or when the covering chunk's frame carried no offset
// (sourceEnd zero) — an unknown source position is reported as unavailable,
// never fabricated.
func (a *anchorIndex) SourceAt(media time.Duration) (source time.Duration, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	want := int64(a.format.BytesFor(media))
	e, ok := a.covering(want)
	if !ok || e.sourceEnd == 0 {
		return 0, false
	}

	// Clamped to the newest entry: the end of everything actually sent.
	if want >= e.endByte {
		return e.sourceEnd, true
	}

	// e.sourceEnd marks endByte, one past the chunk's last sample; walk back
	// the byte gap to the position asked for, the mirror of At's capturedAt
	// interpolation. A frame that claimed to end before its own duration
	// (which only a lying producer can do) can walk below zero; clamp rather
	// than hand back a negative source position.
	source = e.sourceEnd - a.format.Duration(int(e.endByte-want))
	if source < 0 {
		source = 0
	}
	return source, true
}
