package stt

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"time"

	"github.com/coder/websocket"

	"livecaption/internal/audio"
	"livecaption/internal/metrics"
)

const (
	// idleInterval is how long a connection may go without audio before the
	// provider is asked for whatever keepalive its protocol needs. Sized for
	// the tightest documented idle timeout we stream to (Deepgram drops an
	// idle socket after ~10s), which leaves ample margin for everyone else.
	idleInterval = 5 * time.Second

	minBackoff = 250 * time.Millisecond
	maxBackoff = 8 * time.Second

	// bufferAudio is how much PCM survives a reconnect: enough to smooth a
	// brief network blip without dumping a stale chunk of audio on the
	// recognizer once the link recovers.
	bufferAudio = 2 * time.Second

	// drainTimeout bounds how long shutdown waits for trailing transcripts
	// after the polite end-of-stream, so ending a session can't hang on a
	// stalled server.
	drainTimeout = 3 * time.Second
)

// errPause is writeLoop's sentinel for "the gate went inactive", distinct from
// a real write failure: runConnection treats it as a polite hangup (finish(),
// no reconnect accounting) rather than a lost link.
var errPause = errors.New("stt: audio paused")

// PermanentError marks a failure that retrying cannot fix: a rejected key, an
// unknown model, a language the provider does not have. RunSession returns it
// straight out on the first attempt instead of backing off forever against a
// typo. Providers wrap their own errors in it, since only they can tell a
// misconfiguration apart from a network blip.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// IsPermanent reports whether err is (or wraps) a PermanentError.
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// Dialer opens one connection and completes whatever handshake the provider
// needs before audio can flow, returning the connection alongside the protocol
// state bound to it. The connection is handed back as well as the Session
// because the driver owns reading and closing it.
type Dialer func(ctx context.Context) (*websocket.Conn, Session, error)

// Session is one connection's protocol state: a fresh one per Dialer call, so
// per-connection counters (sequence numbers, byte clocks) reset with the
// socket and there is no reset path to race.
//
// Decode's ok reports whether the message carried a transcript worth
// publishing; its error is fatal and drops the connection. Anything the
// provider considers harmless noise — acks, metadata, revisable partials, an
// undecodable frame — is its own to log and swallow with ok=false, nil. In
// particular the "settled text only" guarantee is enforced here, per protocol:
// everything downstream paints a Transcript once and never revises it.
type Session interface {
	SendAudio(ctx context.Context, pcm []byte) error
	Idle(ctx context.Context) error
	Decode(data []byte) (t Transcript, ok bool, err error)
	Finish(ctx context.Context) error
}

// RunSession is an Engine minus the protocol: the reconnect state machine, the
// silence gate, the bounded audio buffer and the latency anchoring, driven by
// a provider's Dialer and Session. name appears in log messages only.
//
// It satisfies the Engine.Run contract: it returns only when ctx is cancelled,
// frames run out, or the provider reports a PermanentError.
func RunSession(ctx context.Context, cfg Config, name string, dial Dialer, frames <-chan audio.Frame, out chan<- Transcript) error {
	capBytes := cfg.Format.BytesFor(bufferAudio)
	if capBytes <= 0 {
		// Config.Format is zero-valued (e.g. a misconfigured caller); fall
		// back to the pipeline's own rate rather than buffering nothing.
		capBytes = audio.PipelineFormat.BytesFor(bufferAudio)
	}

	d := &driver{
		cfg:  cfg,
		name: name,
		dial: dial,
		log:  slog.Default(),
		met:  cfg.Metrics,
		gate: NewGate(cfg.Pause),
	}
	d.buf = newRing(capBytes, d.met, d.gate)
	d.framesClosed = startDrain(ctx, frames, d.gate, d.buf)

	return d.run(ctx, out)
}

// driver holds everything one RunSession call needs, so the per-connection
// helpers below take a handful of arguments instead of a dozen.
type driver struct {
	cfg  Config
	name string
	dial Dialer
	log  *slog.Logger
	met  *metrics.Metrics
	gate *Gate
	buf  *ring

	framesClosed <-chan struct{}
}

func (d *driver) run(ctx context.Context, out chan<- Transcript) error {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	backoff := minBackoff
	firstAttempt := true
	var ok bool

	for {
		if audioExhausted(d.framesClosed, d.buf) || ctx.Err() != nil {
			return nil
		}

		setSTTState(d.met, metrics.StateConnecting)
		conn, sess, err := d.dial(ctx)
		if err != nil {
			// Only the first attempt fails fast: a misconfiguration is
			// certain up front, whereas the same symptom mid-session is more
			// likely a server hiccup worth one more try.
			if firstAttempt && IsPermanent(err) && ctx.Err() == nil {
				return err
			}
			firstAttempt = false
			backoff, ok = d.retryAfter(ctx, err, "connect failed, retrying", backoff, rng)
			if !ok {
				return nil
			}
			continue
		}

		firstAttempt = false
		backoff = minBackoff
		setSTTState(d.met, metrics.StateConnected)
		d.log.Info(d.name + ": connected")

		oc, rerr := d.runConnection(ctx, conn, sess, out)
		switch oc {
		case outcomeDone:
			return nil

		case outcomePause:
			if !d.waitResume(ctx) {
				return nil
			}
			// A pause/resume cycle is not an error: no STTReconnect or
			// SetSTTError, so it never trips the /admin health badge.
			backoff = minBackoff

		default: // outcomeReconnect
			backoff, ok = d.retryAfter(ctx, rerr, "disconnected, reconnecting", backoff, rng)
			if !ok {
				return nil
			}
		}
	}
}

// startDrain drains frames into buf for the whole lifetime of the run,
// independent of connection state, so the audio source is never blocked by a
// dead or reconnecting link. Every frame is pushed, including silent ones while
// paused: the ring naturally holds the most recent ~2s, so when speech resumes
// it already contains the onset as pre-roll and the first word survives the
// redial. The returned channel closes once frames run out or ctx is cancelled.
func startDrain(ctx context.Context, frames <-chan audio.Frame, gate *Gate, buf *ring) <-chan struct{} {
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
// reporting false when the run should stop instead of redialing.
func (d *driver) waitResume(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	setSTTState(d.met, metrics.StatePaused)
	if d.met != nil {
		d.met.STTPauseBegin()
		defer d.met.STTPauseEnd()
	}
	d.log.Info(d.name + ": audio silent, connection paused")

	// Wait for the gate to go active again rather than polling it. Changed()
	// must be fetched *before* Active() is tested: it hands back the channel
	// for the next transition, so reading it after a false Active() would miss
	// a resume landing in between and park the connection until the pause
	// after next — a whole segment of speech lost with nothing in the logs to
	// show for it.
	for {
		changed := d.gate.Changed()
		if d.gate.Active() {
			return true
		}
		select {
		case <-changed:
		case <-d.framesClosed:
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
func (d *driver) retryAfter(ctx context.Context, err error, msg string, backoff time.Duration, rng *rand.Rand) (time.Duration, bool) {
	if ctx.Err() != nil {
		return backoff, false
	}
	if d.met != nil {
		d.met.SetSTTError(err)
		d.met.SetSTTState(metrics.StateReconnecting)
		d.met.STTReconnect()
	}
	d.log.Warn(d.name+": "+msg, "err", err, "retry_in", backoff)
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

// connOutcome tells the run loop what happened to one WebSocket lifetime,
// since "audio went silent" and "the link died" call for different handling: a
// pause is not an error and must not be counted as a reconnect.
type connOutcome int

const (
	outcomeDone connOutcome = iota
	outcomeReconnect
	outcomePause
)

// runConnection drives one WebSocket lifetime: a writer goroutine sending PCM
// and keepalives, a reader goroutine turning messages into Transcripts.
func (d *driver) runConnection(ctx context.Context, conn *websocket.Conn, sess Session, out chan<- Transcript) (connOutcome, error) {
	// connCtx is deliberately not derived from ctx: on shutdown we want to
	// keep reading trailing transcripts for a bit after the end-of-stream,
	// which a ctx-derived context would cut off immediately.
	connCtx, cancelConn := context.WithCancel(context.Background())
	defer cancelConn()

	// A new WebSocket means the recognizer's media clock restarts at 0, so the
	// anchor index must restart with it: idx is built here, before either
	// goroutine launches, and lives exactly as long as this connection. That
	// makes readLoop of connection N structurally unable to consult the index
	// of connection N+1, and there is no reset path to race.
	idx := newAnchorIndex(d.cfg.Format)

	readErr := make(chan error, 1)
	go func() { readErr <- d.readLoop(connCtx, conn, sess, out, idx) }()

	writeErr := make(chan error, 1)
	go func() { writeErr <- d.writeLoop(ctx, connCtx, sess, idx) }()

	select {
	case werr := <-writeErr:
		if errors.Is(werr, errPause) {
			// Audio went silent: wrap up exactly like a clean end-of-session
			// so trailing transcripts aren't lost, then let the run loop wait
			// for resume instead of redialing immediately.
			d.finish(conn, sess, readErr)
			return outcomePause, nil
		}
		if werr != nil {
			// A real write failure: the connection is already dead, no point
			// in a polite end-of-stream.
			cancelConn()
			<-readErr
			conn.CloseNow()
			return outcomeReconnect, werr
		}
		// Audio is exhausted (frames closed and drained) or ctx was
		// cancelled: wrap up politely so the tail of the session isn't lost.
		d.finish(conn, sess, readErr)
		return outcomeDone, nil

	case rerr := <-readErr:
		// The server ended the session or the read failed outright.
		cancelConn()
		<-writeErr
		conn.CloseNow()
		return outcomeReconnect, rerr
	}
}

// finish asks the provider to end the stream politely and waits for the server
// to either close its end or go quiet for drainTimeout, so trailing
// transcripts aren't lost.
func (d *driver) finish(conn *websocket.Conn, sess Session, readErr <-chan error) {
	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := sess.Finish(closeCtx); err != nil {
		d.log.Debug(d.name+": failed to end stream", "err", err)
	}
	select {
	case <-readErr:
	case <-time.After(drainTimeout):
		d.log.Debug(d.name + ": drain timed out waiting for trailing results")
	}
	conn.Close(websocket.StatusNormalClosure, "")
}

// writeLoop drains buf into the session and asks it for a keepalive whenever
// idleInterval passes with no audio sent. It returns nil on a clean end (audio
// exhausted or ctx cancelled), errPause once the gate goes inactive, and a
// non-nil error only on a real write failure, which the caller treats as
// "reconnect".
func (d *driver) writeLoop(ctx, connCtx context.Context, sess Session, idx *anchorIndex) error {
	ticker := time.NewTicker(idleInterval)
	defer ticker.Stop()
	lastActivity := time.Now()

	for {
		// Fetched before the Active() test for the same reason as in
		// waitResume: a transition landing between the two must not go
		// unnoticed.
		gateChanged := d.gate.Changed()
		if !d.gate.Active() {
			return errPause
		}

		if c, ok := d.buf.pop(); ok {
			// Recorded immediately BEFORE the write, not after: recording
			// after would leave a window where a fast server reply makes
			// readLoop look up bytes the index doesn't know about yet. If the
			// write then fails, the connection and this index are both
			// discarded together, so a pre-recorded entry is harmless. The
			// same "before" instant also stamps sentAt: the buffered socket
			// write itself only takes microseconds, but sentAt means "handed
			// to the socket", not "delivered" or "acknowledged".
			idx.Add(len(c.pcm), c.capturedAt, time.Now())
			if err := sess.SendAudio(connCtx, c.pcm); err != nil {
				return err
			}
			if d.met != nil {
				d.met.STTBytesSent(len(c.pcm))
			}
			lastActivity = time.Now()
			continue
		}

		select {
		case <-d.framesClosed:
			if d.buf.empty() {
				return nil
			}
			// Frames closed but buf still has data queued from just before
			// the close; loop back around to drain it.
		case <-d.buf.notify:
		case <-gateChanged:
			// Loop back to the top, which re-checks Active(): the pause
			// decision may have just flipped either way.
		case <-ticker.C:
			if time.Since(lastActivity) >= idleInterval {
				if err := sess.Idle(connCtx); err != nil {
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

// readLoop decodes server messages into Transcripts until the connection fails
// or ctx is cancelled. Whatever the session hands back is published as settled
// text, so it is the session's job to have dropped anything revisable first.
func (d *driver) readLoop(ctx context.Context, conn *websocket.Conn, sess Session, out chan<- Transcript, idx *anchorIndex) error {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		t, ok, err := sess.Decode(data)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		t.ReceivedAt = time.Now()
		// Anchor the segment's media end-time to the wall-clock capture and
		// send instants for THIS connection, so latency is measured against
		// when the audio actually entered the pipeline.
		if capturedAt, sentAt, ok := idx.At(t.End()); ok {
			t.CapturedAt = capturedAt
			t.SentAt = sentAt
		}
		select {
		case out <- t:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
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

// SecondsToDuration converts a provider's floating-point seconds into a
// Duration. Every streaming API reports media times this way.
func SecondsToDuration(s float64) time.Duration {
	return time.Duration(s * float64(time.Second))
}
