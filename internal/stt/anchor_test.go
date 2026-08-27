package stt

import (
	"testing"
	"time"

	"livecaption/internal/audio"
)

// samplePeriod is the duration of one 16-bit mono sample at the pipeline
// rate: the tolerance interpolation tests are held to.
func samplePeriod() time.Duration {
	return audio.PipelineFormat.Duration(2)
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// TestAnchorIndex_Interpolation feeds ten 3200-byte (100ms) chunks, chunk i
// (0-indexed) capturing bytes [i*3200, (i+1)*3200) with its last sample
// stamped t0 + 100ms*(i+1) — the real semantics, since CapturedAt always
// marks a chunk's LAST sample. Each chunk is also stamped a distinct sentAt,
// one chunk-length after its capturedAt, so the two never coincide and a bug
// that mixed them up would show. At(250ms) falls 50ms before the boundary of
// the third chunk (media [200ms,300ms)), so capturedAt must land near
// t0+250ms, not on the chunk's own stamp of t0+300ms; a naive "whole chunk =
// one instant" implementation would be off by up to the full 100ms chunk
// length. sentAt, in contrast, is NOT interpolated: the whole chunk was
// handed to the socket at one instant, so At(250ms) must return the third
// chunk's sentAt verbatim, unadjusted by the same 50ms byte-gap walk-back
// that capturedAt gets.
func TestAnchorIndex_Interpolation(t *testing.T) {
	idx := newAnchorIndex(audio.PipelineFormat)
	t0 := time.Now()

	for i := 0; i < 10; i++ {
		capturedAt := t0.Add(time.Duration(i+1) * 100 * time.Millisecond)
		sentAt := capturedAt.Add(100 * time.Millisecond)
		idx.Add(3200, capturedAt, sentAt)
	}

	gotCaptured, gotSent, ok := idx.At(250 * time.Millisecond)
	if !ok {
		t.Fatal("At(250ms) reported not ok")
	}
	wantCaptured := t0.Add(250 * time.Millisecond)
	if d := absDuration(gotCaptured.Sub(wantCaptured)); d > samplePeriod() {
		t.Errorf("capturedAt at 250ms = %v, want ~%v (off by %v, tolerance %v)", gotCaptured, wantCaptured, d, samplePeriod())
	}
	// The third chunk (index 2) covers media [200ms,300ms) and was sent at
	// t0 + 400ms; unlike capturedAt above, this must be exact, not "close".
	wantSent := t0.Add(400 * time.Millisecond)
	if !gotSent.Equal(wantSent) {
		t.Errorf("sentAt at 250ms = %v, want exactly %v (sentAt must not be interpolated)", gotSent, wantSent)
	}
}

// TestAnchorIndex_Eviction writes 60s of chunks (twice anchorWindow) and
// checks that media time near the very start of the stream is no longer
// resolvable, and that the entry count never grows to track the whole
// stream.
func TestAnchorIndex_Eviction(t *testing.T) {
	idx := newAnchorIndex(audio.PipelineFormat)
	t0 := time.Now()

	const chunkBytes = 3200 // 100ms at the pipeline rate
	const chunks = 600      // 600 * 100ms = 60s
	for i := 0; i < chunks; i++ {
		stamp := t0.Add(time.Duration(i+1) * 100 * time.Millisecond)
		idx.Add(chunkBytes, stamp, stamp)
	}

	if _, _, ok := idx.At(1 * time.Second); ok {
		t.Error("At(1s) should be evicted after 60s of chunks, got ok=true")
	}

	// The window is 30s of audio at 100ms/chunk, so entries should hover
	// around 300, nowhere near the 600 chunks actually written.
	if n := len(idx.entries); n > 350 {
		t.Errorf("entries = %d, want bounded well under %d chunks written", n, chunks)
	}
}

// TestAnchorIndex_Clamp checks that media time at or past everything written
// clamps to the newest entry rather than failing: Deepgram rounds
// start+duration to 2-3 decimals, so a final can land a few ms past the byte
// count we tracked.
func TestAnchorIndex_Clamp(t *testing.T) {
	idx := newAnchorIndex(audio.PipelineFormat)
	t0 := time.Now()

	for i := 0; i < 5; i++ {
		stamp := t0.Add(time.Duration(i+1) * 100 * time.Millisecond)
		idx.Add(3200, stamp, stamp)
	}

	written := audio.PipelineFormat.Duration(5 * 3200)
	got, gotSent, ok := idx.At(written + 10*time.Millisecond)
	if !ok {
		t.Fatal("At(written+10ms) reported not ok, want clamp to newest entry")
	}
	want := t0.Add(500 * time.Millisecond)
	if got != want {
		t.Errorf("At(written+10ms) = %v, want newest stamp %v", got, want)
	}
	if gotSent != want {
		t.Errorf("sentAt at (written+10ms) = %v, want newest stamp %v", gotSent, want)
	}
}

// TestAnchorIndex_Refusal covers the two ways At must refuse rather than
// guess: an entry whose capture instant is unknown (zero CapturedAt, as real
// frames built by tests without one produce), and an empty index. It also
// covers the one way At must NOT refuse: a zero sentAt alone still yields a
// usable capturedAt, since a lost send-phase stamp is not the same failure
// as a lost capture stamp — callers just lose the upload/recognition split
// for that transcript, not the whole latency figure.
func TestAnchorIndex_Refusal(t *testing.T) {
	t.Run("zero capturedAt", func(t *testing.T) {
		idx := newAnchorIndex(audio.PipelineFormat)
		idx.Add(3200, time.Time{}, time.Now())
		if _, _, ok := idx.At(50 * time.Millisecond); ok {
			t.Error("At should refuse a chunk with unknown capture time, got ok=true")
		}
	})

	t.Run("zero sentAt", func(t *testing.T) {
		idx := newAnchorIndex(audio.PipelineFormat)
		t0 := time.Now()
		idx.Add(3200, t0.Add(100*time.Millisecond), time.Time{})
		capturedAt, sentAt, ok := idx.At(50 * time.Millisecond)
		if !ok {
			t.Fatal("At should still succeed with a usable capturedAt when only sentAt is unknown, got ok=false")
		}
		if capturedAt.IsZero() {
			t.Error("capturedAt should be resolved even though sentAt is unknown")
		}
		if !sentAt.IsZero() {
			t.Errorf("sentAt = %v, want zero (unknown)", sentAt)
		}
	})

	t.Run("empty index", func(t *testing.T) {
		idx := newAnchorIndex(audio.PipelineFormat)
		if _, _, ok := idx.At(0); ok {
			t.Error("At on an empty index should refuse, got ok=true")
		}
	})
}
