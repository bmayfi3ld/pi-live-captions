// Package deepgram streams PCM audio to Deepgram's real-time speech-to-text
// API over a WebSocket and turns its JSON messages into stt.Transcript.
//
// The connection is inherently unreliable (idle timeouts, network blips,
// server restarts), so most of this file is reconnect plumbing: a writer and
// a reader goroutine per connection, a bounded audio buffer that survives a
// reconnect, and exponential backoff around redials.
package deepgram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"livecaption/internal/audio"
	"livecaption/internal/metrics"
	"livecaption/internal/stt"
)

func init() {
	stt.Register("deepgram", func(cfg stt.Config) (stt.Engine, error) {
		return &Engine{cfg: cfg}, nil
	})
}

const (
	endpoint = "wss://api.deepgram.com/v1/listen"

	// keepAliveInterval matches Deepgram's documented idle timeout: without
	// traffic for ~10s the server drops the connection, so 5s of silence is
	// enough margin to never trip it.
	keepAliveInterval = 5 * time.Second

	// readLimit accommodates Results messages with full word arrays, which
	// exceed the library's 32KB default read limit on longer utterances.
	readLimit = 1 << 20

	minBackoff = 250 * time.Millisecond
	maxBackoff = 8 * time.Second

	// bufferAudio is how much PCM survives a reconnect: enough to smooth a
	// brief network blip without dumping a stale chunk of audio on Deepgram
	// once the link recovers.
	bufferAudio = 2 * time.Second

	// drainTimeout bounds how long shutdown waits for trailing Results after
	// CloseStream, so ending a session can't hang on a stalled server.
	drainTimeout = 3 * time.Second
)

// errPause is writeLoop's sentinel for "the gate went inactive", distinct
// from a real write failure: runConnection treats it as a polite hangup
// (finish(), no reconnect accounting) rather than a lost link.
var errPause = errors.New("deepgram: audio paused")

// Engine streams PCM to Deepgram's real-time API and turns its JSON messages
// into stt.Transcript. It owns its own reconnect logic per the stt.Engine
// contract: Run returns only when ctx is cancelled or frames run out.
type Engine struct {
	cfg stt.Config

	// wsURL overrides the Deepgram endpoint. Only ever set by tests, which
	// point it at an httptest server instead of the real API.
	wsURL string
}

func (e *Engine) Name() string { return "deepgram" }

func (e *Engine) Run(ctx context.Context, frames <-chan audio.Frame, out chan<- stt.Transcript) error {
	log := slog.Default()
	met := e.cfg.Metrics
	gate := stt.NewGate(e.cfg.Pause)

	capBytes := e.cfg.Format.BytesFor(bufferAudio)
	if capBytes <= 0 {
		// Config.Format is zero-valued (e.g. a misconfigured caller); fall
		// back to the pipeline's own rate rather than buffering nothing.
		capBytes = audio.PipelineFormat.BytesFor(bufferAudio)
	}
	buf := newRing(capBytes, met, gate)

	framesClosed := startDrain(ctx, frames, gate, buf)

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	backoff := minBackoff
	firstAttempt := true
	var ok bool

	for {
		if audioExhausted(framesClosed, buf) || ctx.Err() != nil {
			return nil
		}

		setSTTState(met, metrics.StateConnecting)
		conn, err := e.connect(ctx)
		if err != nil {
			if firstAttempt && isAuthError(err) && ctx.Err() == nil {
				return fmt.Errorf("deepgram: %w (check DEEPGRAM_API_KEY)", err)
			}
			firstAttempt = false
			backoff, ok = retryAfter(ctx, err, "deepgram: connect failed, retrying", backoff, rng, met, log)
			if !ok {
				return nil
			}
			continue
		}

		firstAttempt = false
		backoff = minBackoff
		setSTTState(met, metrics.StateConnected)
		log.Info("deepgram: connected")

		oc, rerr := e.runConnection(ctx, conn, buf, framesClosed, out, log, met, gate)
		switch oc {
		case outcomeDone:
			return nil

		case outcomePause:
			if !waitResume(ctx, gate, framesClosed, met, log) {
				return nil
			}
			// A pause/resume cycle is not an error: no STTReconnect or
			// SetSTTError, since Snapshot.Clean() keys off reconnects.
			backoff = minBackoff

		default: // outcomeReconnect
			backoff, ok = retryAfter(ctx, rerr, "deepgram: disconnected, reconnecting", backoff, rng, met, log)
			if !ok {
				return nil
			}
		}
	}
}

// startDrain drains frames into buf for the whole lifetime of Run, independent
// of connection state, so the audio source is never blocked by a dead or
// reconnecting link. Every frame is pushed, including silent ones while
// paused: the ring naturally holds the most recent ~2s, so when speech resumes
// it already contains the onset as pre-roll and the first word survives the
// redial. The returned channel closes once frames run out or ctx is cancelled.
func startDrain(ctx context.Context, frames <-chan audio.Frame, gate *stt.Gate, buf *ring) <-chan struct{} {
	framesClosed := make(chan struct{})
	go func() {
		defer close(framesClosed)
		for {
			select {
			case f, ok := <-frames:
				if !ok {
					return
				}
				gate.Observe(f)
				buf.push(f)
			case <-ctx.Done():
				return
			}
		}
	}()
	return framesClosed
}

// waitResume parks a paused connection until the gate goes active again,
// reporting false when Run should stop instead of redialing.
func waitResume(ctx context.Context, gate *stt.Gate, framesClosed <-chan struct{}, met *metrics.Metrics, log *slog.Logger) bool {
	if ctx.Err() != nil {
		return false
	}
	setSTTState(met, metrics.StatePaused)
	if met != nil {
		met.STTPauseBegin()
		defer met.STTPauseEnd()
	}
	log.Info("deepgram: audio silent, connection paused")

	// Wait for the gate to go active again rather than polling it. Changed()
	// must be fetched *before* Active() is tested: it hands back the channel
	// for the next transition, so reading it after a false Active() would miss
	// a resume landing in between and park the connection until the pause
	// after next — a whole segment of speech lost with nothing in the logs to
	// show for it.
	for {
		changed := gate.Changed()
		if gate.Active() {
			return true
		}
		select {
		case <-changed:
		case <-framesClosed:
			// Ring is full of silence; nothing worth redialing for.
			return false
		case <-ctx.Done():
			return false
		}
	}
}

// retryAfter records a lost or refused connection and waits out the backoff,
// returning the next backoff and false if ctx ended first (or was already
// cancelled, in which case the failure isn't counted at all: a shutdown is not
// a reconnect).
func retryAfter(ctx context.Context, err error, msg string, backoff time.Duration, rng *rand.Rand, met *metrics.Metrics, log *slog.Logger) (time.Duration, bool) {
	if ctx.Err() != nil {
		return backoff, false
	}
	if met != nil {
		met.SetSTTError(err)
		met.SetSTTState(metrics.StateReconnecting)
		met.STTReconnect()
	}
	log.Warn(msg, "err", err, "retry_in", backoff)
	if !sleepBackoff(ctx, backoff, rng) {
		return backoff, false
	}
	return nextBackoff(backoff), true
}

func setSTTState(met *metrics.Metrics, s metrics.ConnState) {
	if met != nil {
		met.SetSTTState(s)
	}
}

func audioExhausted(framesClosed <-chan struct{}, buf *ring) bool {
	select {
	case <-framesClosed:
		return buf.empty()
	default:
		return false
	}
}

// connOutcome tells Run what happened to one WebSocket lifetime, since
// "audio went silent" and "the link died" call for different handling: a
// pause is not an error and must not be counted as a reconnect.
type connOutcome int

const (
	outcomeDone connOutcome = iota
	outcomeReconnect
	outcomePause
)

// runConnection drives one WebSocket lifetime: a writer goroutine sending PCM
// and KeepAlives, a reader goroutine turning Results into Transcripts.
func (e *Engine) runConnection(
	ctx context.Context,
	conn *websocket.Conn,
	buf *ring,
	framesClosed <-chan struct{},
	out chan<- stt.Transcript,
	log *slog.Logger,
	met *metrics.Metrics,
	gate *stt.Gate,
) (connOutcome, error) {
	// connCtx is deliberately not derived from ctx: on shutdown we want to
	// keep reading trailing Results for a bit after CloseStream, which a
	// ctx-derived context would cut off immediately.
	connCtx, cancelConn := context.WithCancel(context.Background())
	defer cancelConn()

	// A new WebSocket means Deepgram's byte-counting clock restarts at 0, so
	// the anchor index must restart with it: idx is built here, before either
	// goroutine launches, and lives exactly as long as this connection. That
	// makes readLoop of connection N structurally unable to consult the index
	// of connection N+1, and there is no reset path to race.
	idx := newAnchorIndex(e.cfg.Format)
	// pt has the same lifetime for the same reason: a stable prefix is a
	// statement about what THIS connection's revisions have settled on, and a
	// reconnect must not carry that state into what is really a fresh window.
	pt := newPrefixTracker()

	readErr := make(chan error, 1)
	go func() { readErr <- e.readLoop(connCtx, conn, out, met, log, idx, pt) }()

	writeErr := make(chan error, 1)
	go func() { writeErr <- e.writeLoop(ctx, connCtx, conn, buf, framesClosed, met, gate, idx) }()

	select {
	case werr := <-writeErr:
		if errors.Is(werr, errPause) {
			// Audio went silent: wrap up exactly like a clean end-of-session
			// so trailing Results aren't lost, then let Run wait for resume
			// instead of redialing immediately.
			e.finish(conn, readErr, log)
			return outcomePause, nil
		}
		if werr != nil {
			// A real write failure: the connection is already dead, no point
			// sending CloseStream.
			cancelConn()
			<-readErr
			conn.CloseNow()
			return outcomeReconnect, werr
		}
		// Audio is exhausted (frames closed and drained) or ctx was
		// cancelled: wrap up politely so the tail of the session isn't lost.
		e.finish(conn, readErr, log)
		return outcomeDone, nil

	case rerr := <-readErr:
		// The server ended the session or the read failed outright.
		cancelConn()
		<-writeErr
		conn.CloseNow()
		return outcomeReconnect, rerr
	}
}

// finish sends CloseStream and waits for the server to either close its end
// or go quiet for drainTimeout, so trailing Results aren't lost.
func (e *Engine) finish(conn *websocket.Conn, readErr <-chan error, log *slog.Logger) {
	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := writeJSON(closeCtx, conn, controlMessage{Type: "CloseStream"}); err != nil {
		log.Debug("deepgram: failed to send CloseStream", "err", err)
	}
	select {
	case <-readErr:
	case <-time.After(drainTimeout):
		log.Debug("deepgram: drain timed out waiting for trailing results")
	}
	conn.Close(websocket.StatusNormalClosure, "")
}

// writeLoop drains buf into binary WebSocket frames and sends a KeepAlive
// whenever 5s pass with no audio sent. It returns nil on a clean end (audio
// exhausted or ctx cancelled), errPause once the gate goes inactive, and a
// non-nil error only on a real write failure, which the caller treats as
// "reconnect".
func (e *Engine) writeLoop(
	ctx context.Context,
	connCtx context.Context,
	conn *websocket.Conn,
	buf *ring,
	framesClosed <-chan struct{},
	met *metrics.Metrics,
	gate *stt.Gate,
	idx *anchorIndex,
) error {
	ticker := time.NewTicker(keepAliveInterval)
	defer ticker.Stop()
	lastActivity := time.Now()

	for {
		// Fetched before the Active() test for the same reason as in Run:
		// a transition landing between the two must not go unnoticed.
		gateChanged := gate.Changed()
		if !gate.Active() {
			return errPause
		}

		if c, ok := buf.pop(); ok {
			// Recorded immediately BEFORE the write, not after: recording
			// after would leave a window where a fast server reply makes
			// readLoop look up bytes the index doesn't know about yet. If the
			// write then fails, the connection and this index are both
			// discarded together, so a pre-recorded entry is harmless. The
			// same "before" instant also stamps sentAt: the buffered socket
			// write itself only takes microseconds, but sentAt means "handed
			// to the socket", not "delivered" or "acknowledged" by Deepgram.
			idx.Add(len(c.pcm), c.capturedAt, time.Now())
			if err := conn.Write(connCtx, websocket.MessageBinary, c.pcm); err != nil {
				return err
			}
			if met != nil {
				met.STTBytesSent(len(c.pcm))
			}
			lastActivity = time.Now()
			continue
		}

		select {
		case <-framesClosed:
			if buf.empty() {
				return nil
			}
			// Frames closed but buf still has data queued from just before
			// the close; loop back around to drain it.
		case <-buf.notify:
		case <-gateChanged:
			// Loop back to the top, which re-checks Active(): the pause
			// decision may have just flipped either way.
		case <-ticker.C:
			if time.Since(lastActivity) >= keepAliveInterval {
				if err := writeJSON(connCtx, conn, controlMessage{Type: "KeepAlive"}); err != nil {
					return err
				}
				lastActivity = time.Now()
			}
		case <-ctx.Done():
			return nil
		case <-connCtx.Done():
			return connCtx.Err()
		}
	}
}

// readLoop decodes server messages into Transcripts until the connection
// fails or ctx is cancelled. Every decoded message passes through pt first:
// pt is what turns Deepgram's revisable interim/final stream into the
// append-only segments out and everything downstream expect.
func (e *Engine) readLoop(ctx context.Context, conn *websocket.Conn, out chan<- stt.Transcript, met *metrics.Metrics, log *slog.Logger, idx *anchorIndex, pt *prefixTracker) error {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		t, isFinal, ok, err := decodeTranscript(data)
		if err != nil {
			log.Debug("deepgram: undecodable message", "err", err)
			continue
		}
		if !ok {
			continue
		}
		seg, ok := pt.update(t, isFinal)
		if !ok {
			// Nothing past the already-published prefix and holdback is
			// stable yet; wait for the next interim or the final.
			continue
		}
		seg.ReceivedAt = time.Now()
		// Anchor the segment's media end-time to wall-clock capture and send
		// time on THIS connection. seg.End() lines up exactly with t.End() of
		// whichever message triggered the publish, so this is as precise as
		// the message-level anchor index can be even though seg itself only
		// covers the newly published tokens.
		if capturedAt, sentAt, ok := idx.At(seg.End()); ok {
			seg.CapturedAt = capturedAt
			seg.SentAt = sentAt
		}
		select {
		case out <- seg:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// connect dials Deepgram and returns a ready-to-use connection. Failures that
// carry an HTTP status are wrapped in dialError so the caller can recognize
// an auth failure and fail fast instead of retrying forever.
func (e *Engine) connect(ctx context.Context) (*websocket.Conn, error) {
	h := http.Header{}
	h.Set("Authorization", "Token "+e.cfg.APIKey)

	conn, resp, err := websocket.Dial(ctx, e.dialURL(), &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		if resp != nil {
			return nil, &dialError{status: resp.StatusCode, err: err}
		}
		return nil, err
	}
	conn.SetReadLimit(readLimit)
	return conn, nil
}

func (e *Engine) dialURL() string {
	base := e.wsURL
	if base == "" {
		base = endpoint
	}

	q := url.Values{}
	q.Set("encoding", encodingFor(e.cfg.Format))
	q.Set("sample_rate", strconv.Itoa(e.cfg.Format.SampleRate))
	q.Set("channels", strconv.Itoa(e.cfg.Format.Channels))
	q.Set("model", e.cfg.Model)
	q.Set("language", e.cfg.Language)
	// interim_results=true: prefixTracker (see prefix.go) needs the revisable
	// stream to publish a stable prefix well before is_final, which is what
	// keeps text landing at a natural speech cadence instead of arriving in
	// multi-second bursts gated on Deepgram's own finalization window.
	q.Set("interim_results", "true")
	// punctuate and smart_format are load-bearing, not cosmetic: the hub
	// closes transcript lines on terminal punctuation (§3 of the interim
	// removal plan), so turning these off would silently degrade
	// transcript.txt to the speech-gap fallback for every sentence.
	q.Set("punctuate", "true")
	q.Set("profanity_filter", "true")
	q.Set("smart_format", "true")
	// Deepgram's own `endpointing` is deliberately not set: prefixTracker
	// publishes off interims, so cadence no longer waits on the server's
	// finalization window.
	for _, k := range e.cfg.Keyterms {
		q.Add("keyterm", k)
	}
	return base + "?" + q.Encode()
}

// encodingFor names the PCM layout for Deepgram's `encoding` parameter. The
// pipeline is fixed at 16-bit signed samples, but this stays format-driven
// rather than hardcoded in case that ever changes.
func encodingFor(f audio.Format) string {
	if f.BitDepth == 8 {
		return "mulaw"
	}
	return "linear16"
}

// dialError carries the HTTP status from a failed handshake, so a 401/403 can
// be told apart from a plain network failure.
type dialError struct {
	status int
	err    error
}

func (e *dialError) Error() string { return e.err.Error() }
func (e *dialError) Unwrap() error { return e.err }

func isAuthError(err error) bool {
	var de *dialError
	if errors.As(err, &de) {
		return de.status == http.StatusUnauthorized || de.status == http.StatusForbidden
	}
	return false
}

// sleepBackoff waits a jittered duration around d, reporting whether it slept
// to completion (false means ctx was cancelled first).
func sleepBackoff(ctx context.Context, d time.Duration, rng *rand.Rand) bool {
	half := d / 2
	jittered := half + time.Duration(rng.Int63n(int64(half)+1))
	t := time.NewTimer(jittered)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

type controlMessage struct {
	Type string `json:"type"`
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, b)
}

// messageType is decoded first so only the shape a message actually claims to
// be attempts a full unmarshal: SpeechStarted carries "channel" as a plain
// index array rather than the object Results uses, so blindly unmarshaling
// everything as resultsMessage would fail on it.
type messageType struct {
	Type string `json:"type"`
}

// resultsMessage matches the subset of Deepgram's Results JSON this engine
// cares about; word timings and everything else are decoded straight through
// and ignored.
type resultsMessage struct {
	IsFinal  bool    `json:"is_final"`
	Start    float64 `json:"start"`
	Duration float64 `json:"duration"`
	Channel  struct {
		Alternatives []struct {
			Transcript string  `json:"transcript"`
			Confidence float64 `json:"confidence"`
		} `json:"alternatives"`
	} `json:"channel"`
}

// decodeTranscript turns one server message into a Transcript plus whether it
// was Deepgram's is_final for that window. ok is false for messages that
// carry nothing worth publishing: empty Results (no speech yet), Metadata,
// SpeechStarted, and any other unrecognized type. Both interim and final
// Results decode with ok=true — deciding which of their words are actually
// safe to publish is prefixTracker's job (see prefix.go), not this one's.
func decodeTranscript(data []byte) (t stt.Transcript, isFinal bool, ok bool, err error) {
	var head messageType
	if err := json.Unmarshal(data, &head); err != nil {
		return stt.Transcript{}, false, false, err
	}

	if head.Type != "Results" {
		return stt.Transcript{}, false, false, nil
	}

	var msg resultsMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return stt.Transcript{}, false, false, err
	}
	// if !msg.IsFinal {
	// 	return stt.Transcript{}, false, false, err
	// }
	if len(msg.Channel.Alternatives) == 0 {
		return stt.Transcript{}, false, false, nil
	}
	alt := msg.Channel.Alternatives[0]
	if alt.Transcript == "" {
		return stt.Transcript{}, false, false, nil
	}
	return stt.Transcript{
		Text:       alt.Transcript,
		Start:      secondsToDuration(msg.Start),
		Duration:   secondsToDuration(msg.Duration),
		Confidence: alt.Confidence,
	}, msg.IsFinal, true, nil
}

func secondsToDuration(s float64) time.Duration {
	return time.Duration(s * float64(time.Second))
}

// chunk is one ring entry: PCM plus the wall time it was captured, which is
// what latency is ultimately measured against.
type chunk struct {
	pcm        []byte
	capturedAt time.Time
}

// ring holds PCM chunks while the connection is down or catching up,
// dropping the oldest chunk once full so a blip never backpressures capture.
type ring struct {
	mu       sync.Mutex
	chunks   []chunk
	bytes    int
	capBytes int
	met      *metrics.Metrics
	gate     *stt.Gate

	// notify wakes writeLoop when data arrives; buffered 1 and non-blocking
	// to push so a slow or absent reader of it never stalls push.
	notify chan struct{}
}

func newRing(capBytes int, met *metrics.Metrics, gate *stt.Gate) *ring {
	return &ring{capBytes: capBytes, met: met, gate: gate, notify: make(chan struct{}, 1)}
}

func (r *ring) push(f audio.Frame) {
	r.mu.Lock()
	r.chunks = append(r.chunks, chunk{pcm: f.PCM, capturedAt: f.CapturedAt})
	r.bytes += len(f.PCM)
	for r.bytes > r.capBytes && len(r.chunks) > 1 {
		dropped := r.chunks[0]
		r.chunks = r.chunks[1:]
		r.bytes -= len(dropped.pcm)
		// While the gate is inactive, an eviction is the pre-roll buffer
		// working as designed: we keep pushing silent frames so the ring
		// always holds the most recent ~bufferAudio, and the oldest stale
		// silence has to go somewhere. That's not degradation, so it stays
		// uncounted. While the gate is active, though, evicting live audio
		// means the link isn't draining fast enough to keep up — that IS
		// worth flagging.
		if r.met != nil && r.gate != nil && r.gate.Active() {
			r.met.STTBufferDrop()
		}
	}
	r.mu.Unlock()

	select {
	case r.notify <- struct{}{}:
	default:
	}
}

func (r *ring) pop() (chunk, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.chunks) == 0 {
		return chunk{}, false
	}
	c := r.chunks[0]
	r.chunks = r.chunks[1:]
	r.bytes -= len(c.pcm)
	return c, true
}

func (r *ring) empty() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.chunks) == 0
}
