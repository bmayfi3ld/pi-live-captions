// Package speechmatics speaks Speechmatics' real-time speech-to-text protocol
// over a WebSocket and turns its JSON messages into stt.Transcript.
//
// Everything that is not protocol — reconnect backoff, the silence gate, the
// bounded audio buffer, latency anchoring — lives in stt.RunSession, which
// this package hands a stt.Dialer to.
//
// Unlike Deepgram, Speechmatics needs a handshake before audio can flow
// (StartRecognition, acknowledged with RecognitionStarted) and carries its own
// sequence numbering, so a session here holds real per-connection state.
package speechmatics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"livecaption/internal/audio"
	"livecaption/internal/stt"
)

const (
	// endpoint is the global entry point, which routes to the nearest region
	// on its own. Regional hosts (eu2.rt., etc.) exist but pinning one only
	// helps if you know better than the router, which for a travelling
	// soundboard we do not.
	endpoint = "wss://global.rt.speechmatics.com/v2"

	// readLimit matches the Deepgram engine's: an AddTranscript carrying a
	// full word array for a long utterance easily exceeds the library's 32KB
	// default.
	readLimit = 1 << 20

	// maxDelay is how long Speechmatics may wait before committing a final
	// transcript, in seconds. Its own default is 4s (valid range 0.7–4) and
	// that is far too slow here: caption.breakGap is 1.5s, and once the
	// finalisation window reaches breakGap every committed chunk also reads as
	// a speech pause, which puts the ragged-rows bug of an early draft back on
	// screen. 1.0 keeps finals comfortably underneath it.
	//
	// This is the knob to reach for if captions feel late (lower: sooner, in
	// smaller pieces) or if phrases fragment across rows (raise it, but never
	// to within reach of breakGap). Watch Segments / lines on /admin to tell
	// which is happening.
	maxDelay = 1.0

	// handshakeTimeout bounds the wait for RecognitionStarted. Without it a
	// server that accepts the socket and then goes quiet would park Run
	// forever, with the reconnect machinery never getting a chance to fire.
	//
	// Generous because additional_vocab makes session start slow: Speechmatics
	// documents a delay of up to 15s while it builds the dictionary, on every
	// connection whose vocabulary the server has not cached (their cache is
	// per-identical-list and expires after 24h unused). With --auto-pause
	// redialing after every quiet spell, a timeout under that would turn a big
	// keyterm list into an endless reconnect loop.
	handshakeTimeout = 30 * time.Second

	// maxVocab is the documented ceiling on additional_vocab: "up to 1000 words
	// or phrases (per job)". What the server does with entry 1001 is not
	// documented, so the list is cut before it goes out.
	maxVocab = 1000
)

// errEndOfTranscript is what Decode reports for the server's EndOfTranscript,
// which is not a failure: it ends the read loop so the driver stops waiting
// out its drain timeout after a polite EndOfStream.
var errEndOfTranscript = errors.New("speechmatics: end of transcript")

// permanentErrors are the Error types no amount of retrying will fix — they
// mean the key or the transcription config is wrong. Everything else (job
// errors, quota, timeouts) gets the normal backoff-and-redial treatment.
var permanentErrors = map[string]bool{
	"not_authorised":      true,
	"not_allowed":         true,
	"invalid_config":      true,
	"invalid_language":    true,
	"invalid_model":       true,
	"invalid_audio_type":  true,
	"invalid_output_type": true,
}

// Engine streams PCM to Speechmatics' real-time API and turns its JSON
// messages into stt.Transcript.
type Engine struct {
	cfg stt.Config

	// wsURL overrides the Speechmatics endpoint. Only ever set by tests,
	// which point it at an httptest server instead of the real API.
	wsURL string
}

// New builds the Speechmatics engine.
func New(cfg stt.Config) *Engine { return &Engine{cfg: cfg} }

func (e *Engine) Name() string { return "speechmatics" }

func (e *Engine) Run(ctx context.Context, frames <-chan audio.Frame, out chan<- stt.Transcript) error {
	return stt.RunSession(ctx, e.cfg, e.Name(), e.dial, frames, out)
}

// dial opens one connection and completes the StartRecognition handshake, so
// the driver only ever sees a connection that is actually ready for audio. A
// rejected key or a bad transcription config comes back as a
// stt.PermanentError, stopping the run instead of retrying a typo forever.
func (e *Engine) dial(ctx context.Context) (*websocket.Conn, stt.Session, error) {
	base := e.wsURL
	if base == "" {
		base = endpoint
	}

	h := http.Header{}
	h.Set("Authorization", "Bearer "+e.cfg.APIKey)

	// nolint:bodyclose // coder/websocket owns resp.Body: nil on success,
	// an in-memory NopCloser on failure. Its docs say never close it.
	conn, resp, err := websocket.Dial(ctx, base, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return nil, nil, &stt.PermanentError{
				Err: fmt.Errorf("speechmatics: %w (check SPEECHMATICS_API_KEY)", err),
			}
		}
		return nil, nil, err
	}
	conn.SetReadLimit(readLimit)

	s := &session{conn: conn, log: slog.Default()}
	if err := s.handshake(ctx, e.startMessage()); err != nil {
		conn.CloseNow()
		return nil, nil, err
	}
	return conn, s, nil
}

func (e *Engine) startMessage() startRecognition {
	terms := stt.CapKeyterms(e.cfg.Keyterms, maxVocab, slog.Default())
	vocab := make([]vocabEntry, 0, len(terms))
	for _, k := range terms {
		vocab = append(vocab, vocabEntry{Content: k})
	}
	// "speaker" is Speechmatics' only diarization mode worth asking for here
	// (the other, "channel", attributes by audio channel, and the pipeline is
	// mono); the empty string when Diarize is false omits the field from the
	// wire entirely rather than spelling out "none".
	diarization := ""
	if e.cfg.Diarize {
		diarization = "speaker"
	}
	return startRecognition{
		Message: "StartRecognition",
		AudioFormat: audioFormat{
			Type: "raw",
			// The pipeline is fixed at 16-bit signed little-endian samples end
			// to end (see audio.PipelineFormat), so the encoding is too. Raw
			// Speechmatics audio is mono, which is what the pipeline sends.
			Encoding:   "pcm_s16le",
			SampleRate: e.cfg.Format.SampleRate,
		},
		Config: transcriptionConfig{
			Language: e.cfg.Language,
			Model:    e.cfg.Model,
			MaxDelay: maxDelay,
			// This engine ships only settled text. Stated explicitly rather
			// than left to the server's default: everything downstream paints
			// a Transcript once and never revises it, so a changed default
			// silently arriving would corrupt the display rather than merely
			// change its timing. Decode drops partials regardless.
			EnablePartials:  false,
			AdditionalVocab: vocab,
			Diarization:     diarization,
		},
	}
}

// session is Speechmatics' protocol state for one connection.
type session struct {
	conn *websocket.Conn
	log  *slog.Logger

	// seqNo counts AddAudio messages, which EndOfStream must report back. Only
	// touched by SendAudio (the driver's single writer goroutine) and then by
	// Finish, which the driver calls only after that goroutine has returned.
	seqNo int
}

// handshake sends StartRecognition and reads until the server acknowledges it.
func (s *session) handshake(ctx context.Context, start startRecognition) error {
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()

	if err := s.writeJSON(ctx, start); err != nil {
		return fmt.Errorf("speechmatics: StartRecognition: %w", err)
	}

	for {
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("speechmatics: waiting for RecognitionStarted: %w", err)
		}
		var msg serverMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			s.log.Debug("speechmatics: undecodable handshake message", "err", err)
			continue
		}
		switch msg.Message {
		case "RecognitionStarted":
			return nil
		case "Error":
			return msg.asError()
		case "Warning":
			s.log.Warn("speechmatics: "+msg.Type, "reason", msg.Reason)
		}
	}
}

func (s *session) SendAudio(ctx context.Context, pcm []byte) error {
	if err := s.conn.Write(ctx, websocket.MessageBinary, pcm); err != nil {
		return err
	}
	s.seqNo++
	return nil
}

// Idle is a no-op: the driver feeds every frame into the ring, silence
// included, so a live connection never actually goes quiet long enough for
// Speechmatics' idle timeout, and the protocol has no keepalive message.
func (s *session) Idle(context.Context) error { return nil }

func (s *session) Finish(ctx context.Context) error {
	return s.writeJSON(ctx, endOfStream{Message: "EndOfStream", LastSeqNo: s.seqNo})
}

// Decode turns one server message into zero or more Transcripts. Acks,
// metadata and revisable partials are dropped; only an Error tears the
// connection down.
func (s *session) Decode(data []byte) ([]stt.Transcript, error) {
	var msg serverMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		// Noise, not a reason to drop a working link.
		s.log.Debug("speechmatics: undecodable message", "err", err)
		return nil, nil
	}

	switch msg.Message {
	case "AddTranscript":
		return msg.transcripts()

	case "AddPartialTranscript":
		// enable_partials is false, so this should never fire. It stays as a
		// trust boundary against the server sending partials anyway — a
		// setting silently ignored, or a changed default. Publishing one as
		// settled text would break the append-only guarantee the whole display
		// rests on, and it is one comparison to prevent.
		return nil, nil

	case "Error":
		return nil, msg.asError()

	case "EndOfTranscript":
		return nil, errEndOfTranscript

	case "Warning":
		s.log.Warn("speechmatics: "+msg.Type, "reason", msg.Reason)
		return nil, nil

	default:
		// AudioAdded, Info, RecognitionStarted, audio events, anything new:
		// nothing to publish.
		return nil, nil
	}
}

func (s *session) writeJSON(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.conn.Write(ctx, websocket.MessageText, b)
}

// --- wire types ---

type startRecognition struct {
	Message     string              `json:"message"`
	AudioFormat audioFormat         `json:"audio_format"`
	Config      transcriptionConfig `json:"transcription_config"`
}

type audioFormat struct {
	Type       string `json:"type"`
	Encoding   string `json:"encoding"`
	SampleRate int    `json:"sample_rate"`
}

type transcriptionConfig struct {
	Language        string       `json:"language"`
	Model           string       `json:"model,omitempty"`
	MaxDelay        float64      `json:"max_delay"`
	EnablePartials  bool         `json:"enable_partials"`
	AdditionalVocab []vocabEntry `json:"additional_vocab,omitempty"`
	Diarization     string       `json:"diarization,omitempty"`
}

type vocabEntry struct {
	Content string `json:"content"`
}

type endOfStream struct {
	Message   string `json:"message"`
	LastSeqNo int    `json:"last_seq_no"`
}

// serverMessage matches the subset of Speechmatics' server messages this
// engine cares about. Every type is decoded through this one struct: the
// fields that don't apply to a given message simply stay zero, and none of
// them collide across types the way Deepgram's `channel` does.
type serverMessage struct {
	Message string `json:"message"`

	// Type and Reason carry the detail on Error, Warning and Info.
	Type   string `json:"type"`
	Reason string `json:"reason"`

	Metadata struct {
		StartTime float64 `json:"start_time"`
		EndTime   float64 `json:"end_time"`
		// Speechmatics has moved the assembled transcript between the top
		// level and metadata across doc revisions, so both are decoded and
		// whichever is populated wins. Cheaper than being wrong about it.
		Transcript string `json:"transcript"`
	} `json:"metadata"`
	Transcript string `json:"transcript"`

	Results []struct {
		Type         string  `json:"type"`
		StartTime    float64 `json:"start_time"`
		EndTime      float64 `json:"end_time"`
		Alternatives []struct {
			Content    string  `json:"content"`
			Confidence float64 `json:"confidence"`
			// Speaker is Speechmatics' own label: "S1", "S2", ... per
			// identified speaker, or "UU" for unattributed audio. Empty when
			// diarization was never requested.
			Speaker string `json:"speaker"`
		} `json:"alternatives"`
	} `json:"results"`
}

// asError turns an Error message into a Go error, marking the ones that mean
// "your key or config is wrong" permanent so the driver gives up instead of
// redialing into the same rejection forever.
func (m serverMessage) asError() error {
	err := fmt.Errorf("speechmatics: %s: %s", m.Type, m.Reason)
	if permanentErrors[m.Type] {
		if m.Type == "not_authorised" {
			return &stt.PermanentError{Err: fmt.Errorf("%w (check SPEECHMATICS_API_KEY)", err)}
		}
		return &stt.PermanentError{Err: err}
	}
	return err
}

// transcripts builds the Transcripts carried by an AddTranscript. Without
// diarization (or against a server that ignored the config), or when Results
// is empty for some other reason, this falls back to exactly today's single
// Transcript built from the flat text field, Speaker 0 — the case that keeps
// --no-diarize working and preserves the existing hedge about Speechmatics
// having moved the assembled-transcript field between the top level and
// metadata across doc revisions (both are decoded, whichever is populated
// wins).
//
// With Results present, text is assembled from Results[] instead of the flat
// field, grouped into consecutive runs by speaker: one Transcript per run, so
// a window that itself spans a speaker change (an MC interrupted by a
// question, say) still splits correctly. Per-word confidence averaging
// (previously a standalone confidence() method) is folded into the same walk,
// per run, exactly as it worked before: only "word" results count, "entity"
// and "punctuation" are skipped.
func (m serverMessage) transcripts() ([]stt.Transcript, error) {
	if len(m.Results) == 0 {
		text := m.Transcript
		if text == "" {
			text = m.Metadata.Transcript
		}
		if text == "" {
			return nil, nil
		}
		start := stt.SecondsToDuration(m.Metadata.StartTime)
		end := stt.SecondsToDuration(m.Metadata.EndTime)
		return []stt.Transcript{{
			Text: text,
			// Media time on the current connection, exactly like Deepgram's
			// byte clock, which is what lets the shared anchor index resolve
			// CapturedAt/SentAt for both engines the same way.
			Start:    start,
			Duration: end - start,
		}}, nil
	}

	var ts []stt.Transcript
	var text strings.Builder
	var speaker int
	var start, end float64
	var confSum float64
	var confN int
	open := false

	flush := func() {
		if !open {
			return
		}
		var confidence float64
		if confN > 0 {
			confidence = confSum / float64(confN)
		}
		ts = append(ts, stt.Transcript{
			Text:       text.String(),
			Start:      stt.SecondsToDuration(start),
			Duration:   stt.SecondsToDuration(end - start),
			Confidence: confidence,
			Speaker:    speaker,
		})
		text.Reset()
		confSum, confN = 0, 0
		open = false
	}

	for _, r := range m.Results {
		if len(r.Alternatives) == 0 {
			continue
		}
		alt := r.Alternatives[0]

		// A punctuation result attaches to the preceding word with no space
		// and inherits the run it's joining — it must never start a run of
		// its own (there is nothing to attribute punctuation to on its own,
		// and treating a comma as a speaker change would fragment text that
		// belongs together). If somehow the very first result is
		// punctuation, there is no run to attach to yet, so it is dropped
		// rather than starting one for a symbol alone.
		if r.Type == "punctuation" {
			if open {
				text.WriteString(alt.Content)
				end = r.EndTime
			}
			continue
		}
		// Only "word" builds text. Speechmatics also has an "entity" result
		// type that repeats the words it spans in a written form ("twenty
		// twenty six" -> "2026"); it is gated behind enable_entities, which
		// startMessage never sets, but assembling text from every non-
		// punctuation type would silently double those words if that ever
		// changed. The old code was immune by construction, taking text from
		// the flat transcript field and walking Results only for confidence —
		// reassembling from Results is what makes this guard load-bearing.
		if r.Type != "word" {
			continue
		}

		spk := parseSpeaker(alt.Speaker)
		if !open || spk != speaker {
			flush()
			speaker = spk
			start = r.StartTime
			open = true
		}
		if text.Len() > 0 {
			text.WriteByte(' ')
		}
		text.WriteString(alt.Content)
		end = r.EndTime
		confSum += alt.Confidence
		confN++
	}
	flush()

	return ts, nil
}

// parseSpeaker turns Speechmatics' speaker label into this package's 1-based
// convention: "S1" -> 1, "S2" -> 2, and so on by parsing the trailing digits,
// "UU" (unattributed audio) and an empty label both -> 0, our own "unknown"
// sentinel.
func parseSpeaker(label string) int {
	i := len(label)
	for i > 0 && label[i-1] >= '0' && label[i-1] <= '9' {
		i--
	}
	digits := label[i:]
	if digits == "" {
		return 0
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return 0
	}
	return n
}
