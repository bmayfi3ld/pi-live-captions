package stt

// Shared-session tests: a scripted WebSocket server plus a scripted Session
// drive RunSession's read path end to end — real dialer, real ring, real
// anchor index — so the source-time normalization in readLoop is tested on
// the same code path every provider ships, without any provider's protocol.
//
// The one contract these tests lean on: writeLoop records an anchor BEFORE
// SendAudio writes the bytes, so once the server has READ n bytes the client
// has anchored n bytes. A trigger message the server sends only after
// reading a given byte count can therefore script provider media positions
// that are guaranteed resolvable.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"livecaption/internal/audio"
	"livecaption/internal/metrics"
)

// --- plumbing ---

// serverConn is what a scripted handler gets: the accepted connection plus
// the request context to drive reads and writes with.
type serverConn struct {
	c   *websocket.Conn
	ctx context.Context
}

// readAudio reads until at least want bytes of binary audio have arrived,
// the client signals EndOfStream, or the read fails. Returns the binary byte
// count and whether EndOfStream was seen.
func readAudio(sc serverConn, want int) (int, bool) {
	n := 0
	for n < want {
		typ, data, err := sc.c.Read(sc.ctx)
		if err != nil {
			return n, false
		}
		switch typ {
		case websocket.MessageBinary:
			n += len(data)
		case websocket.MessageText:
			if string(data) == "EndOfStream" {
				return n, true
			}
		}
	}
	return n, false
}

// trigger writes the message whose arrival makes the client's readLoop call
// Decode once.
func trigger(sc serverConn) {
	_ = sc.c.Write(sc.ctx, websocket.MessageText, []byte("go"))
}

// closeAfter gives the client's readLoop a moment to consume what was
// written, then closes the connection so its Read returns promptly.
func closeAfter(sc serverConn, d time.Duration) {
	time.Sleep(d)
	sc.c.CloseNow()
}

// scriptMsg is what one server message decodes to.
type scriptMsg struct {
	ts   []Transcript
	edge *MusicEdge
}

// scriptedSession is a Session that counts the bytes it was handed (its
// provider media clock, in effect) and answers Decode from a queue the
// test fills before writing the trigger message.
type scriptedSession struct {
	conn *websocket.Conn

	mu     sync.Mutex
	script []scriptMsg
}

func (s *scriptedSession) enqueue(m scriptMsg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.script = append(s.script, m)
}

func (s *scriptedSession) SendAudio(ctx context.Context, pcm []byte) error {
	return s.conn.Write(ctx, websocket.MessageBinary, pcm)
}

func (s *scriptedSession) Idle(context.Context) error { return nil }

// Finish writes a text message the server can wait on, mirroring the real
// providers' end-of-stream so the graceful drain is observable.
func (s *scriptedSession) Finish(ctx context.Context) error {
	return s.conn.Write(ctx, websocket.MessageText, []byte("EndOfStream"))
}

func (s *scriptedSession) Decode(_ []byte) ([]Transcript, *MusicEdge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.script) == 0 {
		return nil, nil, nil
	}
	m := s.script[0]
	s.script = s.script[1:]
	return m.ts, m.edge, nil
}

// newScriptedServer starts a fake recognizer server. Each accepted
// connection is paired, in order, with the scriptedSession the client's
// dialer built for it — RunSession only ever has one dial in flight, so
// pairing by handshake order is exact. onAccept scripts that connection.
func newScriptedServer(t *testing.T, onAccept func(sc serverConn, s *scriptedSession)) (*httptest.Server, chan *scriptedSession) {
	t.Helper()
	sessions := make(chan *scriptedSession, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		var s *scriptedSession
		select {
		case s = <-sessions:
		case <-r.Context().Done():
			return
		}
		onAccept(serverConn{c: c, ctx: r.Context()}, s)
	}))
	t.Cleanup(srv.Close)
	return srv, sessions
}

// dialer builds the Dialer for RunSession: it dials the fake server and
// hands the fresh scriptedSession to both the driver and, through the
// channel, the server handler that will script it.
func dialer(srv *httptest.Server, sessions chan *scriptedSession) Dialer {
	return func(ctx context.Context) (*websocket.Conn, Session, error) {
		conn, _, err := websocket.Dial(ctx, srv.URL, nil)
		if err != nil {
			return nil, nil, err
		}
		s := &scriptedSession{conn: conn}
		select {
		case sessions <- s:
		default: // unreachable: one dial in flight, buffer of four
		}
		return conn, s, nil
	}
}

// feeder pushes frames of pcm every interval of wall time, starting the
// source clock at base and advancing it by step per frame, until stop closes
// or ctx ends. It is the tests' stand-in for an audio source: offsets are
// the source clock and keep advancing whether or not a connection is up.
type feeder struct {
	frames chan<- audio.Frame
	stop   chan struct{}
}

func startFeeder(ctx context.Context, frames chan audio.Frame, pcm []byte, base, step, interval time.Duration, capturedAt func(time.Duration) time.Time) *feeder {
	f := &feeder{frames: frames, stop: make(chan struct{})}
	go func() {
		var offset time.Duration
		for {
			select {
			case frames <- audio.Frame{PCM: pcm, Offset: base + offset + step, CapturedAt: capturedAt(base + offset + step)}:
				offset += step
			case <-f.stop:
				return
			case <-ctx.Done():
				return
			}
			time.Sleep(interval)
		}
	}()
	return f
}

func (f *feeder) halt() { close(f.stop) }

// loudPCM is a buffer RMSDBFS reports well above the silence threshold.
func loudPCM(n int) []byte {
	pcm := make([]byte, n)
	for i := 0; i+1 < n; i += 2 {
		pcm[i], pcm[i+1] = 0x20, 0x4e // little-endian int16(20000)
	}
	return pcm
}

// collect gathers n transcripts from out or fails the test on timeout.
func collect(t *testing.T, out <-chan Transcript, n int) []Transcript {
	t.Helper()
	var got []Transcript
	deadline := time.After(5 * time.Second)
	for len(got) < n {
		select {
		case tr := <-out:
			got = append(got, tr)
		case <-deadline:
			t.Fatalf("timed out after %d of %d transcripts: %+v", len(got), n, got)
		}
	}
	return got
}

// ms and sec keep the assertions below readable.
func ms(n int) time.Duration  { return time.Duration(n) * time.Millisecond }
func sec(n int) time.Duration { return time.Duration(n) * time.Second }

// --- source-time normalization in the shared read path ---

// TestRunSession_SourceTimePreRollAndLatencyOrder pins the read path's
// ordering contract and its per-word mapping on one connection whose audio
// was pre-rolled deep into the source stream (the frames' offsets start at
// 10s, as a replacement connection's ring contents do):
//
//   - every timed word maps individually onto the source clock, and the
//     transcript's own bounds are re-derived from the mapped words;
//   - an untimed transcript maps as a unit, without dropping its text;
//   - a music edge goes through the same anchor before OnMusic sees it;
//   - latency (CapturedAt/SentAt) is anchored on the ORIGINAL provider end:
//     normalizing the timing first would make End() address a source
//     position this connection never wrote, clamping CapturedAt to the
//     newest frame instead of the frame the audio actually arrived in.
func TestRunSession_SourceTimePreRollAndLatencyOrder(t *testing.T) {
	const base = 10 * time.Second
	t0 := time.Now()

	srv, sessions := newScriptedServer(t, func(sc serverConn, s *scriptedSession) {
		// Trigger 1 after 500ms of audio: words at provider [0.1,0.2) and
		// [0.3,0.4), which map to source [10.1,10.2) and [10.3,10.4).
		readAudio(sc, 16000)
		s.enqueue(scriptMsg{ts: []Transcript{{
			Words: []Word{
				{Text: "one", Start: ms(100), End: ms(200)},
				{Text: "two", Start: ms(300), End: ms(400)},
			},
			Start:    ms(100),
			Duration: ms(300),
		}}})
		trigger(sc)
		// Triggers 2 and 3 after a second of audio: a music edge at provider
		// 0.45 (source 10.45), then an untimed transcript spanning provider
		// [0.5,0.7) (source [10.5,10.7)).
		readAudio(sc, 32000)
		s.enqueue(scriptMsg{edge: &MusicEdge{Active: true, At: ms(450)}})
		trigger(sc)
		s.enqueue(scriptMsg{ts: []Transcript{{
			Words:    Untimed("no timing"),
			Start:    ms(500),
			Duration: ms(200),
		}}})
		trigger(sc)
		closeAfter(sc, 150*time.Millisecond)
	})

	var musicCalls []time.Duration
	var mu sync.Mutex
	cfg := Config{
		Format:  audio.PipelineFormat,
		Metrics: metrics.New("test", "session"),
		OnMusic: func(active bool, at time.Duration) {
			mu.Lock()
			musicCalls = append(musicCalls, at)
			mu.Unlock()
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	frames := make(chan audio.Frame)
	out := make(chan Transcript, 16)
	done := make(chan error, 1)
	go func() { done <- RunSession(ctx, cfg, "scripted", dialer(srv, sessions), frames, out) }()

	f := startFeeder(ctx, frames, make([]byte, 3200), base, ms(100), 10*time.Millisecond,
		func(offset time.Duration) time.Time { return t0.Add(offset) })
	got := collect(t, out, 2)
	f.halt()
	cancel()
	<-done

	if len(got) != 2 {
		t.Fatalf("transcripts = %d, want 2", len(got))
	}
	first := got[0]
	if first.Text() != "one two" {
		t.Errorf("text = %q, want %q", first.Text(), "one two")
	}
	wantWords := []struct {
		start, end time.Duration
	}{{base + ms(100), base + ms(200)}, {base + ms(300), base + ms(400)}}
	for i, w := range wantWords {
		if first.Words[i].Start != w.start || first.Words[i].End != w.end {
			t.Errorf("word %d timing = [%v,%v), want [%v,%v) on the source clock",
				i, first.Words[i].Start, first.Words[i].End, w.start, w.end)
		}
	}
	if first.Start != base+ms(100) || first.End() != base+ms(400) {
		t.Errorf("bounds = [%v,%v), want re-derived [10.1s,10.4s) from the mapped words", first.Start, first.End())
	}

	// The latency anchor: the transcript's provider end is 400ms, the last
	// sample of the 4th frame, whose CapturedAt the test pinned to
	// t0+10.4s. Any other value — in particular the newest frame's stamp,
	// which a normalize-first implementation would clamp to — is wrong.
	if want := t0.Add(base + ms(400)); !first.CapturedAt.Equal(want) {
		t.Errorf("CapturedAt = %v, want %v (the frame the provider end covers, not the newest)", first.CapturedAt, want)
	}
	if first.SentAt.IsZero() || first.SentAt.After(first.ReceivedAt) {
		t.Errorf("SentAt = %v, want the socket write instant at or before ReceivedAt %v", first.SentAt, first.ReceivedAt)
	}

	second := got[1]
	if second.Text() != "no timing" {
		t.Errorf("untimed text = %q, want it preserved verbatim", second.Text())
	}
	if second.Start != base+ms(500) || second.Duration != ms(200) {
		t.Errorf("untimed timing = [%v,+%v), want the unit-mapped [10.5s,+200ms)", second.Start, second.Duration)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(musicCalls) != 1 || musicCalls[0] != base+ms(450) {
		t.Errorf("OnMusic calls = %v, want exactly one at source 10.45s", musicCalls)
	}
}

// TestRunSession_SourceTimeAcrossReconnect drives two connections in one
// session: the first drops mid-stream, the second picks up the ring's
// pre-roll. The second connection's provider time starts at zero like any
// fresh recognizer clock, but its published timing must land after the first
// connection's on the source clock — nondecreasing, never restarted at
// zero. Also pins that the connection-end hook fires between the two: it
// must have fired before the replacement's results can arrive.
func TestRunSession_SourceTimeAcrossReconnect(t *testing.T) {
	var connNum atomic.Int32
	var hookCalls atomic.Int32
	var eventMu sync.Mutex
	var events []string

	srv, sessions := newScriptedServer(t, func(sc serverConn, s *scriptedSession) {
		n := connNum.Add(1)
		switch n {
		case 1:
			readAudio(sc, 9600) // 300ms of audio
			s.enqueue(scriptMsg{ts: []Transcript{{
				Words:    []Word{{Text: "first", Start: 0, End: ms(300)}},
				Start:    0,
				Duration: ms(300),
			}}})
			trigger(sc)
			closeAfter(sc, 150*time.Millisecond) // drop without a close handshake
		default:
			readAudio(sc, 6400) // 200ms of pre-roll plus fresh audio
			s.enqueue(scriptMsg{ts: []Transcript{{
				Words:    []Word{{Text: "second", Start: 0, End: ms(200)}},
				Start:    0,
				Duration: ms(200),
			}}})
			trigger(sc)
			closeAfter(sc, 150*time.Millisecond)
		}
	})

	cfg := Config{
		Format:  audio.PipelineFormat,
		Metrics: metrics.New("test", "session"),
		OnTranscript: func(tr Transcript) {
			eventMu.Lock()
			events = append(events, tr.Text())
			eventMu.Unlock()
		},
		OnConnectionEnd: func() {
			hookCalls.Add(1)
			eventMu.Lock()
			events = append(events, "reset")
			eventMu.Unlock()
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	frames := make(chan audio.Frame)
	out := make(chan Transcript, 16)
	done := make(chan error, 1)
	go func() { done <- RunSession(ctx, cfg, "scripted", dialer(srv, sessions), frames, out) }()

	// Frames keep flowing across the drop — the ring holds them, and their
	// offsets keep advancing: the source clock does not care that the
	// connection died.
	f := startFeeder(ctx, frames, make([]byte, 3200), 0, ms(100), 25*time.Millisecond,
		func(time.Duration) time.Time { return time.Now() })
	got := collect(t, out, 2)
	f.halt()
	cancel()
	<-done

	if len(got) != 2 {
		t.Fatalf("transcripts = %d, want 2", len(got))
	}
	if got[0].Text() != "first" || got[1].Text() != "second" {
		t.Fatalf("transcripts = %q, %q", got[0].Text(), got[1].Text())
	}
	if got[0].Start != 0 || got[0].End() != ms(300) {
		t.Errorf("first = [%v,%v), want [0,300ms) of source time", got[0].Start, got[0].End())
	}
	if got[1].Start < got[0].End() {
		t.Errorf("second connection restarted the clock: Start %v < first End %v", got[1].Start, got[0].End())
	}
	if got[1].Duration != ms(200) {
		t.Errorf("second Duration = %v, want 200ms (the word's own span, translated)", got[1].Duration)
	}
	if got[0].CapturedAt.IsZero() || got[1].CapturedAt.IsZero() {
		t.Errorf("latency anchoring lost across the reconnect: %+v %+v", got[0], got[1])
	}
	// The second transcript proves the replacement connection was dialed,
	// which the run loop only does after the connection-end hook — so the
	// hook must already have fired exactly once for connection 1.
	if n := hookCalls.Load(); n < 1 {
		t.Errorf("OnConnectionEnd fired %d times before the replacement's transcript arrived, want >= 1", n)
	}
	eventMu.Lock()
	defer eventMu.Unlock()
	if len(events) < 3 || events[0] != "first" || events[1] != "reset" || events[2] != "second" {
		t.Errorf("callback order = %v, want first transcript, reset, replacement transcript", events)
	}
}

func TestSourceTime_UnresolvedWordsAreExplicitlyUntimed(t *testing.T) {
	idx := newAnchorIndex(audio.PipelineFormat)
	idx.Add(3200, time.Now(), time.Now(), 10*time.Second+100*time.Millisecond)
	tr := sourceTime(Transcript{
		Words: []Word{
			{Text: "mapped", Start: 0, End: 50 * time.Millisecond},
			{Text: "stale", Start: -time.Second, End: -500 * time.Millisecond},
		},
		Start: -time.Second, Duration: 1050 * time.Millisecond,
	}, idx)
	if tr.Words[0].Start != 10*time.Second || tr.Words[0].End != 10*time.Second+50*time.Millisecond {
		t.Errorf("mapped word = [%v,%v)", tr.Words[0].Start, tr.Words[0].End)
	}
	if tr.Words[1].Start != 0 || tr.Words[1].End != 0 {
		t.Errorf("unresolved word retained provider timing: [%v,%v)", tr.Words[1].Start, tr.Words[1].End)
	}
}

// TestRunSession_SourceGapWithinOneResult feeds one connection frames whose
// source offsets jump — the shape discarded ring audio or a resumed
// connection's pre-roll produces — and scripts a single provider result
// straddling the jump. The words before and after the jump must land on
// their own source positions, and the transcript's span must contain the
// gap rather than compress it onto the provider's contiguous timeline.
func TestRunSession_SourceGapWithinOneResult(t *testing.T) {
	srv, sessions := newScriptedServer(t, func(sc serverConn, s *scriptedSession) {
		readAudio(sc, 32000) // all ten frames below
		s.enqueue(scriptMsg{ts: []Transcript{{
			Words: []Word{
				{Text: "before", Start: ms(200), End: ms(300)},
				{Text: "after", Start: ms(600), End: ms(700)},
			},
			Start:    ms(200),
			Duration: ms(500),
		}}})
		trigger(sc)
		closeAfter(sc, 150*time.Millisecond)
	})

	cfg := Config{Format: audio.PipelineFormat, Metrics: metrics.New("test", "session")}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	frames := make(chan audio.Frame)
	out := make(chan Transcript, 16)
	done := make(chan error, 1)
	go func() { done <- RunSession(ctx, cfg, "scripted", dialer(srv, sessions), frames, out) }()

	// Frames 1-5 cover source [0,500ms); frames 6-10 pick up at source
	// [19.9s,20.4s]. Provider bytes stay contiguous across the jump.
	offsets := []time.Duration{ms(100), ms(200), ms(300), ms(400), ms(500),
		sec(20), sec(20) + ms(100), sec(20) + ms(200), sec(20) + ms(300), sec(20) + ms(400)}
	pcm := make([]byte, 3200)
	for _, off := range offsets {
		frames <- audio.Frame{PCM: pcm, Offset: off, CapturedAt: time.Now()}
	}
	close(frames)

	got := collect(t, out, 1)
	cancel()
	<-done

	tr := got[0]
	if tr.Text() != "before after" {
		t.Fatalf("text = %q", tr.Text())
	}
	if tr.Words[0].Start != ms(200) || tr.Words[0].End != ms(300) {
		t.Errorf("word before the gap = [%v,%v), want [200ms,300ms) of source time", tr.Words[0].Start, tr.Words[0].End)
	}
	if tr.Words[1].Start < sec(19) {
		t.Errorf("word after the gap = [%v,%v), want source positions past the 19.4s jump, not compressed onto the provider timeline", tr.Words[1].Start, tr.Words[1].End)
	}
	if tr.Duration < 19*time.Second {
		t.Errorf("Duration = %v, want the ~19.9s the source gap spans, not the provider-local 500ms", tr.Duration)
	}
}

// TestRunSession_ConnectionEndHookOnPause drives the auto-pause path: the
// gate goes silent, the connection hangs up gracefully (draining one
// trailing transcript through the real Finish handshake), the hook fires
// while parked, and speech resumes on a fresh connection whose timing
// continues the source clock. A pause must not count as a reconnect.
func TestRunSession_ConnectionEndHookOnPause(t *testing.T) {
	var connNum atomic.Int32
	hookFired := make(chan struct{}, 4)

	srv, sessions := newScriptedServer(t, func(sc serverConn, s *scriptedSession) {
		n := connNum.Add(1)
		switch n {
		case 1:
			// Feed happens to be loud at first: wait for 200ms of audio,
			// publish one transcript, then wait out the pause. The graceful
			// hangup writes EndOfStream (scriptedSession.Finish); the
			// trailing transcript rides the drain, then the server closes.
			if _, eos := readAudio(sc, 6400); eos {
				return
			}
			s.enqueue(scriptMsg{ts: []Transcript{{
				Words:    []Word{{Text: "trailing.", Start: 0, End: ms(100)}},
				Start:    0,
				Duration: ms(100),
			}}})
			trigger(sc)
			if _, eos := readAudio(sc, 1<<30); eos {
				// The pause hangup arrived: hand back one trailing final,
				// then let the read loop see the connection end.
				s.enqueue(scriptMsg{ts: []Transcript{{
					Words:    []Word{{Text: "drained.", Start: ms(100), End: ms(200)}},
					Start:    ms(100),
					Duration: ms(100),
				}}})
				trigger(sc)
				closeAfter(sc, 100*time.Millisecond)
			}
		default:
			readAudio(sc, 3200)
			s.enqueue(scriptMsg{ts: []Transcript{{
				Words:    []Word{{Text: "resumed.", Start: 0, End: ms(100)}},
				Start:    0,
				Duration: ms(100),
			}}})
			trigger(sc)
			closeAfter(sc, 150*time.Millisecond)
		}
	})

	met := metrics.New("test", "session")
	cfg := Config{
		Format:  audio.PipelineFormat,
		Metrics: met,
		Pause:   PauseConfig{Enabled: true, Hold: 200 * time.Millisecond},
		OnConnectionEnd: func() {
			select {
			case hookFired <- struct{}{}:
			default:
			}
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	frames := make(chan audio.Frame)
	out := make(chan Transcript, 16)
	done := make(chan error, 1)
	go func() { done <- RunSession(ctx, cfg, "scripted", dialer(srv, sessions), frames, out) }()

	// One feeder with switchable PCM and a monotonically advancing source
	// clock — silence must arrive to trip the pause BEFORE the hook can
	// fire, and a real source's offsets never run backwards between batches.
	var (
		fmu  sync.Mutex
		pcm  = loudPCM(3200)
		stop = make(chan struct{})
	)
	setPCM := func(p []byte) {
		fmu.Lock()
		pcm = p
		fmu.Unlock()
	}
	go func() {
		var offset time.Duration
		for {
			fmu.Lock()
			p := pcm
			fmu.Unlock()
			select {
			case frames <- audio.Frame{PCM: p, Offset: offset + ms(100), CapturedAt: time.Now()}:
				offset += ms(100)
			case <-stop:
				return
			case <-ctx.Done():
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	// Silence long past Hold trips the pause and lands the hook; the drain
	// publishes the trailing transcript either side of it.
	time.Sleep(150 * time.Millisecond) // some loud audio first
	setPCM(make([]byte, 3200))
	select {
	case <-hookFired:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the pause's connection-end hook")
	}
	// Resume: speech on a fresh connection, continuing the source clock.
	setPCM(loudPCM(3200))

	got := collect(t, out, 3)
	close(stop)
	cancel()
	<-done

	snap := met.Snapshot()
	if snap.STT.Pauses == 0 {
		t.Error("a pause/resume cycle must count as a pause")
	}
	if snap.STT.Reconnects != 0 {
		t.Errorf("a pause/resume cycle must not count as a reconnect, got %d", snap.STT.Reconnects)
	}
	// Every transcript continues the one source clock: nondecreasing start
	// positions, none restarted at zero by the fresh connection.
	prev := time.Duration(0)
	for i, tr := range got {
		if i > 0 && tr.Start < prev {
			t.Errorf("transcript %d (%q) restarted the clock: %v < %v", i, tr.Text(), tr.Start, prev)
		}
		prev = tr.End()
	}
	if texts := []string{got[0].Text(), got[1].Text(), got[2].Text()}; texts[0] != "trailing." || texts[2] != "resumed." {
		t.Errorf("texts = %v, want trailing/drained/resumed in order", texts)
	}
	if got[2].Start < got[1].End() {
		t.Errorf("resumed transcript at %v, want >= the drained transcript's end %v on the source clock", got[2].Start, got[1].End())
	}
}
