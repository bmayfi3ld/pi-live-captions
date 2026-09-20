package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"livecaption/internal/audio"
	"livecaption/internal/metrics"
	"livecaption/internal/stt"
	"livecaption/internal/ui"
)

// observeLatency touches only s.met, so a session built with nothing but a
// fresh Metrics is a sufficient fixture for these tests.
func newLatencySession() *session {
	return &session{met: metrics.New("test", "session")}
}

func newTestTerminal() *ui.Terminal {
	return ui.NewTerminal(ui.Options{Out: io.Discard, Err: io.Discard})
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestLiveUnavailableInputKeepsSessionAlive(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	original := listDevices
	defer func() { listDevices = original }()
	for _, tc := range []struct {
		name, wantLog string
		devices       []audio.Device
	}{
		{"validation rejected", "action=\"restart required after correction\"", []audio.Device{{Backend: "pulse", Name: "other"}}},
		{"empty enumeration", "action=retry", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listDevices = func(context.Context) []audio.Device { return tc.devices }
			var logs bytes.Buffer
			cmd := LiveCmd{
				Device: "selected", Backend: "pulse",
				STTFlags:    STTFlags{Engine: "mock", NoiseThresholdDBFS: -35, NoiseRelease: 3 * time.Second, SilenceHold: time.Minute},
				ServerFlags: ServerFlags{Addr: "127.0.0.1:0", AudioStream: false},
				OutputFlags: OutputFlags{NoTranscript: true},
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- cmd.Run(ctx, newTestTerminal(), slog.New(slog.NewTextHandler(&logs, nil))) }()
			select {
			case err := <-done:
				t.Fatalf("live session exited before cancellation: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("live session after cancellation: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("live session did not stop after cancellation")
			}
			for _, want := range []string{"backend=pulse", "device=selected", tc.wantLog} {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("logs %q missing %q", logs.String(), want)
				}
			}
		})
	}
}

type heldSource struct{}

func (heldSource) Describe() string { return "live input" }
func (heldSource) Start(ctx context.Context) (<-chan audio.Frame, error) {
	out := make(chan audio.Frame)
	go func() {
		<-ctx.Done()
		close(out)
	}()
	return out, nil
}
func (heldSource) Err() error   { return nil }
func (heldSource) Close() error { return nil }

func TestUnavailableInputKeepsSessionAliveUntilCancellation(t *testing.T) {
	for _, state := range []string{"missing", "unavailable"} {
		t.Run(state, func(t *testing.T) {
			term := newTestTerminal()
			s, err := newSession(buildOpts{
				kind: "live", sourceLabel: "alsa:missing", source: heldSource{},
				stt:    STTFlags{Engine: "mock", NoiseThresholdDBFS: -35, NoiseRelease: 3 * time.Second},
				server: ServerFlags{Addr: "127.0.0.1:0", AudioStream: false},
				output: OutputFlags{NoTranscript: true},
			}, term, testLogger())
			if err != nil {
				t.Fatal(err)
			}
			s.met.SourceKind = "live"
			s.met.SetSourceState(state, "input unavailable", state == "missing")

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- s.run(ctx) }()
			select {
			case err := <-done:
				t.Fatalf("session exited before cancellation: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			if health := s.met.Snapshot().Health; health != "degraded" {
				t.Errorf("health = %q, want degraded while input is %s", health, state)
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("session after cancellation: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("session did not stop after cancellation")
			}
			s.shutdown()
		})
	}
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

// TestSessionAuditLifecycle drives a full session with transcript recording
// enabled and asserts the audit stream's lifecycle: session_start first with
// the run's identity, elapsed_ms nondecreasing throughout, the connection
// state transitions recorded as structured records, and — on a clean
// shutdown — session_end as the final complete record, carrying the same
// metrics snapshot the shutdown summary prints. No credential can appear,
// because the metadata type has nowhere to put one.
func TestSessionAuditLifecycle(t *testing.T) {
	s, err := newSession(buildOpts{
		kind: "live", sourceLabel: "alsa:test", source: heldSource{},
		stt:    STTFlags{Engine: "mock", NoiseThresholdDBFS: -35, NoiseRelease: 3 * time.Second},
		server: ServerFlags{Addr: "127.0.0.1:0", AudioStream: false},
		output: OutputFlags{TranscriptDir: t.TempDir()},
	}, newTestTerminal(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if s.writer == nil {
		t.Fatal("writer must be enabled for the audit lifecycle test")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.run(ctx) }()
	time.Sleep(100 * time.Millisecond) // let the mock connect
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	s.shutdown()

	data, err := os.ReadFile(filepath.Join(s.writer.Dir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("invalid audit line %q: %v", line, err)
		}
		records = append(records, rec)
	}
	if len(records) < 3 {
		t.Fatalf("got %d records, want at least session_start, a state change and session_end", len(records))
	}
	if records[0]["type"] != "session_start" {
		t.Errorf("first record = %v, want session_start", records[0])
	}
	if records[0]["version"] != Version {
		t.Errorf("session_start version = %v, want %q", records[0]["version"], Version)
	}
	if _, ok := records[0]["session_id"]; !ok {
		t.Error("session_start missing session_id")
	}
	// Credentials are excluded by construction; assert the whole family is
	// absent from every record, not just the first.
	for _, rec := range records {
		raw, _ := json.Marshal(rec)
		for _, forbidden := range []string{"api_key", "password", "authorization"} {
			if strings.Contains(strings.ToLower(string(raw)), forbidden) {
				t.Errorf("record leaks credential field %q: %s", forbidden, raw)
			}
		}
	}
	// The session timeline is monotonic in elapsed_ms.
	var prev float64
	for i, rec := range records {
		ms := rec["elapsed_ms"].(float64)
		if i > 0 && ms < prev {
			t.Errorf("elapsed_ms went backwards: %v then %v", prev, ms)
		}
		prev = ms
	}
	// The mock connected, and shutdown closed: both transitions recorded.
	states := map[string]bool{}
	for _, rec := range records {
		if rec["type"] == "state" && rec["component"] == "stt" {
			states[rec["state"].(string)] = true
		}
	}
	if !states["connected"] || !states["closed"] {
		t.Errorf("stt state records = %v, want connected and closed", states)
	}
	last := records[len(records)-1]
	if last["type"] != "session_end" {
		t.Errorf("last record = %v, want session_end", last)
	}
	summary, _ := last["summary"].(map[string]any)
	if summary == nil || summary["health"] != "closed" {
		t.Errorf("session_end summary health = %v, want closed", summary["health"])
	}
	if summary != nil {
		stt, _ := summary["stt"].(map[string]any)
		if stt == nil || stt["reconnects_total"] != float64(0) || stt["buffer_drops_total"] != float64(0) {
			t.Errorf("session_end stt counters = %v, want zeros for a clean session", stt)
		}
		src, _ := summary["source"].(map[string]any)
		if src == nil || src["ffmpeg_restarts_total"] != float64(0) {
			t.Errorf("session_end source counters = %v, want zeros", src)
		}
	}
}
