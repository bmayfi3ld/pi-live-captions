package mock

import (
	"math/rand"
	"strings"
	"time"

	"livecaption/internal/stt"
)

// phrases are deliberately caption-shaped: varied length, real punctuation,
// and a few proper nouns, so the viewer layout is exercised honestly.
var phrases = []string{
	"Good morning everyone, and thank you all for coming out today.",
	"We're going to start with a few announcements before the main session.",
	"If you can't hear me at the back, please raise your hand.",
	"The agenda for this morning is posted at the entrance.",
	"First, I want to thank the volunteers who set up the room.",
	"We'll take a short break at around half past ten.",
	"Please silence your phones for the duration of the talk.",
	"There's coffee and tea available in the lobby throughout the morning.",
	"Our first speaker has been working on this project for three years.",
	"I think you'll find the results genuinely surprising.",
	"Let's give a warm welcome to our guest this morning.",
	"Feel free to ask questions as we go along.",
	"That brings us to the end of the first section.",
	"We have about fifteen minutes left for discussion.",
	"If anyone needs to step out, the exits are on both sides.",
}

const (
	// Minimum media time between one segment's emission and the next, so
	// segments arrive paced like a real recognizer's chunks rather than all
	// at once.
	segmentAudio = 400 * time.Millisecond
	// Silence between utterances. Must clear --speech-break's 1.5s default
	// so the mock actually produces the pause break the real display relies
	// on — the dev loop should show the same row behaviour a real room will.
	gapAudio = 2 * time.Second
)

// phraseState drives the canned-utterance state machine shared by both mock
// engines: mock paces it off real frame offsets, mock-2 off a synthetic
// level schedule instead, but the phrase timing, segment split and gaps are
// identical so the two engines look the same to a viewer.
type phraseState struct {
	rng *rand.Rand

	phraseIdx int
	// segments is the current phrase split into clause-sized, settled
	// pieces — computed once per utterance, since a real recognizer doesn't
	// re-segment text it has already committed to.
	segments []string
	segIdx   int
	// segStart is the media time the pending segment's audio began, so its
	// emitted Duration reflects that segment alone rather than the whole
	// utterance so far.
	segStart    time.Duration
	nextSegment time.Duration
	inGap       bool
	gapUntil    time.Duration
}

func newPhraseState(rng *rand.Rand) *phraseState {
	return &phraseState{rng: rng}
}

// restart begins a fresh utterance at media time now, discarding whatever was
// in flight. mock-2 calls this on resume from a pause: the stale segment
// timing is by then a whole silent stretch in the past, so without it the
// first frame back would fire a finished phrase instantly rather than easing
// back in the way returning speech actually does. phraseIdx is left alone, so
// the interrupted sentence starts over rather than being skipped.
func (p *phraseState) restart(now time.Duration) {
	p.segments = splitSegments(phrases[p.phraseIdx%len(phrases)])
	p.segIdx = 0
	p.segStart = now
	p.nextSegment = now + segmentAudio
	p.inGap = false
}

// step advances the state machine to media time now, emitting one settled
// segment through emit whenever its scheduled slot arrives. It reports false
// only when emit says the caller should stop (ctx cancelled); a step with
// nothing to emit yet still reports true.
func (p *phraseState) step(now time.Duration, emit func(stt.Transcript) bool) bool {
	if p.inGap {
		if now < p.gapUntil {
			return true
		}
		p.inGap = false
		p.restart(now)
	}
	if p.segments == nil {
		p.restart(now)
	}
	if now < p.nextSegment {
		return true
	}

	seg := p.segments[p.segIdx]
	if !emit(stt.Transcript{
		Text:       seg,
		Start:      p.segStart,
		Duration:   now - p.segStart,
		Confidence: 0.90 + p.rng.Float64()*0.09,
	}) {
		return false
	}

	p.segIdx++
	if p.segIdx >= len(p.segments) {
		p.phraseIdx++
		p.inGap = true
		p.gapUntil = now + gapAudio
		return true
	}
	p.segStart = now
	p.nextSegment = now + segmentAudio
	return true
}

// splitSegments divides one phrase into clause-sized pieces the way endpointing
// against real speech would: at ", " where the phrase has it, or into 2-3
// roughly equal word runs otherwise. Only the last piece carries the phrase's
// terminal punctuation, which needs no special handling — it's already
// attached to the last word since nothing strips it.
func splitSegments(phrase string) []string {
	if parts := strings.Split(phrase, ", "); len(parts) > 1 {
		for i := 0; i < len(parts)-1; i++ {
			parts[i] += ","
		}
		return parts
	}

	words := strings.Fields(phrase)
	n := 2
	if len(words) >= 9 {
		n = 3
	}
	if n > len(words) {
		n = len(words)
	}
	segments := make([]string, 0, n)
	base, extra := len(words)/n, len(words)%n
	idx := 0
	for i := 0; i < n; i++ {
		size := base
		if i < extra {
			size++
		}
		segments = append(segments, strings.Join(words[idx:idx+size], " "))
		idx += size
	}
	return segments
}
