package deepgram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"livecaption/internal/audio"
	"livecaption/internal/metrics"
	"livecaption/internal/stt"
)

func TestDecodeTranscript(t *testing.T) {
	cases := []struct {
		name string
		json string
		want stt.Transcript
		ok   bool
	}{
		{
			// interim_results=false means the wire should never carry this,
			// but decodeTranscript must not trust that and paint it anyway:
			// the IsFinal guard is defense-in-depth for a debug flip.
			name: "is_final false is dropped",
			json: `{"type":"Results","is_final":false,"start":1.5,"duration":0.4,
				"channel":{"alternatives":[{"transcript":"hello there","confidence":0.82}]}}`,
			ok: false,
		},
		{
			name: "final result",
			json: `{"type":"Results","is_final":true,"start":0,"duration":2.1,
				"channel":{"alternatives":[{"transcript":"good morning everyone","confidence":0.95}]}}`,
			want: stt.Transcript{
				Text:  "good morning everyone",
				Start: 0, Duration: 2100 * time.Millisecond, Confidence: 0.95,
			},
			ok: true,
		},
		{
			name: "empty transcript is skipped",
			json: `{"type":"Results","is_final":true,"channel":{"alternatives":[{"transcript":"","confidence":0}]}}`,
			ok:   false,
		},
		{
			name: "no alternatives is skipped",
			json: `{"type":"Results","is_final":true,"channel":{"alternatives":[]}}`,
			ok:   false,
		},
		{
			name: "metadata is ignored",
			json: `{"type":"Metadata","request_id":"abc","duration":5.0}`,
			ok:   false,
		},
		{
			name: "speech started is ignored",
			json: `{"type":"SpeechStarted","channel":[0],"timestamp":0.1}`,
			ok:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := decodeTranscript([]byte(tc.json))
			if err != nil {
				t.Fatalf("decodeTranscript: %v", err)
			}
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if got.Text != tc.want.Text || got.Start != tc.want.Start ||
				got.Duration != tc.want.Duration || got.Confidence != tc.want.Confidence {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestDialURL_DropsInterimsAndUtteranceEnd pins the query string this engine
// asks Deepgram for: no revisable text, no UtteranceEnd/vad_events backstop
// (both require interim_results=true and are dead weight without it),
// endpointing driven from Config, and punctuate/smart_format still on. The
// last two are load-bearing now that the hub closes transcript lines on
// punctuation — this assertion is what stops a future cleanup from silently
// removing them.
func TestDialURL_DropsInterimsAndUtteranceEnd(t *testing.T) {
	eng := testEngine("wss://example.invalid")
	eng.cfg.Endpointing = 150 * time.Millisecond

	u, err := url.Parse(eng.dialURL())
	if err != nil {
		t.Fatalf("dialURL did not produce a valid URL: %v", err)
	}
	q := u.Query()

	if got := q.Get("interim_results"); got != "false" {
		t.Errorf("interim_results = %q, want %q", got, "false")
	}
	if q.Has("utterance_end_ms") {
		t.Error("utterance_end_ms should not be present: it requires interim_results=true")
	}
	if q.Has("vad_events") {
		t.Error("vad_events should not be present: its only signal is discarded by decodeTranscript")
	}
	if got := q.Get("endpointing"); got != "150" {
		t.Errorf("endpointing = %q, want %q (from Config.Endpointing)", got, "150")
	}
	if got := q.Get("punctuate"); got != "true" {
		t.Errorf("punctuate = %q, want %q: it is load-bearing for transcript line breaks now", got, "true")
	}
	if got := q.Get("smart_format"); got != "true" {
		t.Errorf("smart_format = %q, want %q: it is load-bearing for transcript line breaks now", got, "true")
	}
}

// --- test server plumbing ---

// serverConn is what a scripted test handler gets: an accepted connection
// plus the request context to drive reads and writes with.
type serverConn struct {
	c   *websocket.Conn
	ctx context.Context
}

func (s serverConn) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.c.Write(s.ctx, websocket.MessageText, b)
}

// drainBinary reads and discards audio frames until the connection ends or
// the client sends CloseStream, in which case it reports that on done.
func (s serverConn) drainBinary(done chan<- struct{}) {
	for {
		typ, data, err := s.c.Read(s.ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageBinary {
			continue
		}
		var m map[string]string
		if json.Unmarshal(data, &m) == nil && m["type"] == "CloseStream" {
			close(done)
			return
		}
	}
}

func newTestServer(t *testing.T, handle func(serverConn)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		handle(serverConn{c: c, ctx: r.Context()})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testEngine(wsURL string) *Engine {
	return &Engine{
		cfg: stt.Config{
			Format:   audio.PipelineFormat,
			Model:    "nova-3",
			Language: "en-US",
			APIKey:   "test-key",
			Metrics:  metrics.New("test", "session"),
		},
		wsURL: wsURL,
	}
}

// testEngineWithPause is testEngine plus an explicit PauseConfig, for the
// auto-pause tests below. Values are kept small so the tests stay fast.
func testEngineWithPause(wsURL string, pause stt.PauseConfig) *Engine {
	eng := testEngine(wsURL)
	eng.cfg.Pause = pause
	return eng
}

// silentPCM is a buffer RMSDBFS reports as silence (all-zero samples).
func silentPCM(n int) []byte { return make([]byte, n) }

// loudPCM is a buffer RMSDBFS reports as comfortably above any reasonable
// silence threshold: constant-amplitude samples well under full scale.
func loudPCM(n int) []byte {
	pcm := make([]byte, n)
	for i := 0; i+1 < n; i += 2 {
		pcm[i], pcm[i+1] = 0x20, 0x4e // little-endian int16(20000)
	}
	return pcm
}

func resultMsg(text string, isFinal bool, start, dur float64) map[string]any {
	return map[string]any{
		"type": "Results", "is_final": isFinal,
		"start": start, "duration": dur,
		"channel": map[string]any{
			"alternatives": []map[string]any{{"transcript": text, "confidence": 0.9}},
		},
	}
}

// --- session tests ---

func TestEngine_FullSession(t *testing.T) {
	var bytesReceived int64
	closeSeen := make(chan struct{})

	srv := newTestServer(t, func(sc serverConn) {
		go func() {
			for {
				typ, data, err := sc.c.Read(sc.ctx)
				if err != nil {
					return
				}
				if typ == websocket.MessageBinary {
					atomic.AddInt64(&bytesReceived, int64(len(data)))
					continue
				}
				var m map[string]string
				if json.Unmarshal(data, &m) == nil && m["type"] == "CloseStream" {
					close(closeSeen)
					return
				}
			}
		}()

		// A non-final Results should never reach the hub: interim_results is
		// requested false, but decodeTranscript's IsFinal guard drops it
		// defensively regardless.
		sc.send(resultMsg("hello", false, 0, 0.4))
		sc.send(resultMsg("hello world", true, 0, 1.0))

		select {
		case <-closeSeen:
		case <-time.After(2 * time.Second):
		}
	})

	eng := testEngine(srv.URL)

	frames := make(chan audio.Frame)
	out := make(chan stt.Transcript, 16)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx, frames, out) }()

	frames <- audio.Frame{PCM: make([]byte, 3200), Offset: 100 * time.Millisecond, CapturedAt: time.Now()}
	close(frames)

	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	close(out)

	var got []stt.Transcript
	for tr := range out {
		got = append(got, tr)
	}

	if len(got) != 1 {
		t.Fatalf("expected 1 transcript (the non-final must be dropped), got %d: %+v", len(got), got)
	}
	if got[0].Text != "hello world" {
		t.Errorf("final segment wrong: %+v", got[0])
	}
	if atomic.LoadInt64(&bytesReceived) == 0 {
		t.Error("server never received audio")
	}
}

func TestEngine_Reconnect(t *testing.T) {
	var connNum int32

	srv := newTestServer(t, func(sc serverConn) {
		n := atomic.AddInt32(&connNum, 1)
		if n == 1 {
			// Simulate a mid-stream drop: one result, then the connection
			// dies without a close handshake.
			sc.send(resultMsg("first connection", true, 0, 0.3))
			time.Sleep(50 * time.Millisecond)
			sc.c.CloseNow()
			return
		}

		done := make(chan struct{})
		go sc.drainBinary(done)
		sc.send(resultMsg("second connection", true, 0, 1.0))
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	})

	eng := testEngine(srv.URL)

	frames := make(chan audio.Frame)
	out := make(chan stt.Transcript, 16)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx, frames, out) }()

	// Keep a trickle of audio flowing across the reconnect.
	stopFeeding := make(chan struct{})
	go func() {
		for {
			select {
			case frames <- audio.Frame{PCM: make([]byte, 320), Offset: time.Millisecond}:
				time.Sleep(10 * time.Millisecond)
			case <-stopFeeding:
				return
			}
		}
	}()

	var got []stt.Transcript
	timeout := time.After(5 * time.Second)
loop:
	for len(got) < 2 {
		select {
		case tr := <-out:
			got = append(got, tr)
		case <-timeout:
			t.Fatal("timed out waiting for transcripts across reconnect")
		case <-done:
			break loop
		}
	}
	// Cancel first so the engine's internal frame reader stops pulling from
	// frames before the feeder goroutine is told to stop; otherwise the
	// feeder can be left trying to send with nothing left reading.
	cancel()
	close(stopFeeding)
	<-done

	if len(got) < 2 {
		t.Fatalf("expected transcripts from both connections, got %+v", got)
	}
	if got[0].Text != "first connection" {
		t.Errorf("first connection text = %q", got[0].Text)
	}
	if got[1].Text != "second connection" {
		t.Errorf("second connection transcript wrong: %+v", got[1])
	}
	if eng.cfg.Metrics.Snapshot().STT.Reconnects == 0 {
		t.Error("expected STTReconnect to be counted")
	}
}

// TestEngine_DropOldestWhenDisconnected verifies that pushing audio while the
// connection is down (or never comes up) never blocks the caller, and that
// once the ring is full it drops the oldest frame and counts it.
func TestEngine_DropOldestWhenDisconnected(t *testing.T) {
	// Point at a server that never completes the handshake, so the engine
	// stays in its connect/backoff loop for the duration of the test.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	eng := testEngine(srv.URL)

	frames := make(chan audio.Frame)
	out := make(chan stt.Transcript, 16)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx, frames, out) }()

	// 2s of buffer at 16kHz mono s16 is 64000 bytes; push well past that
	// without ever being drained, and never let the send block.
	chunk := make([]byte, 3200) // 100ms
	for i := 0; i < 40; i++ {
		select {
		case frames <- audio.Frame{PCM: chunk, Offset: time.Duration(i) * 100 * time.Millisecond}:
		case <-time.After(500 * time.Millisecond):
			t.Fatal("send on frames blocked while disconnected")
		}
	}
	close(frames)
	cancel()
	<-done

	// testEngine leaves Pause at its zero value (disabled), so the gate is
	// always Active and these evictions count as STTBufferDrop, not the
	// unrelated source-side DropFrame counter.
	if got := eng.cfg.Metrics.Snapshot().STT.BufferDrops; got == 0 {
		t.Error("expected STTBufferDrop to be counted while disconnected")
	}
}

// TestEngine_AutoPauseReconnectsOnResume covers (a) and (b) from the design:
// silence past Hold makes the server observe its connection closing (a clean
// CloseStream, not a drop), a subsequent loud frame causes a new dial, and
// the pause is counted without being mistaken for a reconnect.
func TestEngine_AutoPauseReconnectsOnResume(t *testing.T) {
	var connNum int32
	pauseCloseSeen := make(chan struct{})
	secondDialSeen := make(chan struct{})

	srv := newTestServer(t, func(sc serverConn) {
		n := atomic.AddInt32(&connNum, 1)
		done := make(chan struct{})
		go sc.drainBinary(done)
		if n == 2 {
			close(secondDialSeen)
		}
		select {
		case <-done:
			if n == 1 {
				close(pauseCloseSeen)
			}
		case <-time.After(3 * time.Second):
		}
	})

	eng := testEngineWithPause(srv.URL, stt.PauseConfig{
		Enabled: true, ThresholdDB: -30, Hold: 150 * time.Millisecond,
	})

	frames := make(chan audio.Frame)
	out := make(chan stt.Transcript, 16)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx, frames, out) }()

	var (
		mu     sync.Mutex
		pcm    = silentPCM(320)
		offset time.Duration
	)
	setPCM := func(p []byte) {
		mu.Lock()
		pcm = p
		mu.Unlock()
	}

	stopFeeding := make(chan struct{})
	go func() {
		for {
			mu.Lock()
			p := pcm
			mu.Unlock()
			select {
			case frames <- audio.Frame{PCM: p, Offset: offset}:
				offset += 20 * time.Millisecond
			case <-stopFeeding:
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	select {
	case <-pauseCloseSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the paused connection to close cleanly")
	}

	setPCM(loudPCM(320)) // resume: one loud frame should redial almost instantly

	select {
	case <-secondDialSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the resume redial")
	}

	cancel()
	close(stopFeeding)
	<-done

	if got := atomic.LoadInt32(&connNum); got < 2 {
		t.Fatalf("expected at least 2 connections (initial + resume redial), got %d", got)
	}

	snap := eng.cfg.Metrics.Snapshot()
	if snap.STT.Pauses == 0 {
		t.Error("expected pauses_total to be counted")
	}
	if snap.STT.Reconnects != 0 {
		t.Errorf("a pause/resume cycle must not count as a reconnect, got %d", snap.STT.Reconnects)
	}
}

// TestEngine_AutoPauseDisabledNeverCloses covers (c): with Enabled: false a
// silent stream must never trigger the auto-pause CloseStream, no matter how
// long the silence runs.
func TestEngine_AutoPauseDisabledNeverCloses(t *testing.T) {
	var connNum int32
	closeSeen := make(chan struct{})

	srv := newTestServer(t, func(sc serverConn) {
		atomic.AddInt32(&connNum, 1)
		done := make(chan struct{})
		go sc.drainBinary(done)
		select {
		case <-done:
			close(closeSeen)
		case <-time.After(3 * time.Second):
		}
	})

	eng := testEngineWithPause(srv.URL, stt.PauseConfig{
		Enabled: false, ThresholdDB: -30, Hold: 50 * time.Millisecond,
	})

	frames := make(chan audio.Frame)
	out := make(chan stt.Transcript, 16)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx, frames, out) }()

	silent := silentPCM(320)
	stopFeeding := make(chan struct{})
	go func() {
		var offset time.Duration
		for {
			select {
			case frames <- audio.Frame{PCM: silent, Offset: offset}:
				offset += 20 * time.Millisecond
			case <-stopFeeding:
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	// Well past what Hold would trip if auto-pause were enabled.
	time.Sleep(400 * time.Millisecond)
	select {
	case <-closeSeen:
		t.Fatal("connection closed even though auto-pause is disabled")
	default:
	}

	cancel()
	close(stopFeeding)
	<-done

	if got := atomic.LoadInt32(&connNum); got != 1 {
		t.Errorf("expected exactly one connection with auto-pause disabled, got %d", got)
	}
	if snap := eng.cfg.Metrics.Snapshot(); snap.STT.Pauses != 0 {
		t.Errorf("expected no pauses counted when disabled, got %d", snap.STT.Pauses)
	}
}

func TestEngine_AuthFailureFailsFast(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	eng := testEngine(srv.URL)

	frames := make(chan audio.Frame)
	out := make(chan stt.Transcript, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- eng.Run(ctx, frames, out) }()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected an error for a 401 on first connect")
		}
		if !strings.Contains(err.Error(), "DEEPGRAM_API_KEY") {
			t.Errorf("error should mention DEEPGRAM_API_KEY, got: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("auth failure did not return promptly")
	}
	close(frames)
}

// --- ring / anchor integration tests ---

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

// TestEngine_TranscriptCapturedAt checks that a real Run() round trip stamps
// CapturedAt on a final transcript from the frame's own capture time, not
// from receipt time, with the delta between them reflecting genuine
// processing/network delay.
func TestEngine_TranscriptCapturedAt(t *testing.T) {
	closeSeen := make(chan struct{})

	srv := newTestServer(t, func(sc serverConn) {
		done := make(chan struct{})
		go sc.drainBinary(done)
		sc.send(resultMsg("hello", true, 0, 0.1))
		select {
		case <-done:
			close(closeSeen)
		case <-time.After(2 * time.Second):
		}
	})

	eng := testEngine(srv.URL)

	frames := make(chan audio.Frame)
	out := make(chan stt.Transcript, 16)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx, frames, out) }()

	// A 100ms (3200-byte) frame stamped 500ms in the past: the final below
	// covers start:0 duration:0.1, i.e. exactly this one frame, so its
	// CapturedAt should resolve to this timestamp.
	capturedAt := time.Now().Add(-500 * time.Millisecond)
	frames <- audio.Frame{PCM: make([]byte, 3200), Offset: 0, CapturedAt: capturedAt}
	close(frames)

	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	close(out)

	var got stt.Transcript
	found := false
	for tr := range out {
		if tr.Text == "hello" {
			got, found = tr, true
		}
	}
	if !found {
		t.Fatal("no final transcript received")
	}
	if got.CapturedAt.IsZero() {
		t.Fatal("CapturedAt was not stamped")
	}

	delta := got.ReceivedAt.Sub(got.CapturedAt)
	if delta < 350*time.Millisecond || delta > 2*time.Second {
		t.Errorf("ReceivedAt.Sub(CapturedAt) = %v, want ~500ms (generous slack)", delta)
	}
}

// TestEngine_S1ClockRestartAfterReconnect guards the actual S1 bug: Deepgram
// restarts its media clock at 0 on every fresh WebSocket, so any latency
// calculation anchored to an origin that does not also reset per connection
// drifts by roughly however much real wall-clock time has passed since that
// origin was recorded — unboundedly, as pauses accumulate over a long
// session. The fix is that runConnection builds a brand new anchorIndex per
// connection (see anchor.go / runConnection), so connection 2's start:0 can
// only ever resolve against connection 2's own, freshly written bytes.
//
// This test forces a real ~1.5s wall-clock gap between connection 1's
// activity and connection 2's resume, then floods the ring with fresh
// silence past its 2s capacity before resuming so any leftover pre-pause
// bytes are evicted — connection 2's pre-roll is genuinely recent, not an
// artifact of this test's own setup. That isolates the one thing under
// test: whether "byte 0" on connection 2 resolves against ITS OWN recent
// write or against some earlier, unrelated origin.
//
// This test must fail on code that does not scope the anchor index to one
// connection (e.g. a single index built once in Run and reused across
// reconnects, its "written" byte cursor never reset): connection 2's
// start:0 would then resolve against connection 1's cumulative byte
// position, landing back on connection 1's real (pre-sleep) timestamps and
// pushing the observed delta past the ~1s bound asserted below. Sanity
// check: on the fixed code the delta is small because connection 2's own
// first bytes were written moments before the final arrives; on the buggy
// code the delta tracks the real 1.5s sleep, which comfortably clears the
// 1s bound either way this test is run, so it cannot pass by accident.
func TestEngine_S1ClockRestartAfterReconnect(t *testing.T) {
	var connNum int32
	pauseCloseSeen := make(chan struct{})
	finalSent := make(chan struct{})

	srv := newTestServer(t, func(sc serverConn) {
		n := atomic.AddInt32(&connNum, 1)

		if n == 1 {
			done := make(chan struct{})
			go sc.drainBinary(done)
			select {
			case <-done:
				close(pauseCloseSeen)
			case <-time.After(3 * time.Second):
			}
			return
		}

		// Connection 2: wait for at least one binary chunk so the engine's
		// anchor index has something of its own to resolve start:0 against
		// before the server reports it, then script the final that mirrors
		// what a real Deepgram clock restart looks like.
		done := make(chan struct{})
		firstBinary := make(chan struct{})
		go func() {
			seenFirst := false
			for {
				typ, data, err := sc.c.Read(sc.ctx)
				if err != nil {
					return
				}
				if typ == websocket.MessageBinary {
					if !seenFirst {
						seenFirst = true
						close(firstBinary)
					}
					continue
				}
				var m map[string]string
				if json.Unmarshal(data, &m) == nil && m["type"] == "CloseStream" {
					close(done)
					return
				}
			}
		}()

		select {
		case <-firstBinary:
		case <-time.After(2 * time.Second):
		}
		sc.send(resultMsg("resumed", true, 0, 0.02))
		close(finalSent)

		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	})

	eng := testEngineWithPause(srv.URL, stt.PauseConfig{
		Enabled: true, ThresholdDB: -30, Hold: 15 * time.Millisecond,
	})

	frames := make(chan audio.Frame)
	out := make(chan stt.Transcript, 16)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx, frames, out) }()

	var offset time.Duration
	send := func(pcm []byte) {
		select {
		case frames <- audio.Frame{PCM: pcm, Offset: offset, CapturedAt: time.Now()}:
			offset += 20 * time.Millisecond
		case <-time.After(2 * time.Second):
			t.Fatal("send on frames blocked")
		}
	}

	// Open connection 1 with a little loud audio, then push it silent long
	// enough (past Hold) to trigger the pause. Real sleeps between sends
	// give the dial and writeLoop time to actually write these bytes before
	// the pause fires — otherwise gate can go inactive (from the fast,
	// back-to-back pushes below) before connection 1 ever gets to write
	// anything, which would make this test pass even against a shared,
	// never-reset anchor index for the wrong reason: connection 1
	// contributing zero entries either way.
	for i := 0; i < 3; i++ {
		send(loudPCM(320))
		time.Sleep(10 * time.Millisecond)
	}
	for i := 0; i < 3; i++ {
		send(silentPCM(320))
		time.Sleep(10 * time.Millisecond)
	}

	select {
	case <-pauseCloseSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the paused connection to close cleanly")
	}

	// A real wall-clock gap stands in for a long stretch of session
	// silence: enough that connection 1's early bytes, written before this
	// sleep, would read as stale (>1s old) if they ever leaked into
	// connection 2's start:0 lookup — which is exactly the S1 bug were the
	// anchor index not scoped per connection.
	time.Sleep(1500 * time.Millisecond)

	// Flood the ring with fresh (post-sleep) silence, well past its 2s
	// capacity, so any leftover pre-pause bytes are evicted: connection 2's
	// pre-roll is guaranteed genuinely recent, not an artifact of this
	// test's own setup racing the pause.
	for i := 0; i < 260; i++ {
		send(silentPCM(320))
	}

	// Resume with loud, freshly captured audio.
	for i := 0; i < 5; i++ {
		send(loudPCM(320))
	}

	select {
	case <-finalSent:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for connection 2's final")
	}

	var got stt.Transcript
	found := false
loop:
	for {
		select {
		case tr := <-out:
			if tr.Text == "resumed" {
				got, found = tr, true
				break loop
			}
		case <-time.After(3 * time.Second):
			break loop
		}
	}

	cancel()
	<-done

	if !found {
		t.Fatal("did not receive the post-resume final transcript")
	}
	if got.CapturedAt.IsZero() {
		t.Fatal("CapturedAt was not stamped on the post-resume final")
	}
	delta := got.ReceivedAt.Sub(got.CapturedAt)
	if delta < 0 || delta > time.Second {
		t.Errorf("ReceivedAt.Sub(CapturedAt) = %v, want < ~1s; a value tracking the real "+
			"~1.5s gap before resume means connection 2 read connection 1's stale anchor "+
			"instead of its own", delta)
	}
	if got.SentAt.IsZero() {
		t.Fatal("SentAt was not stamped on the post-resume final")
	}
	// SentAt must fall between the chunk's CapturedAt and the transcript's
	// ReceivedAt: it marks the moment between those two that the audio was
	// handed to the socket, so it can only order between them.
	if got.SentAt.Before(got.CapturedAt) {
		t.Errorf("SentAt = %v is before CapturedAt = %v, want SentAt >= CapturedAt", got.SentAt, got.CapturedAt)
	}
	if got.SentAt.After(got.ReceivedAt) {
		t.Errorf("SentAt = %v is after ReceivedAt = %v, want SentAt <= ReceivedAt", got.SentAt, got.ReceivedAt)
	}
}
