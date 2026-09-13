package stt

import (
	"testing"
	"time"

	"livecaption/internal/audio"
	"livecaption/internal/metrics"
)

func pushRingFrame(r *ring, gate *Gate, pcm byte, capturedAt time.Time, offset time.Duration, open bool) {
	f := audio.Frame{
		PCM:           []byte{pcm},
		CapturedAt:    capturedAt,
		Offset:        offset,
		NoiseGated:    true,
		NoiseGateOpen: open,
	}
	gate.Observe(f)
	r.push(f)
}

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
		t.Errorf("pcm len = %d, want %d", len(c.pcm), 4)
	}
}

func TestRing_PausedEvictionOnResume(t *testing.T) {
	gate := NewGate(testCfg())
	met := metrics.New("test", "test")
	r := newRing(3, met, gate)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	push := func(pcm byte, offset time.Duration, open bool) {
		pushRingFrame(r, gate, pcm, now.Add(offset), offset, open)
	}

	push(1, 0, false)
	push(2, time.Second, false) // gate pauses
	if _, ok := r.pop(); !ok {  // discard the initial active-period frame
		t.Fatal("pop: expected initial active-period frame")
	}
	push(3, 2*time.Second, false)
	push(4, 3*time.Second, false) // full paused-period buffer

	push(5, 4*time.Second, true) // gate resumes; evicts paused-period audio
	if got := met.Snapshot(); got.STT.BufferDrops != 0 || got.Health != "ok" {
		t.Fatalf("after paused eviction: drops = %d, health = %q; want 0, ok", got.STT.BufferDrops, got.Health)
	}
	for _, want := range []struct {
		pcm    byte
		offset time.Duration
	}{{3, 2 * time.Second}, {4, 3 * time.Second}, {5, 4 * time.Second}} {
		c, ok := r.pop()
		if !ok || len(c.pcm) != 1 || c.pcm[0] != want.pcm || !c.capturedAt.Equal(now.Add(want.offset)) {
			t.Fatalf("retained chunk = %#v, %t; want PCM %d at %v", c, ok, want.pcm, now.Add(want.offset))
		}
	}

	for i := byte(6); i <= 9; i++ {
		offset := time.Duration(i-1) * time.Second
		push(i, offset, true)
	}
	if got := met.Snapshot(); got.STT.BufferDrops != 1 || got.Health != "degraded" {
		t.Fatalf("after active eviction: drops = %d, health = %q; want 1, degraded", got.STT.BufferDrops, got.Health)
	}
}

func TestRing_EvictionAccountingAcrossGateTransitions(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("paused rotation stays clean", func(t *testing.T) {
		gate := NewGate(testCfg())
		gate.ObserveLevel(-80, 0)
		gate.ObserveLevel(-80, time.Second)
		met := metrics.New("test", "test")
		r := newRing(2, met, gate)
		for i := byte(1); i <= 3; i++ {
			offset := time.Duration(i) * time.Second
			pushRingFrame(r, gate, i, now.Add(offset), offset, false)
		}
		if got := met.Snapshot(); got.STT.BufferDrops != 0 || got.Health != "ok" {
			t.Fatalf("drops = %d, health = %q; want 0, ok", got.STT.BufferDrops, got.Health)
		}
	})

	t.Run("active chunks count after pause", func(t *testing.T) {
		gate := NewGate(testCfg())
		met := metrics.New("test", "test")
		r := newRing(2, met, gate)
		pushRingFrame(r, gate, 1, now, 0, true)
		pushRingFrame(r, gate, 2, now.Add(500*time.Millisecond), 500*time.Millisecond, true)
		pushRingFrame(r, gate, 3, now.Add(time.Second), time.Second, false)
		pushRingFrame(r, gate, 4, now.Add(2*time.Second), 2*time.Second, false)
		if got := met.Snapshot(); got.STT.BufferDrops != 2 || got.Health != "degraded" {
			t.Fatalf("drops = %d, health = %q; want 2, degraded", got.STT.BufferDrops, got.Health)
		}
	})

	t.Run("disabled auto-pause counts every eviction", func(t *testing.T) {
		gate := NewGate(PauseConfig{})
		met := metrics.New("test", "test")
		r := newRing(2, met, gate)
		for i := byte(1); i <= 3; i++ {
			offset := time.Duration(i) * time.Second
			pushRingFrame(r, gate, i, now.Add(offset), offset, false)
		}
		if got := met.Snapshot(); got.STT.BufferDrops != 1 || got.Health != "degraded" {
			t.Fatalf("drops = %d, health = %q; want 1, degraded", got.STT.BufferDrops, got.Health)
		}
	})

	t.Run("paused evictions preserve existing degradation", func(t *testing.T) {
		gate := NewGate(testCfg())
		gate.ObserveLevel(-80, 0)
		gate.ObserveLevel(-80, time.Second)
		met := metrics.New("test", "test")
		met.DropFrame()
		met.STTBufferDrop()
		r := newRing(2, met, gate)
		for i := byte(1); i <= 3; i++ {
			offset := time.Duration(i) * time.Second
			pushRingFrame(r, gate, i, now.Add(offset), offset, false)
		}
		if got := met.Snapshot(); got.Source.FramesDropped != 1 || got.STT.BufferDrops != 1 || got.Health != "degraded" {
			t.Fatalf("source drops = %d, buffer drops = %d, health = %q; want 1, 1, degraded", got.Source.FramesDropped, got.STT.BufferDrops, got.Health)
		}
	})
}
