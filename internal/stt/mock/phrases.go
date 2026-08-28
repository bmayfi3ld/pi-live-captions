package mock

import (
	"strings"
	"time"

	"livecaption/internal/stt"
)

// phrase is one canned line plus who says it. Speaker follows stt.Transcript's
// own convention (1-based, 0 unknown), so phraseState can stamp it straight
// onto the Transcript it emits with no translation step.
type phrase struct {
	Speaker int
	Text    string
}

// phrases is a scripted conversation rather than a monologue, so diarization
// is exercisable offline with no API cost: an MC (1) opens, an audience
// member (2) interrupts with a question and gets a reply — a back-to-back
// 1→2→1 exchange — a guest (3) joins, and a second audience member (4)
// follows up. Deliberately caption-shaped throughout: varied length, real
// punctuation, a few proper nouns, so the viewer layout is exercised
// honestly.
var phrases = []phrase{
	{1, "Good morning everyone, and thank you all for coming out today."},
	{1, "We're going to start with a few announcements before the main session."},
	{1, "First, I want to thank the volunteers who set up the room."},
	{1, "There's coffee and tea available in the lobby throughout the morning."},
	{2, "Sorry, can you hear me okay from the back row?"},
	{1, "Yes, loud and clear — go ahead."},
	{2, "Is the schedule posted anywhere, or just announced here?"},
	{1, "It's posted at the entrance, right next to registration."},
	{1, "Let's give a warm welcome to our guest this morning, Dr. Elena Rossi."},
	{3, "Thank you so much for having me, it's wonderful to be in Portland."},
	{3, "I've been working on this project for about three years now."},
	{3, "I think you'll find the results genuinely surprising."},
	{4, "Does that apply to smaller teams too, or mainly larger ones?"},
	{3, "Great question — it scales down surprisingly well, actually."},
	{1, "We have about fifteen minutes left for questions after this."},
	{1, "If anyone needs to step out, the exits are on both sides."},
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

// phraseState drives mock's canned-utterance state machine, paced off real
// frame offsets so output is identical at any --speed and reproducible in
// tests. It also carries the current speaker, set from phrases on each
// restart, so step can stamp it onto every Transcript it emits.
type phraseState struct {
	phraseIdx int
	// speaker is the current phrase's speaker, cached at restart so step
	// doesn't need to re-index phrases on every segment.
	speaker int
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

func newPhraseState() *phraseState { return &phraseState{} }

// restart begins a fresh utterance at media time now, discarding whatever was
// in flight. step calls this both for the very first phrase and whenever a
// gap ends: the stale segment timing is by then a whole silent stretch in the
// past, so without it the first frame back would fire a finished phrase
// instantly rather than easing back in the way returning speech actually
// does. phraseIdx is left alone, so an utterance interrupted mid-gap starts
// over rather than being skipped.
func (p *phraseState) restart(now time.Duration) {
	ph := phrases[p.phraseIdx%len(phrases)]
	p.segments = splitSegments(ph.Text)
	p.speaker = ph.Speaker
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
		Words:    timeWords(seg, p.segStart, now-p.segStart),
		Speaker:  p.speaker,
		Start:    p.segStart,
		Duration: now - p.segStart,
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

// timeWords gives each word in seg an onset inside [start, start+dur), so the
// offline dev loop carries per-word timing on the same path a real provider
// does rather than leaving that field empty everywhere but production.
//
// ponytail: character-proportional and gapless — a longer word gets a longer
// slice of the segment, and nobody ever pauses mid-clause. Enough to exercise
// a pacer, not enough to judge one by. Upgrade to a syllable estimate, or
// script real pauses into phrases, if the dev loop ever needs honest prosody.
func timeWords(seg string, start, dur time.Duration) []stt.Word {
	fields := strings.Fields(seg)
	if len(fields) <= 1 || dur <= 0 {
		return stt.Untimed(seg)
	}
	chars := 0
	for _, f := range fields {
		chars += len(f)
	}
	words := make([]stt.Word, 0, len(fields))
	at := start
	for _, f := range fields {
		words = append(words, stt.Word{Text: f, Start: at})
		at += dur * time.Duration(len(f)) / time.Duration(chars)
	}
	return words
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
