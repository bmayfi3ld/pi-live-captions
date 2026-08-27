// Package deepgram speaks Deepgram's real-time speech-to-text protocol: it
// dials the WebSocket, frames PCM the way Deepgram expects, and turns its JSON
// messages into stt.Transcript.
//
// Everything that is not protocol — reconnect backoff, the silence gate, the
// bounded audio buffer, latency anchoring — lives in stt.RunSession, which
// this package hands a stt.Dialer to.
package deepgram

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/coder/websocket"

	"livecaption/internal/audio"
	"livecaption/internal/stt"
)

const (
	endpoint = "wss://api.deepgram.com/v1/listen"

	// readLimit accommodates Results messages with full word arrays, which
	// exceed the library's 32KB default read limit on longer utterances.
	readLimit = 1 << 20

	// maxKeyterms keeps the request under Deepgram's budget of 500 tokens
	// across all keyterms, which it enforces by rejecting the whole request.
	// Tokens, not terms: a hyphenated or multi-word term costs more than one,
	// so the cut sits well below 500 rather than at it. Note this is a much
	// smaller list than Speechmatics accepts (1000 entries) — the same keyterm
	// file feeds both, and each engine takes as much of it as it can.
	maxKeyterms = 400
)

// Engine streams PCM to Deepgram's real-time API and turns its JSON messages
// into stt.Transcript.
type Engine struct {
	cfg stt.Config

	// wsURL overrides the Deepgram endpoint. Only ever set by tests, which
	// point it at an httptest server instead of the real API.
	wsURL string
}

// New builds the Deepgram engine.
func New(cfg stt.Config) *Engine { return &Engine{cfg: cfg} }

func (e *Engine) Name() string { return "deepgram" }

func (e *Engine) Run(ctx context.Context, frames <-chan audio.Frame, out chan<- stt.Transcript) error {
	return stt.RunSession(ctx, e.cfg, e.Name(), e.dial, frames, out)
}

// dial opens one connection to Deepgram. A handshake rejected with 401/403 is
// returned as a stt.PermanentError so the driver stops immediately rather than
// retrying a key that will never be accepted.
func (e *Engine) dial(ctx context.Context) (*websocket.Conn, stt.Session, error) {
	h := http.Header{}
	h.Set("Authorization", "Token "+e.cfg.APIKey)

	// nolint:bodyclose // coder/websocket owns resp.Body: nil on success,
	// an in-memory NopCloser on failure. Its docs say never close it.
	conn, resp, err := websocket.Dial(ctx, e.dialURL(), &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return nil, nil, &stt.PermanentError{
				Err: fmt.Errorf("deepgram: %w (check DEEPGRAM_API_KEY)", err),
			}
		}
		return nil, nil, err
	}
	conn.SetReadLimit(readLimit)
	return conn, &session{conn: conn, log: slog.Default()}, nil
}

func (e *Engine) dialURL() string {
	base := e.wsURL
	if base == "" {
		base = endpoint
	}

	q := url.Values{}
	// The pipeline is fixed at 16-bit signed samples end to end (see
	// audio.PipelineFormat), so the encoding is too.
	q.Set("encoding", "linear16")
	q.Set("sample_rate", strconv.Itoa(e.cfg.Format.SampleRate))
	q.Set("channels", strconv.Itoa(e.cfg.Format.Channels))
	q.Set("model", e.cfg.Model)
	q.Set("language", e.cfg.Language)
	// interim_results=false: this engine ships only settled text. Set
	// explicitly rather than left to the server's default — everything
	// downstream paints a Transcript once and never revises it, so a changed
	// default silently arriving would corrupt the display rather than merely
	// change its timing.
	q.Set("interim_results", "false")
	// punctuate and smart_format are load-bearing, not cosmetic: the hub
	// closes transcript lines on terminal punctuation, so turning these off
	// would silently degrade transcript.txt to the speech-gap fallback for
	// every sentence.
	q.Set("punctuate", "true")
	q.Set("profanity_filter", "true")
	q.Set("smart_format", "true")
	q.Set("diarize", strconv.FormatBool(e.cfg.Diarize))
	// Deepgram's own `endpointing` is left at the server default, which is
	// now what governs caption cadence end to end: text lands when Deepgram
	// finalizes a window, not before. It is therefore the first knob to reach
	// for if captions feel late (lower: sooner, in smaller pieces) or if
	// phrases fragment across rows (raise it). Watch Segments / lines on
	// /admin to tell which is happening.
	for _, k := range stt.CapKeyterms(e.cfg.Keyterms, maxKeyterms, slog.Default()) {
		q.Add("keyterm", k)
	}
	return base + "?" + q.Encode()
}

// session is Deepgram's protocol state for one connection. Deepgram tracks the
// media clock by counting the bytes it has received, so there is nothing to
// carry here beyond the socket itself.
type session struct {
	conn *websocket.Conn
	log  *slog.Logger
}

func (s *session) SendAudio(ctx context.Context, pcm []byte) error {
	return s.conn.Write(ctx, websocket.MessageBinary, pcm)
}

// Idle sends a KeepAlive. Deepgram drops a connection that goes ~10s without
// traffic, which is what the driver's idle interval is sized against.
func (s *session) Idle(ctx context.Context) error {
	return s.writeJSON(ctx, controlMessage{Type: "KeepAlive"})
}

func (s *session) Finish(ctx context.Context) error {
	return s.writeJSON(ctx, controlMessage{Type: "CloseStream"})
}

// Decode turns one server message into zero or more Transcripts, dropping
// everything that carries nothing to publish. Errors are swallowed here
// rather than returned: an undecodable frame is noise, not a reason to tear
// down a working link.
func (s *session) Decode(data []byte) ([]stt.Transcript, error) {
	ts, isFinal, err := decodeTranscript(data)
	if err != nil {
		s.log.Debug("deepgram: undecodable message", "err", err)
		return nil, nil
	}
	if len(ts) == 0 {
		return nil, nil
	}
	// dialURL asks for interim_results=false, so this should never fire.
	// It stays as a trust boundary against the server sending interims
	// anyway — a param silently ignored, or a changed default. Publishing
	// one as settled text would break the append-only guarantee the whole
	// display rests on, and it is one comparison to prevent.
	if !isFinal {
		return nil, nil
	}
	return ts, nil
}

func (s *session) writeJSON(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.conn.Write(ctx, websocket.MessageText, b)
}

type controlMessage struct {
	Type string `json:"type"`
}

// messageType is decoded first so only the shape a message actually claims to
// be attempts a full unmarshal: SpeechStarted carries "channel" as a plain
// index array rather than the object Results uses, so blindly unmarshaling
// everything as resultsMessage would fail on it.
type messageType struct {
	Type string `json:"type"`
}

// resultsMessage matches the subset of Deepgram's Results JSON this engine
// cares about; word timings and everything else are decoded straight through
// and ignored.
type resultsMessage struct {
	IsFinal  bool    `json:"is_final"`
	Start    float64 `json:"start"`
	Duration float64 `json:"duration"`
	Channel  struct {
		Alternatives []struct {
			Transcript string  `json:"transcript"`
			Confidence float64 `json:"confidence"`
			// Words is populated when diarize=true and carries per-word
			// speaker attribution; empty with diarization off (or a server
			// that ignored the param), in which case decodeTranscript falls
			// back to the flat Transcript field above.
			Words []struct {
				Word           string  `json:"word"`
				PunctuatedWord string  `json:"punctuated_word"`
				Start          float64 `json:"start"`
				End            float64 `json:"end"`
				Confidence     float64 `json:"confidence"`
				Speaker        int     `json:"speaker"`
			} `json:"words"`
		} `json:"alternatives"`
	} `json:"channel"`
}

// decodeTranscript turns one server message into the Transcripts it carries
// plus whether it was Deepgram's is_final for that window. The slice is empty
// for messages that carry nothing worth publishing: empty Results (no speech
// yet), Metadata, SpeechStarted, and any other unrecognized type. An interim
// still decodes with a non-empty slice and isFinal=false — this function's
// job is to report the message faithfully; Decode is what decides interims
// are not published.
//
// With diarization on, one Results message can span more than one speaker
// (e.g. an MC's question immediately followed by a guest's answer inside the
// same finalized window), so Words is grouped into consecutive runs by
// speaker: one Transcript per run rather than one per message. Without
// diarization (or against a server that ignored the param) Words is empty and
// this falls back to exactly the old single-Transcript behavior, Speaker: 0.
func decodeTranscript(data []byte) (ts []stt.Transcript, isFinal bool, err error) {
	var head messageType
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, false, err
	}

	if head.Type != "Results" {
		return nil, false, nil
	}

	var msg resultsMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, false, err
	}
	if len(msg.Channel.Alternatives) == 0 {
		return nil, false, nil
	}
	alt := msg.Channel.Alternatives[0]

	if len(alt.Words) == 0 {
		// No per-word attribution: today's behavior, unconditionally
		// Speaker 0 (unknown). This is the only path when Diarize is false,
		// and stays the fallback if a diarizing request ever comes back
		// without Words.
		if alt.Transcript == "" {
			return nil, false, nil
		}
		return []stt.Transcript{{
			Text:       alt.Transcript,
			Start:      stt.SecondsToDuration(msg.Start),
			Duration:   stt.SecondsToDuration(msg.Duration),
			Confidence: alt.Confidence,
		}}, msg.IsFinal, nil
	}

	// Group consecutive words by speaker: a run boundary is a change in
	// Speaker, not just any occurrence of a given speaker, so an "MC, guest,
	// MC" exchange yields three runs in order rather than merging the two MC
	// runs together.
	var run []int // indices into alt.Words for the run in progress
	flush := func() {
		if len(run) == 0 {
			return
		}
		first, last := alt.Words[run[0]], alt.Words[run[len(run)-1]]
		var text strings.Builder
		var confSum float64
		for i, wi := range run {
			w := alt.Words[wi]
			if i > 0 {
				text.WriteByte(' ')
			}
			// punctuated_word carries the casing and punctuation that
			// caption.Hub.closeLocked's endsSentence check depends on to
			// close transcript lines; the raw word field is lowercase and
			// bare (Deepgram's punctuate/smart_format shape
			// punctuated_word, not word). Falling back to word only
			// guards against a message that somehow omits the field.
			pw := w.PunctuatedWord
			if pw == "" {
				pw = w.Word
			}
			text.WriteString(pw)
			confSum += w.Confidence
		}
		ts = append(ts, stt.Transcript{
			Text:  text.String(),
			Start: stt.SecondsToDuration(first.Start),
			// Duration spans first word's start to last word's end, not the
			// message's overall Start/Duration, which cover every speaker.
			Duration:   stt.SecondsToDuration(last.End - first.Start),
			Confidence: confSum / float64(len(run)),
			// Deepgram speaker indices are 0-based; this package's Speaker
			// is 1-based with 0 reserved for "unknown", so every index
			// shifts up by one. first.Speaker, not the word that triggered
			// this flush: every word in run already shares one speaker by
			// construction (a differing speaker closes the run before
			// joining it), but at flush time the loop variable may already
			// have moved on to the next word's (different) speaker.
			Speaker: first.Speaker + 1,
		})
		run = run[:0]
	}
	var curSpeaker int
	for i, w := range alt.Words {
		if i > 0 && w.Speaker != curSpeaker {
			flush()
		}
		curSpeaker = w.Speaker
		run = append(run, i)
	}
	flush()

	return ts, msg.IsFinal, nil
}
