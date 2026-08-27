// Package caption turns a stream of settled recognizer segments into stable
// display lines and fans them out to subscribers.
package caption

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"livecaption/internal/metrics"
	"livecaption/internal/stt"
)

// Kind distinguishes the things a subscriber can receive.
type Kind string

const (
	KindCaption  Kind = "caption"  // one settled segment, append-only
	KindStatus   Kind = "status"   // connection state changed
	KindSnapshot Kind = "snapshot" // catch-up sent to a new subscriber
)

// Event is what goes over SSE. One JSON object per message.
type Event struct {
	Seq  int64 `json:"seq"`
	Kind Kind  `json:"kind"`
	// Text is the new segment only on a caption event — never the
	// accumulated utterance — so the client can append it blindly. On a
	// snapshot event it is the whole replay flow instead: history joined
	// plus the open committed text.
	Text string `json:"text,omitempty"`
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

	// Status and snapshot events only.
	State  string `json:"state,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Line is one finalized caption: the OnFinal payload for the transcript
// writer and terminal. It never appears on the wire.
type Line struct {
	ID       string    `json:"id"`
	Text     string    `json:"text"`
	OffsetMS int64     `json:"offset_ms"`
	At       time.Time `json:"at"`
	// Speaker is the line's speaker (1-based, 0 unknown). A closed line
	// belongs to whichever speaker was talking when Publish appended its
	// text, which closeLocked has no way to see itself — see Hub.Publish.
	Speaker int
}

// historyLimit is how many finalized lines are kept for late joiners, and
// with Hub.Snapshot() gone this is also the whole replay window: a snapshot
// event is its only reader, so a second "how much to replay" constant would
// be dead weight. 20 is well more than the 6 rows a viewer shows.
const historyLimit = 20

// subscriberBuffer is how far a subscriber may fall behind before being
// dropped. Captions are realtime — a browser that cannot keep up is better off
// reconnecting and getting a fresh snapshot than receiving stale text.
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
	mu      sync.RWMutex
	seq     int64
	uttSeq  int64
	history []Line
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
	// lastState/lastDetail are the most recent status published via
	// PublishStatus. Subscribe replays them into the snapshot event so a
	// client that connects (or reconnects) mid-pause learns the current
	// state immediately, instead of waiting for a change that may never come.
	lastState, lastDetail string

	subs    map[chan Event]struct{}
	metrics *metrics.Metrics

	// OnFinal is called for each closed line, on the publishing goroutine.
	// Used for the terminal and the transcript writer.
	OnFinal func(Line)
}

func NewHub(m *metrics.Metrics) *Hub {
	return &Hub{subs: make(map[chan Event]struct{}), metrics: m}
}

// Publish feeds one settled segment into the hub. The straight-line shape
// below is deliberate: there is exactly one thing to do with a segment, not
// three branches keyed off control flags the engine no longer reports.
func (h *Hub) Publish(t stt.Transcript) {
	text := strings.TrimSpace(t.Text)
	if text == "" {
		return
	}

	h.mu.Lock()
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
	ev.Text, ev.Break = text, broke // the delta, never the accumulation
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

	if h.metrics != nil {
		h.metrics.STTSegment()
	}
	h.broadcast(ev)

	closeLine := func(l Line) {
		if h.metrics != nil {
			h.metrics.STTLine()
		}
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

	h.uttSeq++
	line := Line{
		ID:       "u" + strconv.FormatInt(h.uttSeq, 10),
		Text:     h.committed,
		OffsetMS: h.uttStart.Milliseconds(),
		At:       time.Now(),
		Speaker:  h.uttSpeaker,
	}
	h.history = append(h.history, line)
	if len(h.history) > historyLimit {
		h.history = h.history[len(h.history)-historyLimit:]
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

// PublishStatus broadcasts a connection state change.
func (h *Hub) PublishStatus(state, detail string) {
	h.mu.Lock()
	ev := h.newEventLocked(KindStatus)
	ev.State, ev.Detail = state, detail
	h.lastState, h.lastDetail = state, detail
	h.mu.Unlock()
	h.broadcast(ev)
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
	if h.metrics != nil {
		h.metrics.STTLine()
	}
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
// unsubscribe function. The first event delivered is always a snapshot.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, subscriberBuffer)

	h.mu.Lock()
	h.seq++
	snap := Event{Seq: h.seq, Kind: KindSnapshot, At: time.Now()}
	// The replay flow: history joined, plus the open committed text. The
	// client just appends it — there is no lines/pending distinction left
	// now that a caption event's Text is always a flat string too.
	snap.Text = joinHistory(h.history, h.committed)
	// Replay the last published status so a subscriber who connects (or
	// reconnects) mid-pause sees the correct indicator immediately, rather
	// than defaulting to "ok" until the state happens to change again.
	snap.State, snap.Detail = h.lastState, h.lastDetail
	// So a client reconnecting mid-session already knows who was talking,
	// and can tell the NEXT caption event's Speaker apart as an actual
	// change rather than treating it as the first speaker it has ever seen.
	snap.Speaker = h.lastSpeaker
	h.subs[ch] = struct{}{}
	h.mu.Unlock()

	ch <- snap // buffered and empty, cannot block
	if h.metrics != nil {
		h.metrics.SSEConnect()
	}

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
			close(ch)
			if h.metrics != nil {
				h.metrics.SSEDisconnect()
			}
		})
	}
}

// broadcast sends to every subscriber without ever blocking.
//
// A subscriber that cannot keep up is dropped rather than backpressured: the
// audio pipeline must never be held up by a slow browser. EventSource
// reconnects on its own and gets a fresh snapshot, so the cost of being
// dropped is one round trip.
func (h *Hub) broadcast(ev Event) {
	h.mu.RLock()
	targets := make([]chan Event, 0, len(h.subs))
	for ch := range h.subs {
		targets = append(targets, ch)
	}
	h.mu.RUnlock()

	if h.metrics != nil {
		h.metrics.SSEEvent()
	}
	for _, ch := range targets {
		select {
		case ch <- ev:
		default:
			if h.metrics != nil {
				h.metrics.SSESlowDrop()
			}
		}
	}
}

// ponytail: joinHistory flattens history to one string, so a snapshot replays
// as plain text with no per-line Speaker structure — a client reconnecting
// mid-session sees no speaker badges on the replayed text until the next
// live change. Accepted for now: fixing it means the snapshot event carrying
// history as []Line (or similar) instead of one joined string, which is a
// wire-format change the viewer side would need to match.
func joinHistory(history []Line, committed string) string {
	var b strings.Builder
	for _, l := range history {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(l.Text)
	}
	if committed != "" {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(committed)
	}
	return b.String()
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
