package speechmatics

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"livecaption/internal/audio"
	"livecaption/internal/metrics"
	"livecaption/internal/stt"
)

// --- test server plumbing ---

// serverConn is what a scripted test handler gets: an accepted connection plus
// the request context to drive reads and writes with.
type serverConn struct {
	c   *websocket.Conn
	ctx context.Context
}

func (s serverConn) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.c.Write(s.ctx, websocket.MessageText, b)
}

// recvJSON reads until a text message arrives and decodes it.
func (s serverConn) recvJSON(v any) error {
	for {
		typ, data, err := s.c.Read(s.ctx)
		if err != nil {
			return err
		}
		if typ != websocket.MessageText {
			continue
		}
		return json.Unmarshal(data, v)
	}
}

// drainBinary reads and discards audio frames until the connection ends or the
// client sends EndOfStream, whose last_seq_no is reported on seen.
func (s serverConn) drainBinary(seen chan<- int) {
	for {
		typ, data, err := s.c.Read(s.ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageBinary {
			continue
		}
		var m struct {
			Message   string `json:"message"`
			LastSeqNo int    `json:"last_seq_no"`
		}
		if json.Unmarshal(data, &m) == nil && m.Message == "EndOfStream" {
			seen <- m.LastSeqNo
			close(seen)
			return
		}
	}
}

func newTestServer(t *testing.T, handle func(serverConn)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		handle(serverConn{c: c, ctx: r.Context()})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testEngine(wsURL string) *Engine {
	return &Engine{
		cfg: stt.Config{
			Format:   audio.PipelineFormat,
			Model:    "enhanced",
			Language: "en",
			Keyterms: []string{"Bruckner", "Wörthersee"},
			APIKey:   "test-key",
			Metrics:  metrics.New("test", "session"),
		},
		wsURL: wsURL,
	}
}

// addTranscript builds an AddTranscript the way the API documents it: one
// "word" result spanning the whole range, so tests that don't care about
// diarization can still assert on Start/Duration the way they always have. The flat transcript/metadata fields are populated too, purely
// as the hedge Speechmatics itself requires (see transcripts()) — with
// results present they are not what decodeTranscript actually reads.
func addTranscript(text string, start, end float64) map[string]any {
	return map[string]any{
		"message":    "AddTranscript",
		"format":     "2.1",
		"metadata":   map[string]any{"start_time": start, "end_time": end},
		"transcript": text,
		"results": []map[string]any{
			wordResult(text, start, end, 0.9, ""),
		},
	}
}

// wordResult and punctuationResult build one Results[] entry the way the API
// documents it. speaker is Speechmatics' own label ("S1", "S2", "UU") or ""
// when diarization wasn't requested.
func wordResult(content string, start, end, confidence float64, speaker string) map[string]any {
	return map[string]any{
		"type":         "word",
		"start_time":   start,
		"end_time":     end,
		"alternatives": []map[string]any{{"content": content, "confidence": confidence, "speaker": speaker}},
	}
}

// profanityResult is a word result carrying Speechmatics' own "profanity" tag,
// which it sends as standard with no config field to enable it.
func profanityResult(content string, start, end float64, speaker string) map[string]any {
	r := wordResult(content, start, end, 1.0, speaker)
	r["alternatives"].([]map[string]any)[0]["tags"] = []string{"profanity"}
	return r
}

func punctuationResult(content string, at float64, speaker string) map[string]any {
	return map[string]any{
		"type":         "punctuation",
		"start_time":   at,
		"end_time":     at,
		"alternatives": []map[string]any{{"content": content, "speaker": speaker}},
	}
}

// --- config ---

// TestStartMessage_ShipsSettledTextOnly pins the StartRecognition config. Two
// of these are load-bearing rather than cosmetic: enable_partials false is what
// keeps revisable text off the append-only display, and max_delay must stay
// clear of caption.breakGap (1.5s) or every committed chunk also reads as a
// speech pause and the rows go ragged.
func TestStartMessage_ShipsSettledTextOnly(t *testing.T) {
	msg := testEngine("").startMessage()

	if msg.Config.EnablePartials {
		t.Error("enable_partials must be false: this engine ships only settled text")
	}
	if msg.Config.MaxDelay >= 1.5 {
		t.Errorf("max_delay = %v, must stay comfortably under caption.breakGap (1.5s)", msg.Config.MaxDelay)
	}
	if msg.Config.MaxDelay < 0.7 {
		t.Errorf("max_delay = %v, below the API's minimum of 0.7", msg.Config.MaxDelay)
	}
	if msg.AudioFormat.Encoding != "pcm_s16le" || msg.AudioFormat.Type != "raw" {
		t.Errorf("audio_format = %+v, want raw/pcm_s16le", msg.AudioFormat)
	}
	// From cfg.Format, never hardcoded: the pipeline's rate is the one source
	// of truth and a mismatch here transcribes chipmunks.
	if msg.AudioFormat.SampleRate != audio.PipelineFormat.SampleRate {
		t.Errorf("sample_rate = %d, want %d", msg.AudioFormat.SampleRate, audio.PipelineFormat.SampleRate)
	}
	if len(msg.Config.AdditionalVocab) != 2 || msg.Config.AdditionalVocab[0].Content != "Bruckner" {
		t.Errorf("additional_vocab = %+v, want one entry per keyterm", msg.Config.AdditionalVocab)
	}
	if msg.Config.Language != "en" || msg.Config.Model != "enhanced" {
		t.Errorf("language/model = %q/%q, want they come from Config", msg.Config.Language, msg.Config.Model)
	}
}

// TestStartMessage_CapsAdditionalVocab guards the documented ceiling of 1000
// entries. What the server does past it is undocumented, and invalid_config is
// a permanent error that stops the run, so an over-long list loses its tail
// here instead.
func TestStartMessage_CapsAdditionalVocab(t *testing.T) {
	e := testEngine("")
	e.cfg.Keyterms = make([]string, maxVocab+50)
	for i := range e.cfg.Keyterms {
		e.cfg.Keyterms[i] = fmt.Sprintf("term%d", i)
	}

	msg := e.startMessage()
	if len(msg.Config.AdditionalVocab) != maxVocab {
		t.Errorf("additional_vocab = %d entries, want it capped at %d", len(msg.Config.AdditionalVocab), maxVocab)
	}
	// Cut from the tail: keyterm files are written most-likely-spoken first.
	if msg.Config.AdditionalVocab[0].Content != "term0" {
		t.Errorf("kept %q first, want the head of the list", msg.Config.AdditionalVocab[0].Content)
	}
}

// TestStartMessage_MusicDetect pins audio_events_config to exactly what
// startMessage promises: present with types ["music"] when requested, and
// the key omitted entirely (not an empty list) when not, so a Speechmatics
// change to its own "no events requested" default can't silently start
// asking for audio events this engine never wanted.
func TestStartMessage_MusicDetect(t *testing.T) {
	e := testEngine("")
	e.cfg.MusicDetect = true
	b, err := json.Marshal(e.startMessage())
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	aec, ok := raw["audio_events_config"].(map[string]any)
	if !ok {
		t.Fatalf("audio_events_config missing from %s", b)
	}
	if types, _ := aec["types"].([]any); len(types) != 1 || types[0] != "music" {
		t.Errorf("audio_events_config.types = %v, want [\"music\"]", aec["types"])
	}

	e.cfg.MusicDetect = false
	b, err = json.Marshal(e.startMessage())
	if err != nil {
		t.Fatal(err)
	}
	raw = nil // json.Unmarshal into a map only adds keys, never clears stale ones
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["audio_events_config"]; ok {
		t.Errorf("audio_events_config should be omitted when MusicDetect is false, got %s", b)
	}
}

// --- decoding ---

func TestDecode(t *testing.T) {
	s := &session{log: slog.Default()}

	t.Run("final transcript is published", func(t *testing.T) {
		data, _ := json.Marshal(addTranscript("hello there", 1.5, 2.5))
		ts, err := s.Decode(data)
		if err != nil || len(ts) != 1 {
			t.Fatalf("Decode = (%v, %v), want one transcript", ts, err)
		}
		tr := ts[0]
		if tr.Text() != "hello there" {
			t.Errorf("Text = %q", tr.Text())
		}
		if tr.Start != 1500*time.Millisecond {
			t.Errorf("Start = %v, want 1.5s", tr.Start)
		}
		if tr.Duration != time.Second {
			t.Errorf("Duration = %v, want 1s (end_time - start_time)", tr.Duration)
		}
	})

	// The trust boundary: enable_partials is false on the wire, but a server
	// that sent one anyway must not reach the append-only display.
	t.Run("partial is dropped", func(t *testing.T) {
		data := []byte(`{"message":"AddPartialTranscript","transcript":"hel",
			"metadata":{"start_time":0,"end_time":0.3}}`)
		if ts, err := s.Decode(data); len(ts) != 0 || err != nil {
			t.Errorf("Decode = (%v, %v), want a partial dropped silently", ts, err)
		}
	})

	t.Run("acks and metadata carry nothing", func(t *testing.T) {
		for _, data := range []string{
			`{"message":"AudioAdded","seq_no":7}`,
			`{"message":"RecognitionStarted","id":"abc"}`,
			`{"message":"Info","type":"recognition_quality","quality":"broadcast"}`,
			`{"message":"Warning","type":"idle_timeout","reason":"quiet"}`,
			`{"message":"AddTranscript","metadata":{"start_time":0,"end_time":1}}`,
			`not json at all`,
		} {
			if ts, err := s.Decode([]byte(data)); len(ts) != 0 || err != nil {
				t.Errorf("Decode(%s) = (%v, %v), want it skipped", data, ts, err)
			}
		}
	})

	t.Run("error is fatal", func(t *testing.T) {
		data := []byte(`{"message":"Error","type":"job_error","reason":"boom"}`)
		_, err := s.Decode(data)
		if err == nil {
			t.Fatal("an Error message should drop the connection")
		}
		if stt.IsPermanent(err) {
			t.Error("job_error is retryable, not permanent")
		}
	})

	t.Run("bad config is permanent", func(t *testing.T) {
		data := []byte(`{"message":"Error","type":"invalid_language","reason":"no such language"}`)
		_, err := s.Decode(data)
		if !stt.IsPermanent(err) {
			t.Errorf("invalid_language should be permanent, got %v", err)
		}
	})

	t.Run("end of transcript ends the read loop", func(t *testing.T) {
		_, err := s.Decode([]byte(`{"message":"EndOfTranscript"}`))
		if err != errEndOfTranscript { //nolint:errorlint // exact sentinel
			t.Errorf("Decode = %v, want errEndOfTranscript", err)
		}
	})
}

// TestDecode_MusicEvents pins AudioEventStarted/AudioEventEnded driving
// OnMusic on true/false edges for "music" events, and confirms an event of
// another type (there is none requested today, but Decode must stay correct
// if that changes) is silently ignored rather than flipping the gate.
func TestDecode_MusicEvents(t *testing.T) {
	var calls []bool
	s := &session{log: slog.Default(), onMusic: func(active bool) { calls = append(calls, active) }}

	start := []byte(`{"message":"AudioEventStarted","event":{"type":"music","start_time":1.2,"confidence":0.8}}`)
	if ts, err := s.Decode(start); len(ts) != 0 || err != nil {
		t.Errorf("Decode(AudioEventStarted) = (%v, %v), want nothing published", ts, err)
	}
	end := []byte(`{"message":"AudioEventEnded","event":{"type":"music","end_time":4.5}}`)
	if ts, err := s.Decode(end); len(ts) != 0 || err != nil {
		t.Errorf("Decode(AudioEventEnded) = (%v, %v), want nothing published", ts, err)
	}
	if want := []bool{true, false}; !slices.Equal(calls, want) {
		t.Errorf("onMusic calls = %v, want %v", calls, want)
	}

	other := []byte(`{"message":"AudioEventStarted","event":{"type":"speech"}}`)
	if _, err := s.Decode(other); err != nil {
		t.Errorf("Decode(other event type) = %v, want no error", err)
	}
	if want := []bool{true, false}; !slices.Equal(calls, want) {
		t.Errorf("a non-music event must not drive onMusic; calls = %v, want %v", calls, want)
	}
}

// TestDecode_TranscriptInMetadata covers the other place Speechmatics has
// documented the assembled transcript over the years. Both are read, so a
// server on either shape still produces captions. This also exercises the
// Results-empty fallback: no "results" key at all here, so transcripts()
// falls all the way back to the flat/metadata text fields, Speaker 0.
func TestDecode_TranscriptInMetadata(t *testing.T) {
	s := &session{log: slog.Default()}
	data := []byte(`{"message":"AddTranscript",
		"metadata":{"start_time":0,"end_time":1,"transcript":"nested"}}`)
	ts, err := s.Decode(data)
	if err != nil || len(ts) != 1 || ts[0].Text() != "nested" {
		t.Fatalf("Decode = (%v, %v), want the metadata transcript", ts, err)
	}
	if ts[0].Speaker != 0 {
		t.Errorf("Speaker = %d, want 0 (the flat-text fallback never attributes a speaker)", ts[0].Speaker)
	}
}

// TestTranscripts_Diarization covers the two rules that matter once
// diarization is on: a punctuation result attaches to the preceding word with
// no space and never starts a run of its own, and a run boundary is a change
// in speaker, so a message spanning "MC ... guest ..." splits into one
// Transcript per speaker in order.
func TestTranscripts_Diarization(t *testing.T) {
	data, err := json.Marshal(map[string]any{
		"message": "AddTranscript",
		"results": []map[string]any{
			wordResult("Hello", 0, 0.4, 0.95, "S1"),
			punctuationResult(",", 0.4, "S1"),
			wordResult("there", 0.4, 0.8, 0.93, "S1"),
			wordResult("Hi", 0.8, 1.1, 0.90, "S2"),
			wordResult("there", 1.1, 1.5, 0.92, "S2"),
			punctuationResult(".", 1.5, "S2"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var msg serverMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatal(err)
	}

	ts, err := msg.transcripts()
	if err != nil {
		t.Fatalf("transcripts: %v", err)
	}
	if len(ts) != 2 {
		t.Fatalf("got %d transcripts, want 2: %+v", len(ts), ts)
	}

	if ts[0].Text() != "Hello, there" {
		t.Errorf("run 0 Text = %q, want %q (punctuation attaches with no space)", ts[0].Text(), "Hello, there")
	}
	// Each word keeps its own onset, and the comma is glued onto the word it
	// follows rather than becoming a Word of its own — punctuation is not a
	// beat a pacer should reveal separately, and a stray entry here would
	// also desync every downstream index into the segment.
	// End is the word's own end, not the comma's: a punctuation result carries
	// an end_time but no spoken duration, so folding it in would tell the pacer
	// "Hello," takes longer to say than it does.
	wantRun0 := []stt.Word{
		{Text: "Hello,", Start: 0, End: 400 * time.Millisecond},
		{Text: "there", Start: 400 * time.Millisecond, End: 800 * time.Millisecond},
	}
	if !slices.Equal(ts[0].Words, wantRun0) {
		t.Errorf("run 0 Words = %+v, want %+v", ts[0].Words, wantRun0)
	}
	if ts[0].Speaker != 1 {
		t.Errorf("run 0 Speaker = %d, want 1 (S1)", ts[0].Speaker)
	}
	if ts[0].Start != 0 || ts[0].Duration != 800*time.Millisecond {
		t.Errorf("run 0 Start/Duration = %v/%v, want 0/800ms", ts[0].Start, ts[0].Duration)
	}

	if ts[1].Text() != "Hi there." {
		t.Errorf("run 1 Text = %q, want %q", ts[1].Text(), "Hi there.")
	}
	wantRun1 := []stt.Word{
		{Text: "Hi", Start: 800 * time.Millisecond, End: 1100 * time.Millisecond},
		{Text: "there.", Start: 1100 * time.Millisecond, End: 1500 * time.Millisecond},
	}
	if !slices.Equal(ts[1].Words, wantRun1) {
		t.Errorf("run 1 Words = %+v, want %+v", ts[1].Words, wantRun1)
	}
	if ts[1].Speaker != 2 {
		t.Errorf("run 1 Speaker = %d, want 2 (S2)", ts[1].Speaker)
	}
	if ts[1].Start != 800*time.Millisecond || ts[1].Duration != 700*time.Millisecond {
		t.Errorf("run 1 Start/Duration = %v/%v, want 800ms/700ms", ts[1].Start, ts[1].Duration)
	}
}

// TestTranscripts_UnknownSpeaker pins "UU" (Speechmatics' unattributed-audio
// label) mapping to this package's own 0/"unknown" sentinel, same as no label
// at all.
func TestTranscripts_UnknownSpeaker(t *testing.T) {
	data, _ := json.Marshal(map[string]any{
		"message": "AddTranscript",
		"results": []map[string]any{wordResult("hello", 0, 0.4, 0.9, "UU")},
	})
	var msg serverMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatal(err)
	}
	ts, err := msg.transcripts()
	if err != nil || len(ts) != 1 {
		t.Fatalf("transcripts = (%v, %v), want one transcript", ts, err)
	}
	if ts[0].Speaker != 0 {
		t.Errorf("Speaker = %d, want 0 for UU", ts[0].Speaker)
	}
}

// TestTranscripts_LeadingPunctuationIsDropped guards the edge case implied by
// "punctuation never starts a run": a message that somehow opens on a
// punctuation result has nothing to attach it to, so it is dropped rather
// than starting a run for a bare symbol.
func TestTranscripts_LeadingPunctuationIsDropped(t *testing.T) {
	data, _ := json.Marshal(map[string]any{
		"message": "AddTranscript",
		"results": []map[string]any{punctuationResult(",", 0, "S1")},
	})
	var msg serverMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatal(err)
	}
	ts, err := msg.transcripts()
	if err != nil || len(ts) != 0 {
		t.Errorf("transcripts = (%v, %v), want none (leading punctuation has nothing to attach to)", ts, err)
	}
}

// TestTranscripts_ProfanityIsStripped pins the whole profanity path at once:
// the tagged word vanishes, and — the part that is easy to get wrong — every
// surviving word keeps the timing it arrived with. Removing a word from a run
// touches the run's start, its end, its speaker splits and the punctuation glue,
// so each is asserted here rather than trusted.
func TestTranscripts_ProfanityIsStripped(t *testing.T) {
	data, _ := json.Marshal(map[string]any{
		"message": "AddTranscript",
		"results": []map[string]any{
			// A run that opens on a profanity: the run's Start must come from
			// "well" at 0.4s, not from the removed word at 0.
			profanityResult("damnit", 0, 0.4, "S1"),
			wordResult("well", 0.4, 0.8, 0.95, "S1"),
			// Mid-run profanity followed by a comma: the word goes, the comma
			// stays and glues onto "well" instead.
			profanityResult("shit", 0.8, 1.1, "S1"),
			punctuationResult(",", 1.1, "S1"),
			wordResult("anyway", 1.2, 1.6, 0.9, "S1"),
			// A whole speaker run that is nothing but profanity: it must not
			// open a run at all, so S1 above and S2 below stay two runs, not
			// three with an empty one between them.
			profanityResult("bollocks", 1.6, 1.9, "S2"),
			wordResult("Amen", 2.0, 2.5, 0.99, "S3"),
			punctuationResult(".", 2.5, "S3"),
		},
	})
	var msg serverMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatal(err)
	}
	ts, err := msg.transcripts()
	if err != nil {
		t.Fatalf("transcripts: %v", err)
	}
	if len(ts) != 2 {
		t.Fatalf("got %d transcripts, want 2 (the all-profanity S2 run must not open one): %+v", len(ts), ts)
	}

	if ts[0].Text() != "well, anyway" {
		t.Errorf("run 0 Text = %q, want %q", ts[0].Text(), "well, anyway")
	}
	// The onsets are the ones Speechmatics sent, untouched: nothing is
	// reindexed or shifted to close the hole the removed word left. The gap
	// between "well," ending at 0.8s and "anyway" starting at 1.2s is what the
	// viewer's pacer reads as a pause.
	wantRun0 := []stt.Word{
		{Text: "well,", Start: 400 * time.Millisecond, End: 800 * time.Millisecond},
		{Text: "anyway", Start: 1200 * time.Millisecond, End: 1600 * time.Millisecond},
	}
	if !slices.Equal(ts[0].Words, wantRun0) {
		t.Errorf("run 0 Words = %+v, want %+v", ts[0].Words, wantRun0)
	}
	if ts[0].Start != 400*time.Millisecond {
		t.Errorf("run 0 Start = %v, want 400ms (the first kept word, not the removed one at 0)", ts[0].Start)
	}
	if ts[0].Duration != 1200*time.Millisecond {
		t.Errorf("run 0 Duration = %v, want 1200ms (400ms..1600ms)", ts[0].Duration)
	}

	if ts[1].Speaker != 3 || ts[1].Text() != "Amen." {
		t.Errorf("run 1 = speaker %d %q, want speaker 3 %q", ts[1].Speaker, ts[1].Text(), "Amen.")
	}
	if ts[1].Start != 2*time.Second || ts[1].Duration != 500*time.Millisecond {
		t.Errorf("run 1 Start/Duration = %v/%v, want 2s/500ms", ts[1].Start, ts[1].Duration)
	}
}

// TestTranscripts_AllProfanityYieldsNothing guards the empty-run edge: flush()
// gates on `open`, not on len(words), so a window that is entirely profanity
// must never open a run — otherwise it ships a Transcript with zero words for
// the hub to drop on empty text.
func TestTranscripts_AllProfanityYieldsNothing(t *testing.T) {
	data, _ := json.Marshal(map[string]any{
		"message": "AddTranscript",
		"results": []map[string]any{
			profanityResult("shit", 0, 0.4, "S1"),
			profanityResult("shit", 0.4, 0.8, "S1"),
			punctuationResult("!", 0.8, "S1"),
		},
	})
	var msg serverMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatal(err)
	}
	ts, err := msg.transcripts()
	if err != nil || len(ts) != 0 {
		t.Errorf("transcripts = (%+v, %v), want none", ts, err)
	}
}

// --- session ---

// TestEngine_FullSession drives real PCM through a fake server: handshake,
// audio, a transcript out, and a polite EndOfStream carrying the right count.
func TestEngine_FullSession(t *testing.T) {
	eosSeen := make(chan int, 1)
	started := make(chan startRecognition, 1)

	srv := newTestServer(t, func(sc serverConn) {
		var start startRecognition
		if err := sc.recvJSON(&start); err != nil {
			return
		}
		started <- start
		if err := sc.send(map[string]any{"message": "RecognitionStarted", "id": "test"}); err != nil {
			return
		}
		eos := make(chan int, 1)
		go sc.drainBinary(eos)
		// A partial first: it must never reach out.
		sc.send(map[string]any{"message": "AddPartialTranscript", "transcript": "hel",
			"metadata": map[string]any{"start_time": 0, "end_time": 0.1}})
		sc.send(addTranscript("hello", 0, 0.1))
		select {
		case n := <-eos:
			eosSeen <- n
			// What the real server does once it has processed the stream;
			// it is also what lets the client stop draining immediately
			// instead of waiting out its timeout.
			sc.send(map[string]any{"message": "EndOfTranscript"})
		case <-sc.ctx.Done():
		}
	})

	eng := testEngine(srv.URL)

	frames := make(chan audio.Frame)
	out := make(chan stt.Transcript, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx, frames, out) }()

	for i := range 5 {
		frames <- audio.Frame{
			PCM:        loudPCM(3200),
			Offset:     time.Duration(i) * 100 * time.Millisecond,
			CapturedAt: time.Now(),
		}
	}

	select {
	case tr := <-out:
		if tr.Text() != "hello" {
			t.Errorf("Text = %q, want the final only (a partial leaked)", tr.Text())
		}
		if tr.ReceivedAt.IsZero() {
			t.Error("ReceivedAt should be stamped by the driver")
		}
		if tr.CapturedAt.IsZero() {
			t.Error("CapturedAt should be resolved from the anchor index")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no transcript arrived")
	}

	close(frames)
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want a clean end", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after frames closed")
	}

	start := <-started
	if start.Message != "StartRecognition" {
		t.Errorf("first message = %q, want StartRecognition", start.Message)
	}

	// EndOfStream must report how many AddAudio messages actually went out, or
	// the server waits forever for audio that will never arrive.
	select {
	case n := <-eosSeen:
		if n != 5 {
			t.Errorf("last_seq_no = %d, want 5 (one per audio frame sent)", n)
		}
	case <-time.After(time.Second):
		t.Error("no EndOfStream was sent")
	}
}

// TestEngine_AuthFailureFailsFast covers a rejected key arriving as an Error
// during the handshake rather than as a 401 on the HTTP upgrade: the run must
// stop, not redial into the same rejection forever.
func TestEngine_AuthFailureFailsFast(t *testing.T) {
	srv := newTestServer(t, func(sc serverConn) {
		var start startRecognition
		if err := sc.recvJSON(&start); err != nil {
			return
		}
		sc.send(map[string]any{
			"message": "Error", "type": "not_authorised", "reason": "invalid api key",
		})
		<-sc.ctx.Done()
	})

	eng := testEngine(srv.URL)

	frames := make(chan audio.Frame)
	out := make(chan stt.Transcript, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- eng.Run(ctx, frames, out) }()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected an error for not_authorised on first connect")
		}
		if !strings.Contains(err.Error(), "SPEECHMATICS_API_KEY") {
			t.Errorf("error should mention SPEECHMATICS_API_KEY, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("auth failure did not return promptly")
	}
	close(frames)
}

// TestEngine_HandshakeFailureReconnects checks the other half: a server that
// drops the connection before acknowledging is a blip, not a misconfiguration,
// so the driver keeps trying rather than giving up.
func TestEngine_HandshakeFailureReconnects(t *testing.T) {
	attempts := make(chan struct{}, 8)

	srv := newTestServer(t, func(sc serverConn) {
		select {
		case attempts <- struct{}{}:
		default:
		}
		// Accept, then hang up without ever sending RecognitionStarted.
		sc.c.CloseNow()
	})

	eng := testEngine(srv.URL)

	frames := make(chan audio.Frame)
	out := make(chan stt.Transcript, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx, frames, out) }()

	go func() {
		for i := range 40 {
			select {
			case frames <- audio.Frame{PCM: loudPCM(3200), Offset: time.Duration(i) * 100 * time.Millisecond}:
			case <-ctx.Done():
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	for range 2 {
		select {
		case <-attempts:
		case <-time.After(3 * time.Second):
			t.Fatal("expected the driver to redial after a failed handshake")
		}
	}
	cancel()
	<-done
}

// loudPCM is a buffer RMSDBFS reports as comfortably above any reasonable
// silence threshold, so the gate stays active and audio actually flows.
func loudPCM(n int) []byte {
	pcm := make([]byte, n)
	for i := 0; i+1 < n; i += 2 {
		pcm[i], pcm[i+1] = 0x20, 0x4e // little-endian int16(20000)
	}
	return pcm
}
