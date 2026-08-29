// Package caption turns a stream of settled recognizer segments into stable
// display lines and fans them out to subscribers.
package caption

import (
	"strings"
	"sync"
	"time"

	"livecaption/internal/metrics"
	"livecaption/internal/stt"
)

// Kind distinguishes the things a subscriber can receive.
type Kind string

const (
	KindCaption Kind = "caption" // one settled segment, append-only
	KindStatus  Kind = "status"  // connection state changed
	KindMusic   Kind = "music"   // music suppression toggled, carried on State as "on"/"off"
	KindClear   Kind = "clear"   // operator wiped the screen; drop everything painted
)

// Event is what goes over SSE. One JSON object per message.
type Event struct {
	Seq  int64 `json:"seq"`
	Kind Kind  `json:"kind"`
	// Words is the new segment on a caption event — never the accumulated
	// utterance — so the client can append it blindly. There is no catch-up
	// payload anywhere on the wire: a client that connects mid-session simply
	// starts from the next segment, which is what let the replay path (and
	// with it the viewer's whole second, unpaced rendering path) go away.
	Words []Word `json:"words,omitempty"`
	// Break asks the viewer to freeze the current row before appending Text:
	// the speaker actually stopped talking, for a pause of breakGap or more
	// (or a media-clock discontinuity — see isBreakLocked). It stays
	// pause-only, not speaker-change-only: a speaker change with no pause is
	// carried on Speaker instead, and it is the viewer's own rebuild() that
	// decides whether to also break the row on that — put here, Break would
	// bake a display rule into the wire format that the client has no way to
	// see or override.
	Break bool `json:"break,omitempty"`
	// Speaker is the segment's 1-based speaker (0 unknown), carried straight
	// through from stt.Transcript. The viewer derives its own row break from
	// this per-word field — see the Break comment above.
	Speaker int `json:"speaker,omitempty"`
	// omitzero, not omitempty: omitempty has no effect on a struct, so an
	// unset time would serialize as "0001-01-01T00:00:00Z".
	At time.Time `json:"at,omitzero"`

	// Status/music events only. On a music event, "on" or "off".
	State string `json:"state,omitempty"`
}

// Word is one word on the wire: its text, when the speaker began it in ms from
// its own segment's start, and how long they spent saying it.
//
// Relative rather than media time on purpose: the client never has to learn
// the media clock, and a reconnect or auto-pause resume that restarts that
// clock (the discontinuity isBreakLocked catches) cannot poison the offsets
// of a segment that arrives across it.
//
// DurMS is what lets the viewer subtract speaking time from onset-to-onset and
// be left with the silence that actually followed the word. Without it a long
// word reads as a pause after a short one — the pause landing a word early.
// Omitted (0) when the provider reported no per-word end, which the viewer
// reads as unmeasured rather than as an instantaneous word.
type Word struct {
	Text     string `json:"t"`
	OffsetMS int64  `json:"o"`
	DurMS    int64  `json:"d,omitempty"`
}

// Line is one finalized caption: the OnFinal payload for the transcript
// writer and terminal. It never appears on the wire.
type Line struct {
	Text     string
	OffsetMS int64
	At       time.Time
	// Speaker is the line's speaker (1-based, 0 unknown). A closed line
	// belongs to whichever speaker was talking when Publish appended its
	// text, which closeLocked has no way to see itself — see Hub.Publish.
	Speaker int
}

// subscriberBuffer is how far a subscriber may fall behind before being
// dropped. Captions are realtime — a browser that cannot keep up is better off
// reconnecting and picking up the live stream than receiving stale text.
const subscriberBuffer = 16

// maxUtteranceChars force-closes a transcript line if neither punctuation nor
// a speech gap ever does, so a misrecognition or an unusually long
// unpunctuated ramble can't grow a single line without bound.
const maxUtteranceChars = 1000

// breakGap is how long the audio must go quiet before it counts as the
// speaker actually stopping, rather than drawing breath. It drives both the
// viewer's row break and the transcript's fallback line break.
//
// Deliberately far above Deepgram's own endpointing window: that decides how
// big a chunk the recognizer commits to, while this decides when a pause is
// meaningful to a reader. Conflating the two is what made an earlier draft of
// this change fragment the transcript. Venue-tuned — a room with a slow
// speaker wants it longer.
const breakGap = 1500 * time.Millisecond

// Hub assembles utterances and broadcasts them.
//
// The engine only emits settled, never-revised segments (see stt.Transcript),
// so there is nothing left to diff — a segment is painted once and stays on
// screen unchanged, which is the whole point of the append-only wire format
// below. What the hub derives is structure — when a display row breaks and
// when a transcript line closes — from media-time gaps and terminal
// punctuation, since the engine itself no longer reports either.
type Hub struct {
	mu  sync.RWMutex
	seq int64
	// committed is the text of the transcript line in progress.
	committed string
	uttStart  time.Duration
	// prevEnd is the media time the last segment covered, so the gap to the
	// next one can be measured without a clock.
	prevEnd time.Duration
	// uttSpeaker is the speaker of the transcript line in progress, cached
	// whenever committed starts a fresh utterance (committed == "") so
	// closeLocked can stamp it onto the Line without needing its own
	// parameter — every segment merged into one committed line already
	// shares a speaker, since a change force-closes the line first (see
	// Publish).
	uttSpeaker int
	// lastSpeaker is the most recently published segment's speaker, used to
	// detect a speaker change independent of any pause. 0 (unknown) never
	// counts as a change either way: diarization dropping out mid-session
	// must not force a break, and a run of unknown-speaker segments must not
	// look like it's constantly changing speakers.
	lastSpeaker int
	// lastState is the most recent status published via PublishStatus.
	// Subscribe replays it as the first event a client receives, so one that
	// connects (or reconnects) mid-pause learns the current state
	// immediately, instead of waiting for a change that may never come. This
	// is now the only thing a subscriber is told about the past.
	lastState string
	// music is whether the recognizer currently reports singing. While set,
	// Publish is a no-op: sung lyrics come back as garble, and freezing the
	// screen (with the viewer told why via KindMusic) beats painting it.
	music bool
	// lastMusic mirrors music for Subscribe, the same way lastState mirrors
	// PublishStatus — a client joining mid-song needs to know the screen is
	// frozen on purpose.
	lastMusic bool

	subs    map[chan Event]struct{}
	metrics *metrics.Metrics

	// OnFinal is called for each closed line, on the publishing goroutine.
	// Used for the terminal and the transcript writer.
	OnFinal func(Line)
}

// NewHub builds a hub. m must be non-nil — every construction site has one,
// so the alternative was a nil check on every counter bump for a case that
// has never existed.
func NewHub(m *metrics.Metrics) *Hub {
	return &Hub{subs: make(map[chan Event]struct{}), metrics: m}
}

// Publish feeds one settled segment into the hub. The straight-line shape
// below is deliberate: there is exactly one thing to do with a segment, not
// three branches keyed off control flags the engine no longer reports.
func (h *Hub) Publish(t stt.Transcript) {
	text := strings.TrimSpace(t.Text())
	if text == "" {
		return
	}

	h.mu.Lock()
	// While music is playing, the recognizer's output is garble and gets
	// dropped whole — before any state mutation, so prevEnd/lastSpeaker stay
	// exactly as they were when the song started and the first segment after
	// it reads as a clean break rather than continuing a stale utterance.
	if h.music {
		h.mu.Unlock()
		return
	}
	// The break must be evaluated, and the line it closes flushed, BEFORE
	// this segment is appended — otherwise a pause lands inside the new
	// utterance instead of separating it from the old one. A speaker change
	// closes the line the same way even with no pause: 0 (unknown) on either
	// side never counts as a change, so diarization dropping out mid-segment
	// can't force a spurious break.
	broke := h.isBreakLocked(t)
	speakerChanged := h.lastSpeaker != 0 && t.Speaker != 0 && t.Speaker != h.lastSpeaker
	before, closedBefore := h.closeLocked(broke || speakerChanged) // flush what came before
	if h.committed == "" {
		h.uttStart = t.Start
		h.uttSpeaker = t.Speaker
	}
	h.committed = joinText(h.committed, text)
	h.prevEnd = t.End()

	ev := h.newEventLocked(KindCaption)
	ev.Words, ev.Break = wireWords(t), broke // the delta, never the accumulation
	ev.Speaker = t.Speaker

	// Deliberately unconditional, not gated on the flush above having been a
	// no-op: a segment arriving right after a pause can itself be a whole
	// sentence ("Good morning."), so both lines close in this one call.
	// Skipping this whenever the pause already closed something would leave
	// that sentence open until the next segment happened to arrive, delaying
	// its transcript write and terminal print by a whole utterance.
	after, closedAfter := h.closeLocked(false) // punctuation / length
	// After, not before isBreakLocked/speakerChanged above are evaluated:
	// both compare t.Speaker against the speaker as of the PREVIOUS segment.
	if t.Speaker != 0 {
		h.lastSpeaker = t.Speaker
	}
	onFinal := h.OnFinal
	h.mu.Unlock()

	h.metrics.STTSegment()
	h.broadcast(ev)

	closeLine := func(l Line) {
		h.metrics.STTLine()
		if onFinal != nil {
			onFinal(l)
		}
	}
	if closedBefore {
		closeLine(before)
	}
	if closedAfter {
		closeLine(after)
	}
}

// wireWords converts a segment's words to the wire's relative-millisecond
// form. A word that claims to start before its own segment would mean the
// provider contradicted itself; it is clamped to 0 rather than sent negative,
// so no client has to defend against an offset that runs backwards.
func wireWords(t stt.Transcript) []Word {
	words := make([]Word, 0, len(t.Words))
	for _, w := range t.Words {
		off := max(w.Start-t.Start, 0)
		// Clamped the same way and for the same reason: a provider that hands
		// back an end before its own start would otherwise put a negative
		// duration on the wire for every client to defend against.
		dur := max(w.End-w.Start, 0)
		words = append(words, Word{
			Text:     w.Text,
			OffsetMS: off.Milliseconds(),
			DurMS:    dur.Milliseconds(),
		})
	}
	return words
}

// isBreakLocked reports whether the gap between the previous segment and t
// counts as the speaker actually stopping. A negative gap means the media
// clock restarted — a reconnect or an auto-pause resume — which is a real
// discontinuity and breaks the row too, so that case falls out of the same
// comparison rather than needing its own branch.
//
// Callers must hold h.mu.
func (h *Hub) isBreakLocked(t stt.Transcript) bool {
	gap := t.Start - h.prevEnd
	return gap < 0 || gap >= breakGap
}

// closeLocked ends the open transcript line. Three signals, each suited to
// its job — and note speech_final is NOT among them: at --endpointing=100ms
// it fires on every hesitation, which would fragment transcript.txt.
//
//   - endsSentence: the semantic boundary, and what fires in normal speech.
//     Deepgram punctuates every segment (punctuate + smart_format in
//     dialURL).
//   - broke: the speaker stopped for breakGap. Covers unpunctuated speech —
//     fragments, lists, a speaker trailing off.
//   - maxUtteranceChars: the guard, if neither ever fires.
//
// Callers must hold h.mu.
func (h *Hub) closeLocked(broke bool) (Line, bool) {
	if h.committed == "" {
		return Line{}, false
	}
	if !broke && !endsSentence(h.committed) && len(h.committed) < maxUtteranceChars {
		return Line{}, false
	}

	line := Line{
		Text:     h.committed,
		OffsetMS: h.uttStart.Milliseconds(),
		At:       time.Now(),
		Speaker:  h.uttSpeaker,
	}
	h.committed = ""
	return line, true
}

// endsSentence reports whether s ends with terminal sentence punctuation.
func endsSentence(s string) bool {
	if s == "" {
		return false
	}
	switch s[len(s)-1] {
	case '.', '?', '!':
		return true
	default:
		return false
	}
}

// SetMusic toggles caption suppression on a music start/end edge from the
// recognizer. On true, the in-progress transcript line is closed through the
// same OnFinal path Flush uses, so the sentence spoken right before the song
// lands in transcript.txt instead of being glued to whatever follows the
// music.
func (h *Hub) SetMusic(active bool) {
	h.mu.Lock()
	if h.music == active {
		// The engine can only send edges, but this is cheap insurance
		// against a redundant one being treated as a fresh state change.
		h.mu.Unlock()
		return
	}
	h.music = active
	h.lastMusic = active

	var line Line
	var closed bool
	if active {
		line, closed = h.closeLocked(true)
	}
	ev := h.newEventLocked(KindMusic)
	if active {
		ev.State = "on"
	} else {
		ev.State = "off"
	}
	onFinal := h.OnFinal
	h.mu.Unlock()

	h.broadcast(ev)
	if closed {
		h.metrics.STTLine()
		if onFinal != nil {
			onFinal(line)
		}
	}
}

// PublishStatus broadcasts a connection state change.
func (h *Hub) PublishStatus(state string) {
	h.mu.Lock()
	ev := h.newEventLocked(KindStatus)
	ev.State = state
	h.lastState = state
	h.mu.Unlock()
	h.broadcast(ev)
}

// Clear tells every viewer to wipe what it has painted — the operator saw
// something on screen that shouldn't stay there.
//
// The in-progress transcript line is closed the way Flush does it, for two
// reasons: what was on screen still belongs in transcript.txt, and the next
// segment must start a fresh utterance rather than being glued onto text the
// viewers no longer show.
//
// Not replayed in Subscribe: a clear is an edge, not a state. A viewer that
// connects afterwards has nothing painted to wipe.
func (h *Hub) Clear() {
	h.mu.Lock()
	line, closed := h.closeLocked(true)
	ev := h.newEventLocked(KindClear)
	onFinal := h.OnFinal
	h.mu.Unlock()

	h.broadcast(ev)
	if closed {
		h.metrics.STTLine()
		if onFinal != nil {
			onFinal(line)
		}
	}
}

// Flush closes any in-progress utterance. Called at shutdown so the tail of a
// session is not lost when the speaker was still talking.
func (h *Hub) Flush() {
	h.mu.Lock()
	line, closed := h.closeLocked(true)
	onFinal := h.OnFinal
	h.mu.Unlock()
	if !closed {
		return
	}
	h.metrics.STTLine()
	if onFinal != nil {
		onFinal(line)
	}
}

// newEventLocked stamps every event with a publish instant. At is now
// unconditional on every event kind, not just some — a viewer measures its
// own publish->paint latency off it regardless of which kind taught it about
// the text.
func (h *Hub) newEventLocked(k Kind) Event {
	h.seq++
	return Event{Seq: h.seq, Kind: k, At: time.Now()}
}

// Subscribe registers a new listener and returns its channel plus an
// unsubscribe function.
//
// The first event delivered is a status carrying the last published state, so
// a client that connects (or reconnects) mid-pause shows the right indicator
// immediately rather than defaulting to "ok" until the state happens to change
// again — which, during a pause, may be never. It is a plain status event
// rather than a kind of its own: with no catch-up text left to send, a
// dedicated "snapshot" kind would have carried nothing a status doesn't.
//
// No caption history follows it. A late joiner starts from the next segment.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, subscriberBuffer)

	h.mu.Lock()
	ev := h.newEventLocked(KindStatus)
	ev.State = h.lastState
	var musicEv Event
	if h.lastMusic {
		musicEv = h.newEventLocked(KindMusic)
		musicEv.State = "on"
	}
	h.subs[ch] = struct{}{}
	h.mu.Unlock()

	ch <- ev // buffered and empty, cannot block
	// A viewer joining mid-song needs to know why the screen is frozen —
	// pushed after the status event it already replays, same buffered
	// channel, so it also cannot block.
	if h.lastMusic {
		ch <- musicEv
	}
	h.metrics.SSEConnect()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
			close(ch)
			h.metrics.SSEDisconnect()
		})
	}
}

// broadcast sends to every subscriber without ever blocking.
//
// A subscriber that cannot keep up is dropped rather than backpressured: the
// audio pipeline must never be held up by a slow browser. EventSource
// reconnects on its own, so the cost of being dropped is one round trip plus
// whatever was said during it.
func (h *Hub) broadcast(ev Event) {
	h.mu.RLock()
	targets := make([]chan Event, 0, len(h.subs))
	for ch := range h.subs {
		targets = append(targets, ch)
	}
	h.mu.RUnlock()

	h.metrics.SSEEvent()
	for _, ch := range targets {
		select {
		case ch <- ev:
		default:
			h.metrics.SSESlowDrop()
		}
	}
}

func joinText(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + " " + b
	}
}
