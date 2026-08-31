// Package metrics holds the single mutable snapshot of how the session is
// going. The admin page, the status line and the shutdown summary all read
// from here, so they can never disagree with each other.
//
// Rule of thumb: anything that can degrade silently gets a counter here.
package metrics

import (
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// ConnState is the STT connection state, shown on the status line and /admin.
type ConnState int32

const (
	StateIdle ConnState = iota
	StateConnecting
	StateConnected
	StateReconnecting
	StateClosed
	// StatePaused is appended after StateClosed so existing values don't
	// renumber; a session that never auto-pauses never sees it.
	StatePaused
)

func (s ConnState) String() string {
	switch s {
	case StateConnecting:
		return "connecting"
	case StateConnected:
		return "connected"
	case StateReconnecting:
		return "reconnecting"
	case StateClosed:
		return "closed"
	case StatePaused:
		return "paused"
	default:
		return "idle"
	}
}

// Metrics is safe for concurrent use by every stage of the pipeline.
type Metrics struct {
	// Immutable session identity, set once at startup.
	Version   string
	SessionID string
	StartedAt time.Time

	// Source.
	SourceKind   string // "replay" or "live"
	SourceSpec   string // file path or device name
	SourceFormat string // "44100 Hz stereo -> 16000 Hz mono"

	framesTotal    atomic.Int64
	bytesTotal     atomic.Int64
	framesDropped  atomic.Int64
	ffmpegRestarts atomic.Int64
	xruns          atomic.Int64

	// Monitor (replay --monitor only).
	MonitorEnabled bool
	monitorDropped atomic.Int64
	monitorAlive   atomic.Bool

	// Audio stream (/audio.mp3).
	AudioEnabled        bool   // the flag; false under --no-audio-stream
	AudioReason         string // why it is degraded, "" when healthy
	audioLive           atomic.Bool
	audioListeners      atomic.Int64
	audioListenersTotal atomic.Int64
	audioDropped        atomic.Int64

	// STT.
	Engine string
	// sttStateHook is set once at startup, before any goroutine that could
	// fire it exists, so it needs no synchronization of its own.
	sttStateHook   func(ConnState)
	sttState       atomic.Int32
	sttReconnect   atomic.Int64
	sttSegments    atomic.Int64
	sttLines       atomic.Int64
	sttBytesSent   atomic.Int64
	sttPauses      atomic.Int64
	sttBufferDrops atomic.Int64

	// Web.
	sseClients      atomic.Int64
	sseClientsTotal atomic.Int64
	sseEvents       atomic.Int64
	sseSlowDrops    atomic.Int64

	// Transcript.
	TranscriptPath string
	transcriptLine atomic.Int64
	transcriptByte atomic.Int64

	mu             sync.RWMutex
	lastStderr     string
	sttLastErr     string
	sttLastErrAt   time.Time
	sttPauseStart  time.Time     // zero when no pause is currently open
	sttPausedTotal time.Duration // accumulated from completed pauses only
	transcriptErr  string
	latCaption     latencySeries // one series now that there's only one kind of segment: the headline figure
	latViewer      latencySeries // browser-reported publish->paint, POSTed by viewers
	latUpload      latencySeries // CapturedAt -> released to the recognizer's socket
	latRecognize   latencySeries // released -> ReceivedAt
	latAssemble    latencySeries // ReceivedAt -> hub publish
	mediaProcessed time.Duration
	mediaTotal     time.Duration // 0 for live (unknown length)
	lastDegradedAt time.Time     // zero until the first silent-degradation event

	viewerReports atomic.Int64
}

// latencySample pairs a measurement with when it was taken, so the series can
// age samples out instead of only bounding them by count.
type latencySample struct {
	d  time.Duration
	at time.Time
}

// latencySeries is a time-windowed FIFO of latency samples. Percentiles and
// max describe the window, not the session: the pre-roll flush after an
// auto-pause resume is a true reading of that audio's latency, but it must not
// go on defining p95 and max for the next half hour.
type latencySeries struct {
	samples []latencySample // ascending by at
}

// observe appends a sample and trims the series to the current window.
// Negative durations are rejected the same way the old ring rejected them:
// a clock skew or ordering hiccup upstream must not corrupt the figures
// operators are reading live.
func (s *latencySeries) observe(d time.Duration, now time.Time) {
	if d < 0 {
		return
	}
	s.samples = append(s.samples, latencySample{d, now})
	s.trim(now)
}

// trim drops samples older than latencyWindow, then caps the remainder at
// latencyCap, compacting the backing array in place so it can't grow without
// bound across a long session.
func (s *latencySeries) trim(now time.Time) {
	i := 0
	for i < len(s.samples) && now.Sub(s.samples[i].at) > latencyWindow {
		i++
	}
	if len(s.samples)-i > latencyCap {
		i = len(s.samples) - latencyCap
	}
	if i > 0 {
		s.samples = append(s.samples[:0], s.samples[i:]...)
	}
}

// stats trims to the current window and returns the figures /admin reads:
// last is the most recent retained sample, p50/p95/max describe the window,
// and n is how many samples it holds.
func (s *latencySeries) stats(now time.Time) (last, p50, p95, max time.Duration, n int) {
	s.trim(now)
	n = len(s.samples)
	if n == 0 {
		return 0, 0, 0, 0, 0
	}
	last = s.samples[n-1].d
	sorted := make([]time.Duration, n)
	for i, samp := range s.samples {
		sorted[i] = samp.d
	}
	slices.Sort(sorted)
	p50 = percentile(sorted, 0.50)
	p95 = percentile(sorted, 0.95)
	max = sorted[n-1]
	return last, p50, p95, max, n
}

// latencyWindow is how far back the latency figures look. Long enough for a
// stable p95 at realistic final rates (~15/min, so ~75 samples), short enough
// that a spike doesn't outlive the part of the event it happened in.
const latencyWindow = 5 * time.Minute

// latencyCap bounds memory regardless of sample rate.
const latencyCap = 512

// degradedWindow is how long a silent-degradation event keeps the /admin
// health badge at "degraded" after it happens. A ring eviction or a
// reconnect is worth flagging right away, but the badge must not latch
// there forever off one blip from an hour ago — it reports current state,
// the cumulative counters (Clean, the tiles) are what remember the whole
// session.
const degradedWindow = 60 * time.Second

func New(version, sessionID string) *Metrics {
	return &Metrics{
		Version:   version,
		SessionID: sessionID,
		StartedAt: time.Now(),
	}
}

// --- Source ---

// AddFrame records one PCM frame reaching the pipeline. offset is the media
// time of the frame's last sample, which is what drives progress display.
func (m *Metrics) AddFrame(nbytes int, offset time.Duration) {
	m.framesTotal.Add(1)
	m.bytesTotal.Add(int64(nbytes))
	m.mu.Lock()
	m.mediaProcessed = offset
	m.mu.Unlock()
}

func (m *Metrics) SetMediaTotal(d time.Duration) {
	m.mu.Lock()
	m.mediaTotal = d
	m.mu.Unlock()
}

// markDegradedLocked stamps the time of the most recent silent-degradation
// event, which the health badge in Snapshot() uses to decide "degraded" vs
// "ok". Callers must already hold m.mu — it never takes the lock itself, so
// that SetTranscriptError (which holds m.mu across its own write) can call
// it inline instead of re-entering a non-reentrant sync.RWMutex.
func (m *Metrics) markDegradedLocked() {
	m.lastDegradedAt = time.Now()
}

// degrade increments a silent-degradation counter and stamps the health
// badge's recency window in one step. Every counter that means "something
// went wrong quietly" goes through here, so none can be bumped without the
// badge noticing.
func (m *Metrics) degrade(c *atomic.Int64) {
	c.Add(1)
	m.mu.Lock()
	m.markDegradedLocked()
	m.mu.Unlock()
}

func (m *Metrics) DropFrame()             { m.degrade(&m.framesDropped) }
func (m *Metrics) FFmpegRestart()         { m.degrade(&m.ffmpegRestarts) }
func (m *Metrics) Xrun()                  { m.degrade(&m.xruns) }
func (m *Metrics) MonitorDrop()           { m.degrade(&m.monitorDropped) }
func (m *Metrics) SetMonitorAlive(v bool) { m.monitorAlive.Store(v) }

func (m *Metrics) AudioDrop()          { m.degrade(&m.audioDropped) }
func (m *Metrics) SetAudioLive(v bool) { m.audioLive.Store(v) }
func (m *Metrics) AudioListenerJoined() {
	m.audioListeners.Add(1)
	m.audioListenersTotal.Add(1)
}
func (m *Metrics) AudioListenerLeft() { m.audioListeners.Add(-1) }

func (m *Metrics) SetLastStderr(s string) {
	m.mu.Lock()
	m.lastStderr = s
	m.mu.Unlock()
}

// --- STT ---

// SetSTTState fires the hook registered with SetSTTStateHook only when the
// value actually changes, so a stretch of identical states (e.g. repeated
// StateConnected sets) doesn't spam status subscribers with no-op events.
func (m *Metrics) SetSTTState(s ConnState) {
	old := m.sttState.Swap(int32(s))
	if old == int32(s) {
		return
	}
	if m.sttStateHook != nil {
		m.sttStateHook(s)
	}
}
func (m *Metrics) STTState() ConnState { return ConnState(m.sttState.Load()) }
func (m *Metrics) STTReconnect()       { m.degrade(&m.sttReconnect) }

// STTSegment counts a caption segment painted to the display. Kept
// deliberately distinct from STTLine: segments_total / lines_total is the
// direct readout for whether --endpointing is sane — a ratio climbing well
// past a few segments per line means it is too low and phrases are
// fragmenting.
func (m *Metrics) STTSegment() { m.sttSegments.Add(1) }

// STTLine counts a transcript line closed by the hub.
func (m *Metrics) STTLine()           { m.sttLines.Add(1) }
func (m *Metrics) STTBytesSent(n int) { m.sttBytesSent.Add(int64(n)) }

// STTBufferDrop records an eviction from the reconnect ring that happened
// while the gate was active — i.e. the link is not keeping up with live
// audio, not the pre-roll buffer discarding stale silence during a pause.
// See ring.push in internal/stt/deepgram/deepgram.go for the gating logic.
func (m *Metrics) STTBufferDrop() { m.degrade(&m.sttBufferDrops) }

// SetSTTStateHook registers a callback invoked when the STT state changes.
// Must be called before the session starts — it is not safe against a
// concurrent SetSTTState — and is called from engine goroutines thereafter, so
// the callback must not block.
func (m *Metrics) SetSTTStateHook(f func(ConnState)) { m.sttStateHook = f }

func (m *Metrics) SetSTTError(err error) {
	m.mu.Lock()
	if err != nil {
		m.sttLastErr = err.Error()
		m.sttLastErrAt = time.Now()
	}
	m.mu.Unlock()
}

// STTPauseBegin records the start of an automatic pause and increments the
// pause counter. Idempotent: a second Begin before the matching End leaves
// the original start time in place, so a stray duplicate call can't inflate
// PausedSec.
func (m *Metrics) STTPauseBegin() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.sttPauseStart.IsZero() {
		return
	}
	m.sttPauseStart = time.Now()
	m.sttPauses.Add(1)
}

// STTPauseEnd accumulates the elapsed time of the currently open pause.
// Idempotent: a call with no open pause (no matching Begin, or already
// ended) is a no-op.
func (m *Metrics) STTPauseEnd() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sttPauseStart.IsZero() {
		return
	}
	m.sttPausedTotal += time.Since(m.sttPauseStart)
	m.sttPauseStart = time.Time{}
}

// ObserveLatency records how far behind wall clock a caption segment
// arrived. There is only one series now, since every segment the pipeline
// emits is already settled: the headline figure IS time-to-first-pixels,
// with no earlier, more optimistic paint to also track.
func (m *Metrics) ObserveLatency(d time.Duration) {
	m.mu.Lock()
	m.latCaption.observe(d, time.Now())
	m.mu.Unlock()
}

// ObserveViewerLatency records a browser-reported publish->paint span, POSTed
// by the viewer itself, and counts it toward viewerReports so the two figures
// can never disagree about how many reports were accepted.
func (m *Metrics) ObserveViewerLatency(d time.Duration) {
	if d < 0 {
		return
	}
	m.mu.Lock()
	m.latViewer.observe(d, time.Now())
	m.mu.Unlock()
	m.viewerReports.Add(1)
}

// ObservePhases records the three-way split of one transcript's pipeline
// latency: upload (CapturedAt -> released to the recognizer's socket),
// recognize (released -> ReceivedAt) and assemble (ReceivedAt -> hub
// publish). All three are recorded under one lock with one timestamp so the
// series can never hold samples from different transcripts, and all three are
// dropped together if any is negative — a partial split would let the
// stacked bar on /admin disagree with the total it sits under.
func (m *Metrics) ObservePhases(upload, recognize, assemble time.Duration) {
	if upload < 0 || recognize < 0 || assemble < 0 {
		return
	}
	now := time.Now()
	m.mu.Lock()
	m.latUpload.observe(upload, now)
	m.latRecognize.observe(recognize, now)
	m.latAssemble.observe(assemble, now)
	m.mu.Unlock()
}

// --- Web ---

func (m *Metrics) SSEConnect() {
	m.sseClients.Add(1)
	m.sseClientsTotal.Add(1)
}
func (m *Metrics) SSEDisconnect() { m.sseClients.Add(-1) }
func (m *Metrics) SSEEvent()      { m.sseEvents.Add(1) }
func (m *Metrics) SSESlowDrop()   { m.degrade(&m.sseSlowDrops) }

// --- Transcript ---

func (m *Metrics) TranscriptWrote(lines, bytes int) {
	m.transcriptLine.Add(int64(lines))
	m.transcriptByte.Add(int64(bytes))
}

func (m *Metrics) SetTranscriptError(err error) {
	m.mu.Lock()
	if err != nil {
		m.transcriptErr = err.Error()
		// Call markDegradedLocked directly rather than through a
		// self-locking wrapper: we already hold m.mu here, and it's a plain
		// sync.RWMutex, not reentrant — taking it twice would deadlock.
		m.markDegradedLocked()
	}
	m.mu.Unlock()
}

// --- Snapshot ---

// Snapshot is a consistent point-in-time copy, serialized straight to JSON for
// /api/stats and used verbatim by the status line and shutdown summary.
type Snapshot struct {
	Version   string    `json:"version"`
	SessionID string    `json:"session_id"`
	StartedAt time.Time `json:"started_at"`
	UptimeSec float64   `json:"uptime_sec"`
	// Health is the server-computed "what is happening right now" summary —
	// "closed" / "paused" / "degraded" / "ok" — so the status line, /admin
	// and the shutdown summary read the exact same verdict instead of each
	// re-deriving their own from the raw counters.
	Health string `json:"health"`

	Source struct {
		Kind           string  `json:"kind"`
		Spec           string  `json:"spec"`
		Format         string  `json:"format"`
		FramesTotal    int64   `json:"frames_total"`
		BytesTotal     int64   `json:"bytes_total"`
		SecondsTotal   float64 `json:"seconds_total"`
		TotalSeconds   float64 `json:"total_seconds"` // 0 when unknown (live)
		FramesDropped  int64   `json:"frames_dropped_total"`
		FFmpegRestarts int64   `json:"ffmpeg_restarts_total"`
		Xruns          int64   `json:"xruns_total"`
		LastStderr     string  `json:"ffmpeg_last_stderr"`
	} `json:"source"`

	Monitor struct {
		Enabled       bool  `json:"enabled"`
		Alive         bool  `json:"alive"`
		FramesDropped int64 `json:"frames_dropped_total"`
	} `json:"monitor"`

	Audio struct {
		Enabled        bool   `json:"enabled"`
		Live           bool   `json:"live"`
		Reason         string `json:"reason"`
		Listeners      int64  `json:"listeners"`
		ListenersTotal int64  `json:"listeners_total"`
		ChunksDropped  int64  `json:"chunks_dropped_total"`
	} `json:"audio"`

	STT struct {
		Engine       string  `json:"engine"`
		State        string  `json:"state"`
		Reconnects   int64   `json:"reconnects_total"`
		BufferDrops  int64   `json:"buffer_drops_total"`
		Segments     int64   `json:"segments_total"`
		Lines        int64   `json:"lines_total"`
		BytesSent    int64   `json:"bytes_sent_total"`
		LastError    string  `json:"last_error"`
		LastErrorAt  string  `json:"last_error_at"`
		LatencyLast  float64 `json:"latency_last_ms"`
		LatencyP50   float64 `json:"latency_p50_ms"`
		LatencyP95   float64 `json:"latency_p95_ms"`
		LatencyMax   float64 `json:"latency_max_ms"`
		LatencyCount int     `json:"latency_samples"`

		UploadLatencyLast float64 `json:"upload_latency_last_ms"`
		UploadLatencyP50  float64 `json:"upload_latency_p50_ms"`
		UploadLatencyP95  float64 `json:"upload_latency_p95_ms"`
		UploadLatencyMax  float64 `json:"upload_latency_max_ms"`

		RecognizeLatencyLast float64 `json:"recognize_latency_last_ms"`
		RecognizeLatencyP50  float64 `json:"recognize_latency_p50_ms"`
		RecognizeLatencyP95  float64 `json:"recognize_latency_p95_ms"`
		RecognizeLatencyMax  float64 `json:"recognize_latency_max_ms"`

		AssembleLatencyLast float64 `json:"assemble_latency_last_ms"`
		AssembleLatencyP50  float64 `json:"assemble_latency_p50_ms"`
		AssembleLatencyP95  float64 `json:"assemble_latency_p95_ms"`
		AssembleLatencyMax  float64 `json:"assemble_latency_max_ms"`

		PhaseLatencyCount int `json:"phase_latency_samples"`

		Pauses    int64   `json:"pauses_total"`
		PausedSec float64 `json:"paused_sec"`
	} `json:"stt"`

	Web struct {
		Clients      int64 `json:"sse_clients"`
		ClientsTotal int64 `json:"sse_clients_total"`
		Events       int64 `json:"events_total"`
		SlowDrops    int64 `json:"slow_disconnects_total"`

		ViewerLatencyLast  float64 `json:"viewer_latency_last_ms"`
		ViewerLatencyP50   float64 `json:"viewer_latency_p50_ms"`
		ViewerLatencyP95   float64 `json:"viewer_latency_p95_ms"`
		ViewerLatencyMax   float64 `json:"viewer_latency_max_ms"`
		ViewerLatencyCount int     `json:"viewer_latency_samples"`
		ViewerReports      int64   `json:"viewer_reports_total"`
	} `json:"web"`

	Transcript struct {
		Path      string `json:"path"`
		Lines     int64  `json:"lines_written"`
		Bytes     int64  `json:"bytes_written"`
		LastError string `json:"last_write_error"`
	} `json:"transcript"`

	Goroutines int `json:"goroutines"`
}

func (m *Metrics) Snapshot() Snapshot {
	// Full Lock, not RLock: stats() calls trim() on each series, which
	// mutates it. Trimming on read is what makes an idle session's window
	// actually empty out, rather than showing stale figures forever.
	m.mu.Lock()
	now := time.Now()
	capLast, capP50, capP95, capMax, capN := m.latCaption.stats(now)
	viewerLast, viewerP50, viewerP95, viewerMax, viewerN := m.latViewer.stats(now)
	uploadLast, uploadP50, uploadP95, uploadMax, uploadN := m.latUpload.stats(now)
	recognizeLast, recognizeP50, recognizeP95, recognizeMax, _ := m.latRecognize.stats(now)
	assembleLast, assembleP50, assembleP95, assembleMax, _ := m.latAssemble.stats(now)
	lastStderr, sttErr, sttErrAt := m.lastStderr, m.sttLastErr, m.sttLastErrAt
	transErr := m.transcriptErr
	processed, total := m.mediaProcessed, m.mediaTotal
	pauseStart, pausedTotal := m.sttPauseStart, m.sttPausedTotal
	lastDegradedAt := m.lastDegradedAt
	m.mu.Unlock()

	// A pause still in progress must count toward PausedSec so a long pause
	// shows live on /admin rather than only jumping once it ends.
	if !pauseStart.IsZero() {
		pausedTotal += time.Since(pauseStart)
	}

	var s Snapshot
	s.Version = m.Version
	s.SessionID = m.SessionID
	s.StartedAt = m.StartedAt
	s.UptimeSec = time.Since(m.StartedAt).Seconds()

	s.Source.Kind = m.SourceKind
	s.Source.Spec = m.SourceSpec
	s.Source.Format = m.SourceFormat
	s.Source.FramesTotal = m.framesTotal.Load()
	s.Source.BytesTotal = m.bytesTotal.Load()
	s.Source.SecondsTotal = processed.Seconds()
	s.Source.TotalSeconds = total.Seconds()
	s.Source.FramesDropped = m.framesDropped.Load()
	s.Source.FFmpegRestarts = m.ffmpegRestarts.Load()
	s.Source.Xruns = m.xruns.Load()
	s.Source.LastStderr = lastStderr

	s.Monitor.Enabled = m.MonitorEnabled
	s.Monitor.Alive = m.monitorAlive.Load()
	s.Monitor.FramesDropped = m.monitorDropped.Load()

	s.Audio.Enabled = m.AudioEnabled
	s.Audio.Live = m.audioLive.Load()
	s.Audio.Reason = m.AudioReason
	s.Audio.Listeners = m.audioListeners.Load()
	s.Audio.ListenersTotal = m.audioListenersTotal.Load()
	s.Audio.ChunksDropped = m.audioDropped.Load()

	s.STT.Engine = m.Engine
	s.STT.State = ConnState(m.sttState.Load()).String()
	s.STT.Reconnects = m.sttReconnect.Load()
	s.STT.BufferDrops = m.sttBufferDrops.Load()
	s.STT.Segments = m.sttSegments.Load()
	s.STT.Lines = m.sttLines.Load()
	s.STT.BytesSent = m.sttBytesSent.Load()
	s.STT.LastError = sttErr
	if !sttErrAt.IsZero() {
		s.STT.LastErrorAt = sttErrAt.Format(time.RFC3339)
	}
	s.STT.LatencyLast = ms(capLast)
	s.STT.LatencyP50 = ms(capP50)
	s.STT.LatencyP95 = ms(capP95)
	s.STT.LatencyMax = ms(capMax)
	s.STT.LatencyCount = capN

	s.STT.UploadLatencyLast = ms(uploadLast)
	s.STT.UploadLatencyP50 = ms(uploadP50)
	s.STT.UploadLatencyP95 = ms(uploadP95)
	s.STT.UploadLatencyMax = ms(uploadMax)

	s.STT.RecognizeLatencyLast = ms(recognizeLast)
	s.STT.RecognizeLatencyP50 = ms(recognizeP50)
	s.STT.RecognizeLatencyP95 = ms(recognizeP95)
	s.STT.RecognizeLatencyMax = ms(recognizeMax)

	s.STT.AssembleLatencyLast = ms(assembleLast)
	s.STT.AssembleLatencyP50 = ms(assembleP50)
	s.STT.AssembleLatencyP95 = ms(assembleP95)
	s.STT.AssembleLatencyMax = ms(assembleMax)

	// The three phase series are always observed together under one lock in
	// ObservePhases, so they have equal length by construction.
	s.STT.PhaseLatencyCount = uploadN

	s.STT.Pauses = m.sttPauses.Load()
	s.STT.PausedSec = pausedTotal.Seconds()

	// Health is resolved after STT.State: a closed or paused connection is
	// reported as exactly that rather than "degraded", even if a drop
	// counter ticked earlier in the session — the pause/close already
	// explains the current state, and stacking "degraded" on top of it
	// would just be noise. Only once the link is actually up (or was, and
	// nothing else claims the state) does a recent degradation event win.
	//
	// The degraded check itself is two different shapes of "not ok" ORed
	// together: lastDegradedAt is a point event (a drop, a reconnect) that
	// happened once and is only worth flagging for degradedWindow after the
	// fact, so a blip from an hour ago doesn't latch the badge forever.
	// transErr is the opposite — a condition, not an event: it's set once
	// and never cleared, so it stays true for as long as transcript writes
	// are actually failing. Aging it out on the same timer as a point event
	// would let the badge go green while the transcript diag panel right
	// below it is still showing a live write error, which is exactly the
	// kind of self-contradiction this phase exists to eliminate.
	switch {
	case s.STT.State == StateClosed.String():
		s.Health = "closed"
	case s.STT.State == StatePaused.String():
		s.Health = "paused"
	case (!lastDegradedAt.IsZero() && time.Since(lastDegradedAt) <= degradedWindow) || transErr != "":
		s.Health = "degraded"
	default:
		s.Health = "ok"
	}

	s.Web.Clients = m.sseClients.Load()
	s.Web.ClientsTotal = m.sseClientsTotal.Load()
	s.Web.Events = m.sseEvents.Load()
	s.Web.SlowDrops = m.sseSlowDrops.Load()

	s.Web.ViewerLatencyLast = ms(viewerLast)
	s.Web.ViewerLatencyP50 = ms(viewerP50)
	s.Web.ViewerLatencyP95 = ms(viewerP95)
	s.Web.ViewerLatencyMax = ms(viewerMax)
	s.Web.ViewerLatencyCount = viewerN
	s.Web.ViewerReports = m.viewerReports.Load()

	s.Transcript.Path = m.TranscriptPath
	s.Transcript.Lines = m.transcriptLine.Load()
	s.Transcript.Bytes = m.transcriptByte.Load()
	s.Transcript.LastError = transErr

	s.Goroutines = runtime.NumGoroutine()
	return s
}

// percentile returns the p-th percentile of an already-sorted slice using
// nearest-rank, which is stable and needs no interpolation for our purposes.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)))
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
