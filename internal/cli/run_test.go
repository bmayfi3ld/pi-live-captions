package cli

import (
	"testing"
	"time"

	"livecaption/internal/metrics"
	"livecaption/internal/stt"
)

// observeLatency touches only s.met, so a session built with nothing but a
// fresh Metrics is a sufficient fixture for these tests.
func newLatencySession() *session {
	return &session{met: metrics.New("test", "session")}
}

func TestObserveLatency_UsesCapturedAt(t *testing.T) {
	s := newLatencySession()
	now := time.Now()

	s.observeLatency(stt.Transcript{
		Words:      stt.Untimed("hello"),
		ReceivedAt: now,
		CapturedAt: now.Add(-300 * time.Millisecond),
	}, time.Time{})

	snap := s.met.Snapshot()
	if snap.STT.LatencyCount != 1 {
		t.Fatalf("LatencyCount = %d, want 1", snap.STT.LatencyCount)
	}
	if got := snap.STT.LatencyLast; got < 295 || got > 305 {
		t.Errorf("LatencyLast = %v, want ~300ms", got)
	}
}

func TestObserveLatency_IgnoresZeroCapturedAt(t *testing.T) {
	s := newLatencySession()

	// A missing CapturedAt means the engine couldn't resolve it — record
	// nothing rather than resurrecting the old, unbounded StartedAt-relative
	// figure.
	s.observeLatency(stt.Transcript{
		Words:      stt.Untimed("hello"),
		ReceivedAt: time.Now(),
	}, time.Time{})

	if got := s.met.Snapshot().STT.LatencyCount; got != 0 {
		t.Errorf("LatencyCount = %d, want 0 for a zero CapturedAt", got)
	}
}

func TestObserveLatency_KeepsSmallSample(t *testing.T) {
	s := newLatencySession()
	now := time.Now()

	// The old formula clipped d <= 0 before recording; the new one must not
	// clip a small-but-real 2ms sample, since both timestamps come from
	// time.Now() in-process and Sub uses the monotonic reading.
	s.observeLatency(stt.Transcript{
		Words:      stt.Untimed("hello"),
		ReceivedAt: now,
		CapturedAt: now.Add(-2 * time.Millisecond),
	}, time.Time{})

	snap := s.met.Snapshot()
	if snap.STT.LatencyCount != 1 {
		t.Fatalf("LatencyCount = %d, want 1", snap.STT.LatencyCount)
	}
	if got := snap.STT.LatencyLast; got < 1 || got > 3 {
		t.Errorf("LatencyLast = %v, want ~2ms", got)
	}
}

// TestObserveLatency_IgnoresEmptyText guards against a future engine
// emitting a synthetic zero-range result with real ReceivedAt/CapturedAt but
// no text — decodeTranscript already rejects an empty alternative today, so
// this is belt-and-braces: without the Text guard, idx.At(0) could resolve
// an unrelated capture instant and record pure noise into the series.
func TestObserveLatency_IgnoresEmptyText(t *testing.T) {
	s := newLatencySession()
	now := time.Now()

	s.observeLatency(stt.Transcript{
		Words:      stt.Untimed(""),
		ReceivedAt: now,
		CapturedAt: now.Add(-300 * time.Millisecond),
	}, time.Time{})

	snap := s.met.Snapshot()
	if snap.STT.LatencyCount != 0 {
		t.Errorf("LatencyCount = %d, want 0 for empty Text", snap.STT.LatencyCount)
	}
}

// TestObserveLatency_PhasesSumToTotal pins the invariant the /admin stacked
// bar depends on: upload + recognize + assemble must exactly equal
// publishedAt - CapturedAt (one stage further than the headline final
// latency, which stops at ReceivedAt). Fixed timestamps make the sums exact
// rather than approximate.
func TestObserveLatency_PhasesSumToTotal(t *testing.T) {
	s := newLatencySession()

	captured := time.Unix(1000, 0)
	sent := captured.Add(50 * time.Millisecond)
	received := sent.Add(120 * time.Millisecond)
	published := received.Add(5 * time.Millisecond)

	s.observeLatency(stt.Transcript{
		Words:      stt.Untimed("hello"),
		CapturedAt: captured,
		SentAt:     sent,
		ReceivedAt: received,
	}, published)

	snap := s.met.Snapshot()
	if snap.STT.PhaseLatencyCount != 1 {
		t.Fatalf("PhaseLatencyCount = %d, want 1", snap.STT.PhaseLatencyCount)
	}
	sum := snap.STT.UploadLatencyLast + snap.STT.RecognizeLatencyLast + snap.STT.AssembleLatencyLast
	want := published.Sub(captured).Seconds() * 1000
	if sum != want {
		t.Errorf("phase sum = %v, want exactly %v (published - captured)", sum, want)
	}
}

// TestObserveLatency_ZeroSentAtRecordsTotalNoPhases covers an engine that
// resolved CapturedAt/ReceivedAt but not SentAt: the total latency is still
// sound and must be recorded, but the phase split cannot be attributed and
// must be skipped rather than recording a bogus zero-length upload phase.
func TestObserveLatency_ZeroSentAtRecordsTotalNoPhases(t *testing.T) {
	s := newLatencySession()
	now := time.Now()

	s.observeLatency(stt.Transcript{
		Words:      stt.Untimed("hello"),
		ReceivedAt: now,
		CapturedAt: now.Add(-300 * time.Millisecond),
	}, now.Add(305*time.Millisecond))

	snap := s.met.Snapshot()
	if snap.STT.LatencyCount != 1 {
		t.Errorf("LatencyCount = %d, want 1", snap.STT.LatencyCount)
	}
	if snap.STT.PhaseLatencyCount != 0 {
		t.Errorf("PhaseLatencyCount = %d, want 0 for a zero SentAt", snap.STT.PhaseLatencyCount)
	}
}
