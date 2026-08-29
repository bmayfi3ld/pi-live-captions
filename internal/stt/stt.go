// Package stt defines the speech-to-text abstraction and the machinery every
// streaming provider needs around it: reconnect backoff, the silence gate, a
// bounded audio buffer, and latency anchoring.
//
// Adding a provider means writing a Dialer and a Session for its protocol,
// handing them to RunSession, and adding a case to newEngine in internal/cli.
// Everything that is not protocol already lives here.
package stt

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"livecaption/internal/audio"
	"livecaption/internal/metrics"
)

// Transcript is one settled segment of speech: the engine only emits text it
// will not revise, so this is pure observation — what was heard and when —
// with no control flags left for the hub to interpret. Structure (row breaks,
// transcript line breaks) is derived downstream from Start/Duration and
// punctuation, not reported here.
type Transcript struct {
	// Words is the segment as heard, in order, and the only place its text
	// lives — Text() joins them back. Never empty on a published transcript:
	// a provider with no per-word timing reports the whole segment as a
	// single Word (see Untimed), so every consumer downstream has exactly one
	// shape to handle.
	Words []Word
	// Speaker is 1-based; 0 means unknown (diarization off, or the provider
	// didn't attribute this segment). Every provider labels speakers its own
	// way — Deepgram hands back a 0-based int, Speechmatics a string like
	// "S1" or "UU" — and normalising to an int here, in the engine, keeps
	// that provider-specific syntax out of the hub and off the wire.
	Speaker int
	// Start and Duration are media time, so latency can be measured the same
	// way for a replayed file and a live capture.
	Start      time.Duration
	Duration   time.Duration
	ReceivedAt time.Time
	// CapturedAt is when the audio this transcript covers was released into the
	// pipeline. Zero when the engine could not resolve it; latency is then simply
	// not recorded, because a wrong number is worse than no number.
	CapturedAt time.Time
	// SentAt is when the audio this transcript covers was released to the
	// recognizer's socket. Together with CapturedAt and ReceivedAt it splits
	// total latency into upload / recognition phases. Zero when the engine
	// could not resolve it, in which case the split is simply not recorded.
	SentAt time.Time
}

// Word is one word as the recognizer heard it: its text, and when it began and
// ended, on the same media clock as Transcript.Start. This is the segment's
// measured prosody — where the speaker actually paused and how fast they were
// going — and it keeps its shape all the way to the browser rather than being
// flattened into a string at any hop.
//
// End is what separates a drawn-out word from a short word followed by a
// pause: onset-to-onset alone conflates the two, and a pacer that treats the
// whole of it as dwell paints the word instantly and then sits through the
// time the speaker spent saying it — a phantom pause landing one word early.
// Zero when the provider reported no end (the Untimed case), which downstream
// reads as "unmeasured", not as a zero-length word.
type Word struct {
	Text  string
	Start time.Duration
	End   time.Duration
}

// Untimed is the whole segment as one Word, for a provider (or a test) with
// no per-word detail to report. Text() reproduces the segment exactly, and a
// pacer simply reveals it in one go — the honest behaviour when nothing finer
// was measured.
func Untimed(text string) []Word { return []Word{{Text: text}} }

// Text is the segment's text: the words joined with single spaces. Providers
// build Words so this reproduces exactly what the recognizer sent.
func (t Transcript) Text() string {
	switch len(t.Words) {
	case 0:
		return ""
	case 1:
		return t.Words[0].Text // the Untimed case, no join to do
	}
	var b strings.Builder
	for i, w := range t.Words {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(w.Text)
	}
	return b.String()
}

// End is the media time of the last sample this transcript covers.
func (t Transcript) End() time.Duration { return t.Start + t.Duration }

// Config is what every engine needs to know to start recognizing.
type Config struct {
	Format   audio.Format
	Model    string
	Language string
	Keyterms []string // event-specific proper nouns
	APIKey   string
	Metrics  *metrics.Metrics
	Pause    PauseConfig
	// Diarize asks the provider to attribute segments to speakers.
	Diarize bool
	// MusicDetect asks the provider for music audio-events (Speechmatics
	// only; Deepgram has no equivalent and ignores this).
	MusicDetect bool
	// OnMusic is called on each music start/end edge the provider reports.
	// Nil-safe at every call site — only Speechmatics ever calls it.
	//
	// at is the edge's media time: the event's start_time on a start, its
	// end_time on an end. Without it a consumer can only suppress by the order
	// messages happened to arrive in, and a provider whose music detector needs
	// trailing context reports the end AFTER the finals covering the first
	// words of returning speech — which is how the first word went missing.
	OnMusic func(active bool, at time.Duration)
}

// CapKeyterms trims a keyterm list to what the provider will accept. Deepgram
// rejects the whole request past its budget; Speechmatics documents a ceiling
// but not what it does with a list that exceeds it, and finding out mid-event
// is not the plan. Both get cut client-side.
//
// The cut is from the tail on purpose: keyterm lists are written
// most-likely-spoken first, so the terms that survive are the ones that earn
// their slot.
func CapKeyterms(terms []string, max int, log *slog.Logger) []string {
	if len(terms) <= max {
		return terms
	}
	log.Warn("keyterm list truncated to the provider's limit",
		"given", len(terms), "sent", max, "dropped", len(terms)-max)
	return terms[:max]
}

// Engine consumes PCM frames and emits transcripts.
//
// Implementations own their own reconnect logic: Run should return only when
// ctx is cancelled or the frames channel closes, not on a dropped connection.
// Run must never block indefinitely on the frames channel — a slow engine must
// drop audio rather than stall capture.
type Engine interface {
	Name() string
	Run(ctx context.Context, frames <-chan audio.Frame, out chan<- Transcript) error
}
