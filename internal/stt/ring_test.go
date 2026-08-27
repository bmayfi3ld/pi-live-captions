package stt

import (
	"testing"
	"time"

	"livecaption/internal/audio"
)

// TestRing_CapturedAtRoundTrip checks that push/pop carry a chunk's
// CapturedAt through unchanged, since that value is what the anchor index
// ultimately keys latency off of.
func TestRing_CapturedAtRoundTrip(t *testing.T) {
	r := newRing(1<<20, nil, nil)
	now := time.Now()

	r.push(audio.Frame{PCM: []byte{1, 2, 3, 4}, CapturedAt: now})

	c, ok := r.pop()
	if !ok {
		t.Fatal("pop: expected a chunk")
	}
	if !c.capturedAt.Equal(now) {
		t.Errorf("capturedAt = %v, want %v", c.capturedAt, now)
	}
	if len(c.pcm) != 4 {
		t.Errorf("pcm len = %d, want 4", len(c.pcm))
	}
}
