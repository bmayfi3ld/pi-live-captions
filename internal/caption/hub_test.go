package caption

import (
	"slices"
	"strings"
	"testing"
	"time"

	"livecaption/internal/metrics"
	"livecaption/internal/stt"
)

// testBreakGap aliases the package constant so the boundary tests below read
// as statements about the real threshold rather than a number they chose.
const testBreakGap = breakGap

func newTestHub() *Hub {
	return NewHub(metrics.New("test", "test"))
}

// captionText rejoins a caption event's words, which is what the viewer's
// shim does with them until it paces to their offsets. Assertions about a
// segment's text go through it; assertions about timing read ev.Words.
func captionText(ev Event) string {
	var parts []string
	for _, w := range ev.Words {
		parts = append(parts, w.Text)
	}
	return strings.Join(parts, " ")
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
	drain(sub) // discard the initial status

	h.Publish(stt.Transcript{Words: stt.Untimed("hello there,"), Start: time.Second, Duration: 500 * time.Millisecond})
	h.Publish(stt.Transcript{Words: stt.Untimed("how are you?"), Start: 1500 * time.Millisecond, Duration: 500 * time.Millisecond})

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
			captions = append(captions, captionText(ev))
			texts = append(texts, captionText(ev))
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

	h.Publish(stt.Transcript{Words: stt.Untimed("that's the whole sentence."), Start: time.Second})
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
	h.Publish(stt.Transcript{Words: stt.Untimed("we will begin in a moment"), Start: 0, Duration: time.Second})
	// A gap well past testBreakGap, then a one-segment complete sentence.
	h.Publish(stt.Transcript{Words: stt.Untimed("Good morning."), Start: 3 * time.Second, Duration: time.Second})

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

	h.Publish(stt.Transcript{Words: stt.Untimed("item one"), Start: 0})
	if len(finals) != 0 {
		t.Fatalf("expected no line closed yet, got %d", len(finals))
	}

	// A gap comfortably past testBreakGap.
	h.Publish(stt.Transcript{Words: stt.Untimed("item two"), Start: 2 * time.Second})

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

	h.Publish(stt.Transcript{Words: stt.Untimed("first"), Start: 0, Duration: 100 * time.Millisecond})
	drain(sub)

	// Gap of exactly testBreakGap: isBreakLocked uses >=, so this must break.
	h.Publish(stt.Transcript{Words: stt.Untimed("second"), Start: 100*time.Millisecond + testBreakGap})

	events := drain(sub)
	found := false
	for _, ev := range events {
		if ev.Kind == KindCaption && captionText(ev) == "second" {
			found = true
			if !ev.Break {
				t.Errorf("caption event for %q: Break = false, want true (gap == breakGap)", captionText(ev))
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

	h.Publish(stt.Transcript{Words: stt.Untimed("first"), Start: 0, Duration: 100 * time.Millisecond})
	drain(sub)

	// Comfortably under testBreakGap.
	h.Publish(stt.Transcript{Words: stt.Untimed("second"), Start: 200 * time.Millisecond})

	events := drain(sub)
	found := false
	for _, ev := range events {
		if ev.Kind == KindCaption && captionText(ev) == "second" {
			found = true
			if ev.Break {
				t.Errorf("caption event for %q: Break = true, want false (gap well under breakGap)", captionText(ev))
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

	h.Publish(stt.Transcript{Words: stt.Untimed("before the restart"), Start: 10 * time.Second, Duration: time.Second})
	drain(sub)
	if len(finals) != 0 {
		t.Fatalf("expected no line closed yet, got %d", len(finals))
	}

	// A fresh connection's media clock restarts at (or near) 0.
	h.Publish(stt.Transcript{Words: stt.Untimed("after the restart"), Start: 0})

	if len(finals) != 1 {
		t.Fatalf("expected the clock restart to close the pending line, got %d finals", len(finals))
	}
	if finals[0].Text != "before the restart" {
		t.Errorf("finalized text = %q, want %q", finals[0].Text, "before the restart")
	}

	events := drain(sub)
	found := false
	for _, ev := range events {
		if ev.Kind == KindCaption && captionText(ev) == "after the restart" {
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
		h.Publish(stt.Transcript{Words: stt.Untimed("word"), Start: start})
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

	h.Publish(stt.Transcript{Words: stt.Untimed("an unfinished sentence"), Start: 0})
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
	h := NewHub(m)

	sub, unsub := h.Subscribe()
	defer unsub()
	// Deliberately never read from sub.

	start := time.Duration(0)
	for i := 0; i < subscriberBuffer*3; i++ {
		h.Publish(stt.Transcript{Words: stt.Untimed("line."), Start: start})
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

// TestFirstEventIsStatusWithNoCatchUp pins what a late joiner gets. There is
// deliberately no replay of what was already said: the first event is a status
// carrying the last published state, so a page that connects mid-pause shows
// the right indicator, and nothing else. A blank display until the next
// segment is the accepted cost of having exactly one rendering path.
func TestFirstEventIsStatusWithNoCatchUp(t *testing.T) {
	h := newTestHub()
	h.PublishStatus("paused")
	h.Publish(stt.Transcript{Words: stt.Untimed("earlier line."), Start: 0})
	h.Publish(stt.Transcript{Words: stt.Untimed("in progress"), Start: 5 * time.Second})

	sub, unsub := h.Subscribe()
	defer unsub()

	ev := <-sub
	if ev.Kind != KindStatus {
		t.Fatalf("first event kind = %q, want status", ev.Kind)
	}
	if ev.State != "paused" {
		t.Errorf("first event State = %q, want the last published state", ev.State)
	}
	if len(ev.Words) != 0 {
		t.Errorf("first event carries %d words, want none — there is no catch-up", len(ev.Words))
	}
}

// TestCaptionEventCarriesAt guards against caption and status events shipping
// with a zero At: a viewer needs At on every event to measure its own
// publish->paint latency, since a caption event is what it sees first.
func TestCaptionEventCarriesAt(t *testing.T) {
	h := newTestHub()
	sub, unsub := h.Subscribe()
	defer unsub()
	drain(sub) // discard the initial status

	h.Publish(stt.Transcript{Words: stt.Untimed("in progress"), Start: time.Second})

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

// TestCaptionWordsAreSegmentRelative pins the wire's timing contract: a word
// travels with the onset the speaker actually gave it, rebased onto its own
// segment so the client never has to know the media clock — and a word
// claiming to start before its segment is clamped rather than sent negative.
func TestCaptionWordsAreSegmentRelative(t *testing.T) {
	h := newTestHub()
	sub, unsub := h.Subscribe()
	defer unsub()
	drain(sub)

	h.Publish(stt.Transcript{
		Start:    10 * time.Second,
		Duration: 900 * time.Millisecond,
		Words: []stt.Word{
			// Starts before its own segment, and ends before it starts: both
			// are provider self-contradictions, and both clamp to 0 rather
			// than putting a negative number on the wire.
			{Text: "measured", Start: 9500 * time.Millisecond, End: 9400 * time.Millisecond},
			{Text: "at", Start: 10250 * time.Millisecond, End: 10400 * time.Millisecond},
			{Text: "speed.", Start: 10600 * time.Millisecond, End: 10900 * time.Millisecond},
		},
	})

	events := drain(sub)
	if len(events) != 1 || events[0].Kind != KindCaption {
		t.Fatalf("expected one caption event, got %+v", events)
	}
	// DurMS is the word's own spoken length, NOT the gap to the next onset:
	// "at" runs 10250-10400 even though the next word starts at 10600, so the
	// viewer can tell the 200ms of silence after it from the 150ms of speech.
	want := []Word{
		{Text: "measured", OffsetMS: 0, DurMS: 0},
		{Text: "at", OffsetMS: 250, DurMS: 150},
		{Text: "speed.", OffsetMS: 600, DurMS: 300},
	}
	if !slices.Equal(events[0].Words, want) {
		t.Errorf("words = %+v, want %+v", events[0].Words, want)
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

	h.Publish(stt.Transcript{Words: stt.Untimed("before the pause"), Start: 0, Duration: 100 * time.Millisecond})
	h.Publish(stt.Transcript{Words: stt.Untimed("after the pause"), Start: 100*time.Millisecond + testBreakGap})

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

// TestSpeakerChangeClosesLine covers the diarization half of the same "close
// before append" rule: a switch between two known speakers with no pause at
// all must still close the line in progress, the same as a pause would, and
// must not merge the new speaker's words into it.
func TestSpeakerChangeClosesLine(t *testing.T) {
	h := newTestHub()
	var finals []Line
	h.OnFinal = func(l Line) { finals = append(finals, l) }

	h.Publish(stt.Transcript{Words: stt.Untimed("the MC is talking"), Start: 0, Duration: 100 * time.Millisecond, Speaker: 1})
	// No gap at all: Start picks up exactly where the previous segment ended.
	h.Publish(stt.Transcript{Words: stt.Untimed("now the guest is"), Start: 100 * time.Millisecond, Speaker: 2})

	if len(finals) != 1 {
		t.Fatalf("expected the speaker change to close exactly one line, got %d", len(finals))
	}
	if finals[0].Text != "the MC is talking" {
		t.Errorf("closed line = %q, want %q", finals[0].Text, "the MC is talking")
	}
	if finals[0].Speaker != 1 {
		t.Errorf("closed line Speaker = %d, want 1", finals[0].Speaker)
	}
}

// TestUnknownSpeakerNeverCountsAsAChange guards the other side of that rule:
// Speaker 0 (unknown), on either the previous or the current segment, must
// never itself be treated as a speaker change — diarization dropping out
// mid-session, or never having been enabled, must not fragment transcript
// lines that would otherwise stay whole.
func TestUnknownSpeakerNeverCountsAsAChange(t *testing.T) {
	h := newTestHub()
	var finals []Line
	h.OnFinal = func(l Line) { finals = append(finals, l) }

	h.Publish(stt.Transcript{Words: stt.Untimed("known speaker"), Start: 0, Duration: 100 * time.Millisecond, Speaker: 1})
	h.Publish(stt.Transcript{Words: stt.Untimed("then unknown"), Start: 100 * time.Millisecond, Speaker: 0})

	if len(finals) != 0 {
		t.Fatalf("expected Speaker 0 to not force a close, got %d finals: %+v", len(finals), finals)
	}
}

// TestEventCarriesSpeaker checks that Publish stamps the transcript's speaker
// onto the caption event's wire field.
func TestEventCarriesSpeaker(t *testing.T) {
	h := newTestHub()
	sub, unsub := h.Subscribe()
	defer unsub()
	drain(sub) // discard the initial status

	h.Publish(stt.Transcript{Words: stt.Untimed("hello"), Start: 0, Speaker: 2})

	events := drain(sub)
	var found bool
	for _, ev := range events {
		if ev.Kind != KindCaption {
			continue
		}
		found = true
		if ev.Speaker != 2 {
			t.Errorf("Speaker = %d, want 2", ev.Speaker)
		}
	}
	if !found {
		t.Fatal("expected a caption event")
	}
}

// TestSpeakerChangeDoesNotSetBreak pins Break as pause-only: a speaker change
// with no pause must not set it, even though it does close the transcript
// line — those are two separate signals now, and Break conflating them would
// bake a display rule into the wire format the client can't see.
func TestSpeakerChangeDoesNotSetBreak(t *testing.T) {
	h := newTestHub()
	sub, unsub := h.Subscribe()
	defer unsub()
	drain(sub)

	h.Publish(stt.Transcript{Words: stt.Untimed("first speaker"), Start: 0, Duration: 100 * time.Millisecond, Speaker: 1})
	drain(sub)

	// No gap: Start picks up exactly where the previous segment ended.
	h.Publish(stt.Transcript{Words: stt.Untimed("second speaker"), Start: 100 * time.Millisecond, Speaker: 2})

	events := drain(sub)
	var found bool
	for _, ev := range events {
		if ev.Kind == KindCaption && captionText(ev) == "second speaker" {
			found = true
			if ev.Break {
				t.Error("Break = true for a speaker change with no pause, want false")
			}
		}
	}
	if !found {
		t.Fatal("expected a caption event for the second segment")
	}
}
