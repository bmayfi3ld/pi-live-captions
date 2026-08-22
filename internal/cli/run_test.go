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
		Text:       "hello",
		IsFinal:    true,
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
		Text:       "hello",
		IsFinal:    true,
		ReceivedAt: time.Now(),
	}, time.Time{})

	if got := s.met.Snapshot().STT.LatencyCount; got != 0 {
		t.Errorf("LatencyCount = %d, want 0 for a zero CapturedAt", got)
	}
}

// TestObserveLatency_RoutesInterimsToInterimSeries replaces the old
// "IgnoresInterims" test: interims are no longer dropped, they go to their
// own series, since that's what a viewer actually sees first.
func TestObserveLatency_RoutesInterimsToInterimSeries(t *testing.T) {
	s := newLatencySession()
	now := time.Now()

	s.observeLatency(stt.Transcript{
		Text:       "hello",
		IsFinal:    false,
		ReceivedAt: now,
		CapturedAt: now.Add(-300 * time.Millisecond),
	}, time.Time{})

	snap := s.met.Snapshot()
	if snap.STT.LatencyCount != 0 {
		t.Errorf("LatencyCount = %d, want 0 for an interim", snap.STT.LatencyCount)
	}
	if snap.STT.InterimLatencyCount != 1 {
		t.Errorf("InterimLatencyCount = %d, want 1 for an interim", snap.STT.InterimLatencyCount)
	}
}

func TestObserveLatency_KeepsSmallSample(t *testing.T) {
	s := newLatencySession()
	now := time.Now()

	// The old formula clipped d <= 0 before recording; the new one must not
	// clip a small-but-real 2ms sample, since both timestamps come from
	// time.Now() in-process and Sub uses the monotonic reading.
	s.observeLatency(stt.Transcript{
		Text:       "hello",
		IsFinal:    true,
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

// TestObserveLatency_IgnoresEmptyText is the regression guard for Deepgram's
// synthetic UtteranceEnd shape: SpeechFinal true, IsFinal false, empty Text,
// zero Start/Duration, but real ReceivedAt/CapturedAt. Without the Text guard
// idx.At(0) can resolve an unrelated capture instant, which would record pure
// noise into the interim series.
func TestObserveLatency_IgnoresEmptyText(t *testing.T) {
	s := newLatencySession()
	now := time.Now()

	s.observeLatency(stt.Transcript{
		Text:        "",
		IsFinal:     false,
		SpeechFinal: true,
		ReceivedAt:  now,
		CapturedAt:  now.Add(-300 * time.Millisecond),
	}, time.Time{})

	snap := s.met.Snapshot()
	if snap.STT.LatencyCount != 0 {
		t.Errorf("LatencyCount = %d, want 0 for empty Text", snap.STT.LatencyCount)
	}
	if snap.STT.InterimLatencyCount != 0 {
		t.Errorf("InterimLatencyCount = %d, want 0 for empty Text", snap.STT.InterimLatencyCount)
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
		Text:       "hello",
		IsFinal:    true,
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
		Text:       "hello",
		IsFinal:    true,
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
