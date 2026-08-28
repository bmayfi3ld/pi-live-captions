package caption

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"livecaption/internal/metrics"
)

// TestWriterCreatesSessionFileWithExpectedFormat covers the contract the rest
// of the tool depends on: a session directory named after the start time,
// holding a transcript in the documented format.
func TestWriterCreatesSessionFileWithExpectedFormat(t *testing.T) {
	m := metrics.New("v", "s")
	started := time.Date(2026, 8, 19, 9, 31, 5, 0, time.UTC)
	w, err := NewWriter(t.TempDir(), started, m)
	if err != nil {
		t.Fatal(err)
	}

	at := time.Date(2026, 8, 19, 9, 32, 0, 0, time.UTC)
	w.Write(Line{Text: "hello there", OffsetMS: 754000, At: at})

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if !strings.HasSuffix(w.Dir(), "2026-08-19T09-31-05") {
		t.Errorf("session dir = %q, want it named after the start time", w.Dir())
	}

	txt, err := os.ReadFile(filepath.Join(w.Dir(), "transcript.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// [mm:ss] text — FormatClock(754s) = 12:34.
	if want := "[12:34] hello there\n"; string(txt) != want {
		t.Errorf("transcript.txt = %q, want %q", string(txt), want)
	}

}

// TestWriterSpeakerPrefix pins the one piece of formatting the plain-text file
// carries beyond clock and text: who was speaking, spelled out for a reader
// returning to the transcript later.
func TestWriterSpeakerPrefix(t *testing.T) {
	m := metrics.New("v", "s")
	w, err := NewWriter(t.TempDir(), time.Now(), m)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(Line{Text: "over here", OffsetMS: 0, At: time.Now(), Speaker: 2})
	w.Write(Line{Text: "unattributed", OffsetMS: 0, At: time.Now()})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	txt, err := os.ReadFile(filepath.Join(w.Dir(), "transcript.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := "[00:00] [S2] over here\n[00:00] unattributed\n"
	if string(txt) != want {
		t.Errorf("transcript.txt = %q, want %q", string(txt), want)
	}
}

// TestWriterFlushesOnClose is the crash-safety guarantee: content must reach
// disk when the session ends, without waiting for the periodic 2s flush.
func TestWriterFlushesOnClose(t *testing.T) {
	m := metrics.New("v", "s")
	w, err := NewWriter(t.TempDir(), time.Now(), m)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(Line{Text: "not yet flushed by the ticker", At: time.Now()})
	dir := w.Dir()

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	txt, err := os.ReadFile(filepath.Join(dir, "transcript.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(txt), "not yet flushed by the ticker") {
		t.Errorf("buffered content did not survive Close: %q", txt)
	}
}

// TestWriterMetricsTrackLinesAndBytes drives the transcript row on /admin and
// in the shutdown summary — it must reflect exactly what was written.
func TestWriterMetricsTrackLinesAndBytes(t *testing.T) {
	m := metrics.New("v", "s")
	w, err := NewWriter(t.TempDir(), time.Now(), m)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	w.Write(Line{Text: "one", At: time.Now()})
	w.Write(Line{Text: "two", At: time.Now()})

	snap := m.Snapshot()
	if snap.Transcript.Lines != 2 {
		t.Errorf("lines_written = %d, want 2", snap.Transcript.Lines)
	}
	if snap.Transcript.Bytes <= 0 {
		t.Errorf("bytes_written = %d, want > 0", snap.Transcript.Bytes)
	}
	if snap.Transcript.Path == "" {
		t.Error("transcript path should be set on the metrics for the banner")
	}
}

// TestWriterCloseIsIdempotentAndWriteAfterCloseIsSafe covers shutdown races:
// Close may be invoked from more than one unwind path, and a caption that
// arrives just after shutdown must be dropped quietly, not panic the pipeline.
func TestWriterCloseIsIdempotentAndWriteAfterCloseIsSafe(t *testing.T) {
	m := metrics.New("v", "s")
	w, err := NewWriter(t.TempDir(), time.Now(), m)
	if err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	w.Write(Line{Text: "after close", At: time.Now()})

	txt, err := os.ReadFile(filepath.Join(w.Dir(), "transcript.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(txt), "after close") {
		t.Error("a write after Close should be dropped, not appended to a closed file")
	}
}

// TestWriterContentSurvivesMultipleWrites is a light end-to-end check that two
// lines land in order, which the format tests above assume but don't directly
// exercise across more than one write.
func TestWriterContentSurvivesMultipleWrites(t *testing.T) {
	m := metrics.New("v", "s")
	w, err := NewWriter(t.TempDir(), time.Now(), m)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(Line{Text: "first", At: time.Now()})
	w.Write(Line{Text: "second", At: time.Now()})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	txt, err := os.ReadFile(filepath.Join(w.Dir(), "transcript.txt"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(txt), "\n"), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], "first") || !strings.HasSuffix(lines[1], "second") {
		t.Errorf("transcript.txt = %q, want first then second in order", string(txt))
	}
}
