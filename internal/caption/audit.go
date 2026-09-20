package caption

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"livecaption/internal/audio"
	"livecaption/internal/metrics"
)

// auditSchemaVersion versions the whole record envelope; readers reject or
// interpret lines by it. Bump when a field changes meaning, not when one is
// added.
const auditSchemaVersion = 1

// Record types. One JSON object per line in audit.jsonl; see docs/usage.md.
const (
	auditSessionStart = "session_start"
	auditCaption      = "caption"
	auditMarker       = "marker"
	auditNoiseGate    = "noise_gate"
	auditConfig       = "config"
	auditState        = "state"
	auditWarning      = "warning"
	auditError        = "error"
	auditDrop         = "drop"
	auditSessionEnd   = "session_end"
)

// auditHead is the envelope every record embeds. ObservedAt is absolute UTC;
// ElapsedMS is session-relative; SourceMS carries the source-media position
// only where one is known, so its absence is meaningful.
type auditHead struct {
	SchemaVersion int       `json:"schema_version"`
	Type          string    `json:"type"`
	ObservedAt    time.Time `json:"observed_at"`
	ElapsedMS     int64     `json:"elapsed_ms"`
	SourceMS      *int64    `json:"source_ms,omitempty"`
}

// StartMeta is the session_start payload: resolved runtime configuration
// only. There is no field a credential could flow into, which is how the
// "no secrets in the audit stream" guarantee is kept — by construction, not
// by filtering.
type StartMeta struct {
	Version      string
	SessionID    string
	SourceKind   string
	SourceSpec   string
	SourceFormat string
	Engine       string
	Model        string
	Language     string
	Keyterms     []string
	Diarize      bool
	MusicDetect  bool
	NoiseGate    audio.NoiseSettings
}

type startMetaJSON struct {
	Version   string `json:"version"`
	SessionID string `json:"session_id"`
	Source    struct {
		Kind   string `json:"kind"`
		Spec   string `json:"spec"`
		Format string `json:"format"`
	} `json:"source"`
	Recognition struct {
		Engine      string   `json:"engine"`
		Model       string   `json:"model"`
		Language    string   `json:"language"`
		Keyterms    []string `json:"keyterms"`
		Diarize     bool     `json:"diarize"`
		MusicDetect bool     `json:"music_detect"`
	} `json:"recognition"`
	NoiseGate audio.NoiseSettings `json:"noise_gate"`
}

type captionRecord struct {
	auditHead
	Text    string `json:"text"`
	Speaker int    `json:"speaker,omitempty"`
	// EndMS is the source-relative end of the line, present only when the
	// recognizer supplied a reliable end. Absent, never zero, when unknown.
	EndMS *int64 `json:"end_ms,omitempty"`
}

type markerRecord struct {
	auditHead
	Marker string `json:"marker"`
}

type gateRecord struct {
	auditHead
	Open          bool    `json:"open"`
	ThresholdDBFS float64 `json:"threshold_dbfs"`
	ReleaseSec    float64 `json:"release_sec"`
}

type configRecord struct {
	auditHead
	Component string              `json:"component"`
	Settings  audio.NoiseSettings `json:"settings"`
}

type stateRecord struct {
	auditHead
	Component string `json:"component"`
	State     string `json:"state"`
}

type severityRecord struct {
	auditHead
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Attrs   map[string]any `json:"attrs,omitempty"`
}

type dropRecord struct {
	auditHead
	Component string `json:"component"`
	Event     string `json:"event"`
	Count     int64  `json:"count"`
}

type sessionEndRecord struct {
	auditHead
	Summary metrics.Snapshot `json:"summary"`
}

// newHead stamps the envelope. at is the observation instant (now when the
// caller has none); source is the source-media position, omitted when the
// caller has none rather than invented.
func (w *Writer) newHead(typ string, at time.Time, source int64, hasSource bool) auditHead {
	if at.IsZero() {
		at = time.Now()
	}
	h := auditHead{
		SchemaVersion: auditSchemaVersion,
		Type:          typ,
		ObservedAt:    at.UTC(),
		ElapsedMS:     at.Sub(w.start).Milliseconds(),
	}
	if hasSource {
		ms := source
		h.SourceMS = &ms
	}
	return h
}

// SessionStart writes the metadata record. Called once, right after session
// wiring, so the first line of the file identifies the run.
func (w *Writer) SessionStart(m StartMeta) {
	var rec struct {
		auditHead
		startMetaJSON
	}
	rec.auditHead = w.newHead(auditSessionStart, w.start, 0, false)
	rec.Version = m.Version
	rec.SessionID = m.SessionID
	rec.Source.Kind = m.SourceKind
	rec.Source.Spec = m.SourceSpec
	rec.Source.Format = m.SourceFormat
	rec.Recognition.Engine = m.Engine
	rec.Recognition.Model = m.Model
	rec.Recognition.Language = m.Language
	rec.Recognition.Keyterms = m.Keyterms
	rec.Recognition.Diarize = m.Diarize
	rec.Recognition.MusicDetect = m.MusicDetect
	rec.NoiseGate = m.NoiseGate
	w.writeAudit(rec)
}

// SessionEnd appends the final summary from the same metrics snapshot the
// shutdown summary prints. The last complete record of a clean session.
func (w *Writer) SessionEnd(snap metrics.Snapshot) {
	rec := sessionEndRecord{Summary: snap}
	rec.auditHead = w.newHead(auditSessionEnd, time.Time{}, 0, false)
	w.writeAudit(rec)
}

// auditCaptionLine writes one caption or marker record for a line as it
// reaches transcript.txt. Caller holds w.mu.
func (w *Writer) auditCaptionLine(l Line) {
	if l.Text == musicMarker || l.Text == silenceMarker {
		rec := markerRecord{Marker: l.Text}
		rec.auditHead = w.newHead(auditMarker, l.At, l.OffsetMS, true)
		_ = w.writeAuditLocked(rec)
		return
	}
	rec := captionRecord{Text: l.Text, Speaker: l.Speaker}
	if l.EndOK {
		end := l.EndMS
		rec.EndMS = &end
	}
	rec.auditHead = w.newHead(auditCaption, l.At, l.OffsetMS, true)
	_ = w.writeAuditLocked(rec)
}

// GateEdge records one effective noise-gate transition: the new state, the
// source-media position it applies at, and the settings governing it.
func (w *Writer) GateEdge(open bool, at time.Duration, settings audio.NoiseSettings, observed time.Time) {
	rec := gateRecord{Open: open, ThresholdDBFS: settings.ThresholdDBFS, ReleaseSec: settings.ReleaseSec}
	rec.auditHead = w.newHead(auditNoiseGate, observed, at.Milliseconds(), true)
	w.writeAudit(rec)
}

// GateConfig records a runtime settings change, ahead of the edges that will
// use the new values.
func (w *Writer) GateConfig(settings audio.NoiseSettings) {
	rec := configRecord{Component: "noise_gate", Settings: settings}
	rec.auditHead = w.newHead(auditConfig, time.Time{}, 0, false)
	w.writeAudit(rec)
}

// StateEvent records a source or recognition state transition.
func (w *Writer) StateEvent(component, state string) {
	rec := stateRecord{Component: component, State: state}
	rec.auditHead = w.newHead(auditState, time.Time{}, 0, false)
	w.writeAudit(rec)
}

// DropEvent records one counted degradation event. source is omitted when
// unknown rather than zero-filled.
func (w *Writer) DropEvent(component, event string, count int64, source time.Duration, hasSource bool) {
	rec := dropRecord{Component: component, Event: event, Count: count}
	rec.auditHead = w.newHead(auditDrop, time.Time{}, source.Milliseconds(), hasSource)
	w.writeAudit(rec)
}

// LogRecord records one warning or error diagnostic with its attributes.
func (w *Writer) LogRecord(level, msg string, attrs []slog.Attr) {
	m := make(map[string]any, len(attrs))
	for _, a := range attrs {
		if a.Key != "" {
			value := a.Value.Any()
			key := strings.ToLower(strings.NewReplacer("_", "", "-", "", ".", "").Replace(a.Key))
			if strings.Contains(key, "apikey") || strings.Contains(key, "password") || strings.Contains(key, "authorization") || strings.Contains(key, "secret") || strings.Contains(key, "token") {
				value = "[REDACTED]"
			}
			m[a.Key] = value
		}
	}
	rec := severityRecord{Level: level, Message: msg, Attrs: m}
	rec.auditHead = w.newHead(auditWarning, time.Time{}, 0, false)
	if level == "error" {
		rec.Type = auditError
	}
	w.writeAudit(rec)
}

// writeAudit marshals and appends one record, reporting failure exactly once
// and then retiring the sink. Never call while holding w.mu — a failure has
// to be reportable through the (possibly composed) logger, which writes back
// into here.
func (w *Writer) writeAudit(v any) {
	w.mu.Lock()
	err := w.writeAuditLocked(v)
	w.mu.Unlock()
	w.reportAuditErr(err)
}

// writeAuditLocked is writeAudit with w.mu already held. A write failure
// disables the sink and stashes the error for reportAuditErr after the lock
// is released; it returns nil once disabled, so no further accounting.
func (w *Writer) writeAuditLocked(v any) error {
	if w.auditDisabled {
		return nil
	}
	if w.auditFailHook != nil {
		if err := w.auditFailHook(); err != nil {
			w.auditDisabled = true
			w.pendingAuditErr = err
			return err
		}
	}
	b, err := json.Marshal(v)
	if err == nil {
		b = append(b, '\n')
		_, err = w.auditBuf.Write(b)
	}
	if err != nil {
		w.auditDisabled = true
		w.pendingAuditErr = err
	}
	return err
}

// reportAuditErr surfaces the first audit failure outside the sink: once,
// through the normal logger, never back into the audit stream.
func (w *Writer) reportAuditErr(err error) {
	if err == nil {
		return
	}
	w.mu.Lock()
	w.metrics.SetAuditError(err)
	w.mu.Unlock()
	if w.OnAuditFail != nil {
		w.OnAuditFail(err)
	}
}

// flushPendingAuditErr reports an error stashed by writeAuditLocked while the
// caller held the lock. Call after unlocking.
func (w *Writer) flushPendingAuditErr() {
	w.mu.Lock()
	err := w.pendingAuditErr
	w.pendingAuditErr = nil
	w.mu.Unlock()
	w.reportAuditErr(err)
}

// AuditLogHandler tees warning and error diagnostics into the session audit
// stream. The wrapped handler stays authoritative for operator output; this
// one only mirrors what the spec calls operational degradation records.
type AuditLogHandler struct {
	Next   slog.Handler
	Writer *Writer
	attrs  []slog.Attr
}

func (h *AuditLogHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelWarn || h.Next.Enabled(ctx, l)
}

func (h *AuditLogHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		attrs := append([]slog.Attr(nil), h.attrs...)
		r.Attrs(func(a slog.Attr) bool { attrs = append(attrs, a); return true })
		level := "warning"
		if r.Level >= slog.LevelError {
			level = "error"
		}
		h.Writer.LogRecord(level, r.Message, attrs)
	}
	if h.Next.Enabled(ctx, r.Level) {
		return h.Next.Handle(ctx, r)
	}
	return nil
}

func (h *AuditLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	n := *h
	n.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	n.Next = h.Next.WithAttrs(attrs)
	return &n
}

func (h *AuditLogHandler) WithGroup(name string) slog.Handler {
	n := *h
	n.Next = h.Next.WithGroup(name)
	return &n
}
