package caption

import (
	"strings"
	"testing"
	"time"

	"livecaption/internal/metrics"
	"livecaption/internal/stt"
)

// testBreakGap is a deliberately explicit gap, not the package default, so
// these tests are immune to that default changing and so the boundary tests
// can pick round numbers on either side of it.
const testBreakGap = 500 * time.Millisecond

func newTestHub() *Hub {
	return NewHub(metrics.New("test", "test"), testBreakGap)
}

func drain(ch <-chan Event) []Event {
	var out []Event
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
		default:
			return out
		}
	}
}

// TestUtteranceAssembly covers the basic shape of the new pipeline: every
// segment delivers as its own delta on the wire, never the accumulation, and
// only a closed line reaches OnFinal.
func TestUtteranceAssembly(t *testing.T) {
	h := newTestHub()
	var finals []Line
	h.OnFinal = func(l Line) { finals = append(finals, l) }

	sub, unsub := h.Subscribe()
	defer unsub()
	drain(sub) // discard the initial snapshot

	h.Publish(stt.Transcript{Text: "hello there,", Start: time.Second, Duration: 500 * time.Millisecond})
	h.Publish(stt.Transcript{Text: "how are you?", Start: 1500 * time.Millisecond, Duration: 500 * time.Millisecond})

	if len(finals) != 1 {
		t.Fatalf("expected 1 finalized line, got %d: %v", len(finals), finals)
	}
	if want := "hello there, how are you?"; finals[0].Text != want {
		t.Errorf("finalized text = %q, want %q", finals[0].Text, want)
	}
	// The line is stamped with where the utterance began, not where it ended.
	if finals[0].OffsetMS != 1000 {
		t.Errorf("offset = %dms, want 1000ms", finals[0].OffsetMS)
	}

	events := drain(sub)
	var captions, texts []string
	for _, ev := range events {
		if ev.Kind == KindCaption {
			captions = append(captions, ev.Text)
			texts = append(texts, ev.Text)
		}
	}
	if len(captions) != 2 {
		t.Fatalf("expected 2 caption events (one per segment), got %d: %v", len(captions), texts)
	}
	// Each caption event's Text is the delta, never the accumulation.
	if captions[0] != "hello there," {
		t.Errorf("first caption Text = %q, want the delta %q", captions[0], "hello there,")
	}
	if captions[1] != "how are you?" {
		t.Errorf("second caption Text = %q, want the delta %q", captions[1], "how are you?")
	}
}

// TestPunctuationClosesLine is the primary transcript path: a segment ending
// in terminal punctuation closes the line right there, without waiting for a
// gap.
func TestPunctuationClosesLine(t *testing.T) {
	h := newTestHub()
	var finals []Line
	h.OnFinal = func(l Line) { finals = append(finals, l) }

	h.Publish(stt.Transcript{Text: "that's the whole sentence.", Start: time.Second})
	if len(finals) != 1 {
		t.Fatalf("expected punctuation to close the line immediately, got %d finals", len(finals))
	}
	if finals[0].Text != "that's the whole sentence." {
		t.Errorf("finalized text = %q", finals[0].Text)
	}
}

// TestPauseAndPunctuationBothClose pins the case where a single Publish has to
// close two lines: the pause ends the utterance already in progress, and the
// segment arriving after it is itself a complete sentence. An earlier version
// ran the punctuation check only when the pause had closed nothing, which left
// the new sentence open until some later segment happened to arrive — its
// transcript write and terminal print delayed by a whole utterance.
func TestPauseAndPunctuationBothClose(t *testing.T) {
	h := newTestHub()
	var finals []Line
	h.OnFinal = func(l Line) { finals = append(finals, l) }

	// Left open: no terminal punctuation, no gap yet.
	h.Publish(stt.Transcript{Text: "we will begin in a moment", Start: 0, Duration: time.Second})
	// A gap well past testBreakGap, then a one-segment complete sentence.
	h.Publish(stt.Transcript{Text: "Good morning.", Start: 3 * time.Second, Duration: time.Second})

	if len(finals) != 2 {
		t.Fatalf("closed %d lines, want 2 (the pause closes the first, the period the second)", len(finals))
	}
	if finals[0].Text != "we will begin in a moment" {
		t.Errorf("first line = %q", finals[0].Text)
	}
	if finals[1].Text != "Good morning." {
		t.Errorf("second line = %q", finals[1].Text)
	}
}

// TestSpeechGapClosesUnpunctuatedLine proves the gap fallback: a list read
// aloud with pauses but no terminal punctuation still yields transcript
// lines, since a reader watching later still needs sentence-shaped output.
func TestSpeechGapClosesUnpunctuatedLine(t *testing.T) {
	h := newTestHub()
	var finals []Line
	h.OnFinal = func(l Line) { finals = append(finals, l) }

	h.Publish(stt.Transcript{Text: "item one", Start: 0})
	if len(finals) != 0 {
		t.Fatalf("expected no line closed yet, got %d", len(finals))
	}

	// A gap comfortably past testBreakGap.
	h.Publish(stt.Transcript{Text: "item two", Start: 2 * time.Second})

	if len(finals) != 1 {
		t.Fatalf("expected the speech gap to close the unpunctuated line, got %d finals", len(finals))
	}
	if finals[0].Text != "item one" {
		t.Errorf("finalized text = %q, want %q", finals[0].Text, "item one")
	}
}

// TestSpeechGapSetsBreakFlag and TestSmallGapDoesNotBreak pin the breakGap
// boundary on both sides, driven by a hub with an explicit gap rather than
// the package default.
func TestSpeechGapSetsBreakFlag(t *testing.T) {
	h := newTestHub()
	sub, unsub := h.Subscribe()
	defer unsub()
	drain(sub)

	h.Publish(stt.Transcript{Text: "first", Start: 0, Duration: 100 * time.Millisecond})
	drain(sub)

	// Gap of exactly testBreakGap: isBreakLocked uses >=, so this must break.
	h.Publish(stt.Transcript{Text: "second", Start: 100*time.Millisecond + testBreakGap})

	events := drain(sub)
	found := false
	for _, ev := range events {
		if ev.Kind == KindCaption && ev.Text == "second" {
			found = true
			if !ev.Break {
				t.Errorf("caption event for %q: Break = false, want true (gap == breakGap)", ev.Text)
			}
		}
	}
	if !found {
		t.Fatal("expected a caption event for the second segment")
	}
}

func TestSmallGapDoesNotBreak(t *testing.T) {
	h := newTestHub()
	sub, unsub := h.Subscribe()
	defer unsub()
	drain(sub)

	h.Publish(stt.Transcript{Text: "first", Start: 0, Duration: 100 * time.Millisecond})
	drain(sub)

	// Comfortably under testBreakGap.
	h.Publish(stt.Transcript{Text: "second", Start: 200 * time.Millisecond})

	events := drain(sub)
	found := false
	for _, ev := range events {
		if ev.Kind == KindCaption && ev.Text == "second" {
			found = true
			if ev.Break {
				t.Errorf("caption event for %q: Break = true, want false (gap well under breakGap)", ev.Text)
			}
		}
	}
	if !found {
		t.Fatal("expected a caption event for the second segment")
	}
}

// TestNegativeGapBreaks covers a reconnect or auto-pause resume restarting
// the media clock: Start goes backwards relative to the last segment's end,
// which is a real discontinuity and must break the row even though the
// arithmetic gap is nowhere near breakGap.
func TestNegativeGapBreaks(t *testing.T) {
	h := newTestHub()
	var finals []Line
	h.OnFinal = func(l Line) { finals = append(finals, l) }
	sub, unsub := h.Subscribe()
	defer unsub()
	drain(sub)

	h.Publish(stt.Transcript{Text: "before the restart", Start: 10 * time.Second, Duration: time.Second})
	drain(sub)
	if len(finals) != 0 {
		t.Fatalf("expected no line closed yet, got %d", len(finals))
	}

	// A fresh connection's media clock restarts at (or near) 0.
	h.Publish(stt.Transcript{Text: "after the restart", Start: 0})

	if len(finals) != 1 {
		t.Fatalf("expected the clock restart to close the pending line, got %d finals", len(finals))
	}
	if finals[0].Text != "before the restart" {
		t.Errorf("finalized text = %q, want %q", finals[0].Text, "before the restart")
	}

	events := drain(sub)
	found := false
	for _, ev := range events {
		if ev.Kind == KindCaption && ev.Text == "after the restart" {
			found = true
			if !ev.Break {
				t.Error("expected Break = true for a negative gap (media clock restart)")
			}
		}
	}
	if !found {
		t.Fatal("expected a caption event for the post-restart segment")
	}
}

// TestOverlongUtteranceIsForceClosed guards the runaway guard: if neither
// punctuation nor a speech gap ever fires, maxUtteranceChars forces the line
// closed so a misrecognition can't grow one line without bound.
func TestOverlongUtteranceIsForceClosed(t *testing.T) {
	h := newTestHub()
	var finals []Line
	h.OnFinal = func(l Line) { finals = append(finals, l) }

	// Unpunctuated words, no gaps, until the accumulated text crosses
	// maxUtteranceChars.
	start := time.Duration(0)
	for len(finals) == 0 {
		h.Publish(stt.Transcript{Text: "word", Start: start})
		start += 10 * time.Millisecond
		if start > 10*time.Second {
			t.Fatal("maxUtteranceChars guard never fired")
		}
	}
	if len(finals[0].Text) < maxUtteranceChars {
		t.Errorf("force-closed line is %d chars, want >= maxUtteranceChars (%d)", len(finals[0].Text), maxUtteranceChars)
	}
}

// TestFlushClosesPendingUtterance ensures the tail of a session is not lost
// when shutdown lands mid-sentence.
func TestFlushClosesPendingUtterance(t *testing.T) {
	h := newTestHub()
	var finals []Line
	h.OnFinal = func(l Line) { finals = append(finals, l) }

	h.Publish(stt.Transcript{Text: "an unfinished sentence", Start: 0})
	h.Flush()

	if len(finals) != 1 {
		t.Fatalf("expected Flush to close the pending line, got %d finals", len(finals))
	}
	if finals[0].Text != "an unfinished sentence" {
		t.Errorf("flushed text = %q", finals[0].Text)
	}

	// Flushing again must not emit a duplicate empty line.
	h.Flush()
	if len(finals) != 1 {
		t.Errorf("second Flush produced %d finals, want 1", len(finals))
	}
}

// TestSlowSubscriberIsDropped is the rule that keeps the audio pipeline safe:
// a browser that cannot keep up loses events rather than applying backpressure
// all the way back to capture.
func TestSlowSubscriberIsDropped(t *testing.T) {
	m := metrics.New("test", "test")
	h := NewHub(m, testBreakGap)

	sub, unsub := h.Subscribe()
	defer unsub()
	// Deliberately never read from sub.

	start := time.Duration(0)
	for i := 0; i < subscriberBuffer*3; i++ {
		h.Publish(stt.Transcript{Text: "line.", Start: start})
		start += 10 * time.Millisecond
	}

	snap := m.Snapshot()
	if snap.Web.SlowDrops == 0 {
		t.Error("expected slow-subscriber drops to be counted")
	}
	if len(sub) > subscriberBuffer {
		t.Errorf("subscriber buffer grew to %d, want <= %d", len(sub), subscriberBuffer)
	}
	// Publishing must still have completed: the pipeline was never blocked.
	if snap.STT.Lines != int64(subscriberBuffer*3) {
		t.Errorf("published %d lines, want %d", snap.STT.Lines, subscriberBuffer*3)
	}
}

// TestSnapshotForLateJoiner covers the browser-refresh case: a page that
// connects mid-session must not be blank. The snapshot's Text is the replay
// flow — history joined plus the open committed text — since a caption
// event's Text is a flat delta too, with no lines/pending distinction left.
func TestSnapshotForLateJoiner(t *testing.T) {
	h := newTestHub()
	h.Publish(stt.Transcript{Text: "earlier line.", Start: 0})
	h.Publish(stt.Transcript{Text: "in progress", Start: 5 * time.Second})

	sub, unsub := h.Subscribe()
	defer unsub()

	ev := <-sub
	if ev.Kind != KindSnapshot {
		t.Fatalf("first event kind = %q, want snapshot", ev.Kind)
	}
	if want := "earlier line. in progress"; ev.Text != want {
		t.Errorf("snapshot text = %q, want %q", ev.Text, want)
	}
}

// TestCaptionEventCarriesAt guards against caption and status events shipping
// with a zero At: a viewer needs At on every event to measure its own
// publish->paint latency, since a caption event is what it sees first.
func TestCaptionEventCarriesAt(t *testing.T) {
	h := newTestHub()
	sub, unsub := h.Subscribe()
	defer unsub()
	drain(sub) // discard the initial snapshot

	h.Publish(stt.Transcript{Text: "in progress", Start: time.Second})

	events := drain(sub)
	var found bool
	for _, ev := range events {
		if ev.Kind != KindCaption {
			continue
		}
		found = true
		if ev.At.IsZero() {
			t.Errorf("caption event At is zero, want a publish instant")
		}
	}
	if !found {
		t.Fatal("expected at least one caption event")
	}
}

// TestHistoryIsBounded stops a long session from growing memory without
// limit. Each publish is its own punctuated sentence so every one closes a
// line immediately.
func TestHistoryIsBounded(t *testing.T) {
	h := newTestHub()
	start := time.Duration(0)
	for i := 0; i < historyLimit+50; i++ {
		h.Publish(stt.Transcript{Text: "line.", Start: start})
		start += 10 * time.Millisecond
	}
	if len(h.history) != historyLimit {
		t.Errorf("history = %d lines, want %d", len(h.history), historyLimit)
	}
}

// TestBreakClosesLineBeforeSegmentIsAppended is the subtlest behavior in the
// hub: the break must be evaluated and the previous line closed BEFORE the
// new segment is appended, so a pause separates two utterances rather than
// landing inside one. This is checked directly against the closed line's
// text, not just the Break flag, since a wrong implementation could set
// Break correctly while still having merged the text.
func TestBreakClosesLineBeforeSegmentIsAppended(t *testing.T) {
	h := newTestHub()
	var finals []Line
	h.OnFinal = func(l Line) { finals = append(finals, l) }

	h.Publish(stt.Transcript{Text: "before the pause", Start: 0, Duration: 100 * time.Millisecond})
	h.Publish(stt.Transcript{Text: "after the pause", Start: 100*time.Millisecond + testBreakGap})

	if len(finals) != 1 {
		t.Fatalf("expected the pause to close exactly one line, got %d", len(finals))
	}
	if strings.Contains(finals[0].Text, "after") {
		t.Errorf("closed line = %q, must not contain the segment that came after the pause", finals[0].Text)
	}
	if finals[0].Text != "before the pause" {
		t.Errorf("closed line = %q, want %q", finals[0].Text, "before the pause")
	}
}
