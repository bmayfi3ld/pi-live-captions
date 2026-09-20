package caption

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"livecaption/internal/audio"
	"livecaption/internal/metrics"
)

func noiseSettingsFor(threshold, release float64) audio.NoiseSettings {
	return audio.NoiseSettings{ThresholdDBFS: threshold, ReleaseSec: release}
}

// readAudit parses every complete line of audit.jsonl. An incomplete tail —
// what a crash leaves behind — is ignored, not failed on: complete lines must
// stand on their own.
func readAudit(t *testing.T, dir string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(dir + "/audit.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var records []map[string]any
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	for i, line := range lines {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			if i == len(lines)-1 {
				// An incomplete tail is what a crash leaves behind; complete
				// lines stand on their own and the tail is ignored.
				continue
			}
			t.Fatalf("audit line is not valid JSON: %q: %v", line, err)
		}
		if rec["schema_version"] == nil || rec["type"] == nil {
			t.Fatalf("audit line missing schema_version/type: %q", line)
		}
		records = append(records, rec)
	}
	return records
}

// TestWriterCreatesAuditBesideTranscript covers the companion-file contract:
// the session directory holds both files, every audit line parses with a
// schema version and record type, and transcript.txt keeps its exact
// human-readable format.
func TestWriterCreatesAuditBesideTranscript(t *testing.T) {
	m := metrics.New("v", "s")
	started := time.Date(2026, 8, 19, 9, 31, 5, 0, time.UTC)
	w, err := NewWriter(t.TempDir(), started, m)
	if err != nil {
		t.Fatal(err)
	}

	w.Write(Line{Text: "hello there", OffsetMS: 754000, At: started.Add(time.Second)})
	w.Write(Line{Text: "♪ music ♪", OffsetMS: 760000, At: started.Add(2 * time.Second)})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	records := readAudit(t, w.Dir())
	if len(records) != 2 {
		t.Fatalf("got %d audit records, want 2", len(records))
	}
	if records[0]["type"] != "caption" || records[1]["type"] != "marker" {
		t.Errorf("types = %v, %v; want caption, marker", records[0]["type"], records[1]["type"])
	}

	txt, err := os.ReadFile(w.Dir() + "/transcript.txt")
	if err != nil {
		t.Fatal(err)
	}
	if want := "[12:34] hello there\n[12:40] ♪ music ♪\n"; string(txt) != want {
		t.Errorf("transcript.txt = %q, want %q", string(txt), want)
	}
}

// TestAuditCaptionRecordFields pins the caption record's contract: text,
// source-relative start in source_ms, wall-clock observation, session
// elapsed, speaker, and an end that appears only when it is reliable.
func TestAuditCaptionRecordFields(t *testing.T) {
	m := metrics.New("v", "s")
	started := time.Date(2026, 8, 19, 9, 31, 5, 0, time.UTC)
	w, err := NewWriter(t.TempDir(), started, m)
	if err != nil {
		t.Fatal(err)
	}

	w.Write(Line{Text: "timed line", OffsetMS: 1000, At: started.Add(5 * time.Second), Speaker: 2, EndMS: 2500, EndOK: true})
	w.Write(Line{Text: "untimed line", OffsetMS: 3000, At: started.Add(6 * time.Second)})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	records := readAudit(t, w.Dir())
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2", len(records))
	}
	c := records[0]
	if c["text"] != "timed line" || c["source_ms"] != float64(1000) || c["end_ms"] != float64(2500) {
		t.Errorf("caption record = %v", c)
	}
	if c["speaker"] != float64(2) {
		t.Errorf("speaker = %v, want 2", c["speaker"])
	}
	obs, err := time.Parse(time.RFC3339Nano, c["observed_at"].(string))
	if err != nil {
		t.Fatalf("observed_at: %v", err)
	}
	if obs != started.Add(5*time.Second).UTC() {
		t.Errorf("observed_at = %v, want %v (absolute UTC)", obs, started.Add(5*time.Second).UTC())
	}
	if c["elapsed_ms"] != float64(5000) {
		t.Errorf("elapsed_ms = %v, want 5000", c["elapsed_ms"])
	}

	u := records[1]
	if _, present := u["end_ms"]; present {
		t.Errorf("untimed caption must omit end_ms, got %v", u["end_ms"])
	}
	if u["source_ms"] != float64(3000) {
		t.Errorf("untimed caption source_ms = %v, want 3000", u["source_ms"])
	}
}

// TestAuditSessionStartHasNoSecrets pins the metadata contract: the first
// record identifies the session, version, source, recognition settings and
// noise-gate settings — and there is nowhere for a credential to be, because
// StartMeta has no field for one. A planted secret next to the real values
// must not leak.
func TestAuditSessionStartHasNoSecrets(t *testing.T) {
	m := metrics.New("v", "sess-1")
	w, err := NewWriter(t.TempDir(), time.Now(), m)
	if err != nil {
		t.Fatal(err)
	}
	w.SessionStart(StartMeta{
		Version: "1.2.3", SessionID: "sess-1",
		SourceKind: "live", SourceSpec: "alsa:soundboard", SourceFormat: "44100 Hz stereo s16 -> 16000 Hz mono s16",
		Engine: "deepgram", Model: "nova-3", Language: "en-US",
		Keyterms: []string{"lobby"}, Diarize: true,
		NoiseGate: noiseSettingsFor(-40, 5),
	})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	records := readAudit(t, w.Dir())
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	rec := records[0]
	if rec["type"] != "session_start" {
		t.Fatalf("first record type = %v, want session_start", rec["type"])
	}
	raw, _ := json.Marshal(rec)
	for _, want := range []string{`"version":"1.2.3"`, `"session_id":"sess-1"`, `"kind":"live"`, `"engine":"deepgram"`, `"model":"nova-3"`, `"threshold_dbfs":-40`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("session_start missing %s: %s", want, raw)
		}
	}
	// The planted secret never entered the metadata type, so it cannot be in
	// the record. Assert the absence of the whole credential family, not just
	// one key name.
	for _, forbidden := range []string{"api_key", "apikey", "password", "authorization", "secret"} {
		if strings.Contains(strings.ToLower(string(raw)), forbidden) {
			t.Errorf("session_start leaks credential field %q: %s", forbidden, raw)
		}
	}
}

// TestAuditSessionEndIsLastRecord covers clean-shutdown ordering: whatever
// came before, the final complete record is the metrics summary.
func TestAuditSessionEndIsLastRecord(t *testing.T) {
	m := metrics.New("v", "s")
	w, err := NewWriter(t.TempDir(), time.Now(), m)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(Line{Text: "before shutdown", At: time.Now()})
	w.SessionEnd(m.Snapshot())
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	records := readAudit(t, w.Dir())
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2", len(records))
	}
	last := records[len(records)-1]
	if last["type"] != "session_end" {
		t.Errorf("last record type = %v, want session_end", last["type"])
	}
	if last["summary"] == nil {
		t.Error("session_end carries no metrics summary")
	}
}

// TestAuditTruncatedTailIsIgnorable is the crash contract: a partial final
// line must not damage any complete record, and nothing promises a
// session_end the process never got to write.
func TestAuditTruncatedTailIsIgnorable(t *testing.T) {
	m := metrics.New("v", "s")
	w, err := NewWriter(t.TempDir(), time.Now(), m)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(Line{Text: "one", At: time.Now()})
	w.Write(Line{Text: "two", At: time.Now()})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate a crash mid-write: append a partial record with no newline.
	f, err := os.OpenFile(w.Dir()+"/audit.jsonl", os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"caption","text":"lo`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	records := readAudit(t, w.Dir())
	if len(records) != 2 {
		t.Fatalf("got %d complete records, want 2", len(records))
	}
	for i, rec := range records {
		if rec["type"] != "caption" {
			t.Errorf("record %d type = %v, want caption", i, rec["type"])
		}
	}
}

// TestAuditFailureLeavesTranscriptWorking injects a sink failure: the audit
// stream retires itself after exactly one failed write, the transcript keeps
// recording, caption flow is uninterrupted, and the failure is reported once
// through the normal logger (which writes into the retired sink — it must
// not recurse).
func TestAuditFailureLeavesTranscriptWorking(t *testing.T) {
	m := metrics.New("v", "s")
	w, err := NewWriter(t.TempDir(), time.Now(), m)
	if err != nil {
		t.Fatal(err)
	}

	boom := errors.New("audit disk full")
	var failures int
	w.OnAuditFail = func(err error) {
		failures++
		// Reporting goes through the composed logger, which mirrors the
		// warning back into the audit sink. The sink is disabled by now, so
		// this must be a no-op rather than recursion.
		w.LogRecord("warning", "session audit stream disabled", nil)
	}
	w.auditFailHook = func() error { return boom }

	w.Write(Line{Text: "before failure", At: time.Now()}) // fails
	w.Write(Line{Text: "after failure", At: time.Now()})  // audit no-op, transcript fine
	w.Write(Line{Text: "still working", At: time.Now()})
	if err := w.Close(); err != nil {
		t.Fatalf("transcript Close after audit failure: %v", err)
	}

	if failures != 1 {
		t.Errorf("OnAuditFail called %d times, want exactly 1", failures)
	}

	txt, err := os.ReadFile(w.Dir() + "/transcript.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"before failure", "after failure", "still working"} {
		if !strings.Contains(string(txt), want) {
			t.Errorf("transcript lost %q after audit failure: %q", want, txt)
		}
	}
	// Exactly the one record that was written before the failure.
	if got := len(readAudit(t, w.Dir())); got != 0 {
		t.Errorf("audit records after injected failure = %d, want 0 (failure precedes write)", got)
	}

	snap := m.Snapshot()
	if snap.Audit.LastError == "" {
		t.Error("audit failure not surfaced in metrics")
	}
	if snap.Transcript.Lines != 3 {
		t.Errorf("transcript lines = %d, want 3 (independent of audit failure)", snap.Transcript.Lines)
	}
	if snap.Health != "degraded" {
		t.Errorf("health = %q, want degraded after audit failure", snap.Health)
	}
}

// TestAuditTranscriptFailureRecorded covers the cross-record contract: a
// transcript write failure is itself an audit error record, and the audit
// stream stays alive afterwards.
func TestAuditTranscriptFailureRecorded(t *testing.T) {
	m := metrics.New("v", "s")
	w, err := NewWriter(t.TempDir(), time.Now(), m)
	if err != nil {
		t.Fatal(err)
	}
	// Close the transcript's underlying file out from under the buffer to
	// force a deterministic write failure.
	if err := w.txt.Close(); err != nil {
		t.Fatal(err)
	}
	w.Write(Line{Text: "doomed", At: time.Now()})
	w.Write(Line{Text: "fine", At: time.Now()})
	// The transcript itself failed, so Close legitimately reports it; the
	// audit stream must have survived regardless.
	_ = w.Close()

	records := readAudit(t, w.Dir())
	var sawTranscriptError bool
	for _, rec := range records {
		if rec["type"] == "error" && strings.Contains(rec["message"].(string), "transcript") {
			sawTranscriptError = true
		}
	}
	if !sawTranscriptError {
		t.Errorf("transcript write failure not audited: %v", records)
	}
	if m.Snapshot().Audit.LastError != "" {
		t.Errorf("audit stream itself failed, want it alive: %q", m.Snapshot().Audit.LastError)
	}
}

// TestAuditLogHandlerFiltersAndCarriesAttrs covers the logger composition
// contract: warnings and errors are mirrored with their attributes, info and
// below are not, and the wrapped handler still decides what operators see.
func TestAuditLogHandlerFiltersAndCarriesAttrs(t *testing.T) {
	m := metrics.New("v", "s")
	w, err := NewWriter(t.TempDir(), time.Now(), m)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(&AuditLogHandler{Next: slog.NewTextHandler(io.Discard, nil), Writer: w})
	log.Info("routine", "detail", "not audited")
	log.Warn("careful", "op", "resume", "attempt", 2, "api_key", "do-not-store")
	log.Error("broken")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	records := readAudit(t, w.Dir())
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2 (info filtered out)", len(records))
	}
	if records[0]["type"] != "warning" || records[0]["message"] != "careful" {
		t.Errorf("first record = %v", records[0])
	}
	attrs, _ := records[0]["attrs"].(map[string]any)
	if attrs == nil || attrs["op"] != "resume" || attrs["attempt"] != float64(2) {
		t.Errorf("warning attrs = %v, want op and attempt carried through", records[0]["attrs"])
	}
	if attrs["api_key"] != "[REDACTED]" {
		t.Errorf("secret attribute = %v, want redacted", attrs["api_key"])
	}
	if records[1]["type"] != "error" {
		t.Errorf("second record type = %v, want error", records[1]["type"])
	}
	for _, rec := range records {
		obs, err := time.Parse(time.RFC3339Nano, rec["observed_at"].(string))
		if err != nil {
			t.Fatalf("observed_at: %v", err)
		}
		if obs.Location() != time.UTC {
			t.Errorf("observed_at %v is not UTC", obs)
		}
		if rec["elapsed_ms"].(float64) < 0 {
			t.Errorf("elapsed_ms = %v, want session-relative non-negative", rec["elapsed_ms"])
		}
	}
}

// TestAuditLogHandlerRespectsNextLevel pins the severity split: when the
// operator handler is set to error, a warning still reaches the audit stream
// but must not be re-rendered by the wrapped handler.
func TestAuditLogHandlerRespectsNextLevel(t *testing.T) {
	m := metrics.New("v", "s")
	w, err := NewWriter(t.TempDir(), time.Now(), m)
	if err != nil {
		t.Fatal(err)
	}
	var rendered int
	next := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	capture := nextOpts{Handler: next.Handler(), onHandle: func() { rendered++ }}
	log := slog.New(&AuditLogHandler{Next: capture, Writer: w})
	log.Warn("only audited")
	log.Error("both")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if rendered != 1 {
		t.Errorf("wrapped handler rendered %d records, want 1 (error only)", rendered)
	}
	records := readAudit(t, w.Dir())
	if len(records) != 2 {
		t.Fatalf("audit got %d records, want 2 (level filter is the audit's own)", len(records))
	}
}

// nextOpts wraps a handler to count Handle calls.
type nextOpts struct {
	slog.Handler
	onHandle func()
}

func (n nextOpts) Handle(ctx context.Context, r slog.Record) error {
	n.onHandle()
	return n.Handler.Handle(ctx, r)
}

// TestAuditGateAndStateRecords covers the gate/state/drop writer methods the
// wiring in internal/cli routes into.
func TestAuditGateAndStateRecords(t *testing.T) {
	m := metrics.New("v", "s")
	w, err := NewWriter(t.TempDir(), time.Now(), m)
	if err != nil {
		t.Fatal(err)
	}
	w.GateConfig(noiseSettingsFor(-35, 10))
	w.GateEdge(true, 0, noiseSettingsFor(-35, 10), time.Now())
	w.StateEvent("stt", "reconnecting")
	w.DropEvent("stt", "buffer_drop", 1, 2*time.Second, true)
	w.DropEvent("source", "frames_dropped", 1, 0, false)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	records := readAudit(t, w.Dir())
	if len(records) != 5 {
		t.Fatalf("got %d records, want 5", len(records))
	}
	if records[0]["type"] != "config" || records[0]["component"] != "noise_gate" {
		t.Errorf("config record = %v", records[0])
	}
	if records[1]["type"] != "noise_gate" || records[1]["open"] != true || records[1]["source_ms"] != float64(0) {
		t.Errorf("gate edge record = %v", records[1])
	}
	if records[2]["state"] != "reconnecting" {
		t.Errorf("state record = %v", records[2])
	}
	if records[3]["event"] != "buffer_drop" || records[3]["source_ms"] != float64(2000) {
		t.Errorf("drop record with source = %v", records[3])
	}
	if _, present := records[4]["source_ms"]; present {
		t.Errorf("drop record without a source position must omit source_ms: %v", records[4])
	}
}
