package stt

import (
	"testing"
	"time"

	"livecaption/internal/audio"
	"livecaption/internal/metrics"
)

// TestRing_CapturedAtRoundTrip checks that push/pop carry a chunk's
// CapturedAt through unchanged, since that value is what the anchor index
// ultimately keys latency off of.
func TestRing_CapturedAtRoundTrip(t *testing.T) {
	// Real metrics and gate, not nil: push consults both on eviction, and this
	// cap is only large enough to avoid one by accident.
	r := newRing(1<<20, metrics.New("test", "test"), NewGate(PauseConfig{}))
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
