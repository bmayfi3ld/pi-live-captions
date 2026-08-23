package deepgram

import (
	"testing"
	"time"

	"livecaption/internal/stt"
)

func interim(text string, start, dur time.Duration) stt.Transcript {
	return stt.Transcript{Text: text, Start: start, Duration: dur, Confidence: 0.9}
}

func TestPrefixTracker_FirstInterimPublishesNothing(t *testing.T) {
	p := newPrefixTracker()
	_, ok := p.update(interim("hello there", 0, 400*time.Millisecond), false)
	if ok {
		t.Fatal("a lone interim has nothing to agree with yet, expected no publish")
	}
}

// TestPrefixTracker_StablePrefixMinusHoldback is the core behavior: once two
// consecutive interims agree on a prefix, everything but the trailing
// holdback is safe to publish, and Start/Duration tile the window.
func TestPrefixTracker_StablePrefixMinusHoldback(t *testing.T) {
	p := newPrefixTracker()

	// Round 1: nothing to compare against yet.
	if _, ok := p.update(interim("the quick brown", 0, 300*time.Millisecond), false); ok {
		t.Fatal("round 1: expected no publish")
	}

	// Round 2: "the quick brown" agrees fully with round 1 (3 tokens), minus
	// holdbackTokens(2) leaves 1 token safe to publish: "the".
	got, ok := p.update(interim("the quick brown fox", 0, 500*time.Millisecond), false)
	if !ok {
		t.Fatal("round 2: expected a publish")
	}
	if got.Text != "the" {
		t.Errorf("round 2 text = %q, want %q", got.Text, "the")
	}
	if got.Start != 0 {
		t.Errorf("round 2 Start = %v, want 0", got.Start)
	}
	if got.Duration != 500*time.Millisecond {
		t.Errorf("round 2 Duration = %v, want %v", got.Duration, 500*time.Millisecond)
	}

	// Round 3: "the quick brown fox" agrees fully with round 2 (4 tokens),
	// minus holdback(2) leaves 2 tokens safe; 1 already emitted, so "quick"
	// is the new publish, picking up where round 2's End left off.
	got, ok = p.update(interim("the quick brown fox jumps", 0, 700*time.Millisecond), false)
	if !ok {
		t.Fatal("round 3: expected a publish")
	}
	if got.Text != "quick" {
		t.Errorf("round 3 text = %q, want %q", got.Text, "quick")
	}
	if got.Start != 500*time.Millisecond {
		t.Errorf("round 3 Start = %v, want %v (round 2's End)", got.Start, 500*time.Millisecond)
	}
}

// TestPrefixTracker_RevisedTailNeverPublishedEarly checks that a word only in
// the holdback window is never leaked out just because two interims
// nominally "agreed" on a longer prefix that hasn't cleared the holdback.
func TestPrefixTracker_RevisedTailNeverPublishedEarly(t *testing.T) {
	p := newPrefixTracker()
	p.update(interim("hi", 0, 100*time.Millisecond), false)
	got, ok := p.update(interim("hi there", 0, 200*time.Millisecond), false)
	// prefix agreement is 1 token ("hi"), minus holdback(2) is negative:
	// nothing published, since even "hi" itself is within the holdback zone.
	if ok {
		t.Fatalf("expected no publish while agreement is within the holdback window, got %+v", got)
	}
}

// TestPrefixTracker_FinalFlushesRemainderAndResets checks that is_final
// publishes everything left over regardless of holdback or agreement, and
// that the tracker is clean for the next window afterward.
func TestPrefixTracker_FinalFlushesRemainderAndResets(t *testing.T) {
	p := newPrefixTracker()
	p.update(interim("good morning everyone", 0, 300*time.Millisecond), false)
	// prefix agreement with round 1 is 3 tokens, minus holdback(2) publishes 1: "good".
	p.update(interim("good morning everyone today", 0, 500*time.Millisecond), false)

	got, ok := p.update(interim("good morning everyone today", 0, 600*time.Millisecond), true)
	if !ok {
		t.Fatal("is_final: expected the remainder to be flushed")
	}
	if got.Text != "morning everyone today" {
		t.Errorf("final flush text = %q, want %q", got.Text, "morning everyone today")
	}
	if got.Duration != 100*time.Millisecond {
		t.Errorf("final flush Duration = %v, want %v (600ms - 500ms)", got.Duration, 100*time.Millisecond)
	}

	// A fresh window: prior emitted/prevTokens must not leak in.
	if _, ok := p.update(interim("next window", 2, 200*time.Millisecond), false); ok {
		t.Fatal("first interim of a new window: expected no publish (nothing to agree with yet)")
	}
	got, ok = p.update(interim("next window here", 2, 300*time.Millisecond), true)
	if !ok {
		t.Fatal("new window final: expected a publish")
	}
	if got.Text != "next window here" {
		t.Errorf("new window final text = %q, want %q", got.Text, "next window here")
	}
	if got.Start != 2 {
		t.Errorf("new window final Start = %v, want the new window's own Start (2)", got.Start)
	}
}

// TestPrefixTracker_SingleFinalNoPriorInterim covers an utterance so short
// Deepgram never sends an interim for it: the final must still publish in
// full.
func TestPrefixTracker_SingleFinalNoPriorInterim(t *testing.T) {
	p := newPrefixTracker()
	got, ok := p.update(interim("hello world", 0, time.Second), true)
	if !ok {
		t.Fatal("expected a publish")
	}
	if got.Text != "hello world" {
		t.Errorf("text = %q, want %q", got.Text, "hello world")
	}
	if got.Start != 0 || got.Duration != time.Second {
		t.Errorf("Start/Duration = %v/%v, want 0/%v", got.Start, got.Duration, time.Second)
	}
}
