package caption

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"livecaption/internal/audio"
	"livecaption/internal/metrics"
)

// Writer records finalized captions to disk. Recording is on by default for
// every session — it is the expected behaviour, not something to remember to
// switch on — so the only configuration is where the files go.
//
// One file per session: transcript.txt, "[00:12:34] [S2] text". A parallel
// transcript.jsonl used to sit beside it, carrying the same three fields plus
// a synthetic line ID; nothing ever read it, so the file, the record type and
// the ID scheme all went together.
//
// Opened O_APPEND and flushed periodically, so a crash keeps everything
// already written.
const (
	musicMarker   = "♪ music ♪"
	silenceMarker = "— silence —"
)

type Writer struct {
	dir string
	// start is the session instant every audit record's elapsed_ms measures
	// from — the same clock the transcript directory name is built from.
	start time.Time

	mu         sync.Mutex
	txt        *os.File
	txtBuf     *bufio.Writer
	closed     bool
	lastMarker string

	// The audit stream shares the transcript's lifecycle, lock, and flush
	// cadence but none of its error policy: a failed audit write disables the
	// audit sink (once, reported through OnAuditFail after the lock is
	// released) and leaves transcript.txt untouched.
	auditFile       *os.File
	auditBuf        *bufio.Writer
	auditDisabled   bool
	pendingAuditErr error
	auditErr        error
	// auditFailHook is a test seam: when set, every write consults it before
	// touching the file, so a sink failure can be injected deterministically.
	auditFailHook func() error
	// OnAuditFail is called once, outside the writer lock, with the error that
	// retired the audit stream. The composed session logger reports it; the
	// handler's own write into the disabled sink is then a no-op, so the
	// report cannot recurse.
	OnAuditFail func(error)

	metrics *metrics.Metrics
	done    chan struct{}
}

// NewWriter creates <baseDir>/<RFC3339 session start>/ and opens the file.
func NewWriter(baseDir string, started time.Time, m *metrics.Metrics) (*Writer, error) {
	// Colons are legal on Linux but hostile in filenames, so the timestamp
	// uses dashes: 2026-08-19T09-31-05.
	name := started.Format("2006-01-02T15-04-05")
	dir := filepath.Join(baseDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create transcript dir: %w", err)
	}

	path := filepath.Join(dir, "transcript.txt")
	txt, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open transcript.txt: %w", err)
	}

	w := &Writer{
		dir:     dir,
		start:   started,
		txt:     txt,
		txtBuf:  bufio.NewWriter(txt),
		metrics: m,
		done:    make(chan struct{}),
	}
	m.TranscriptPath = path

	// The audit companion is best-effort from birth: if it cannot even be
	// created, the session runs on without it — captions are the product.
	auditPath := filepath.Join(dir, "audit.jsonl")
	auditFile, err := os.OpenFile(auditPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		w.auditDisabled = true
		w.auditErr = fmt.Errorf("open audit.jsonl: %w", err)
		m.SetAuditError(w.auditErr)
	} else {
		w.auditFile = auditFile
		w.auditBuf = bufio.NewWriter(auditFile)
	}

	// Periodic flush bounds how much a crash can cost to a couple of seconds.
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				w.flush()
			case <-w.done:
				return
			}
		}
	}()
	return w, nil
}

// Dir is the session directory, for the banner and summary.
func (w *Writer) Dir() string { return w.dir }

// AuditErr is the error that permanently disabled the audit stream, if one
// did — including a failure to create it at startup.
func (w *Writer) AuditErr() error { return w.auditErr }

// Write records one finalized line. Write errors are surfaced as a metric
// rather than returned: losing the transcript must not end a live event.
func (w *Writer) Write(l Line) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}

	marker := l.Text == musicMarker || l.Text == silenceMarker
	if marker && l.Text == w.lastMarker {
		w.mu.Unlock()
		return
	}

	clock := audio.FormatClock(time.Duration(l.OffsetMS) * time.Millisecond)
	// Spelled out as "[S2] " here, unlike the live viewer's terse per-word
	// badge: row width is scarce on screen, but a file read later has all the
	// space it needs, and "who said this" is exactly what a reader returning
	// to the transcript wants without cross-referencing anything else.
	speakerPrefix := ""
	if l.Speaker != 0 {
		speakerPrefix = fmt.Sprintf("[S%d] ", l.Speaker)
	}
	n, err := fmt.Fprintf(w.txtBuf, "[%s] %s%s\n", clock, speakerPrefix, l.Text)
	if err != nil {
		w.fail(err)
		w.mu.Unlock()
		w.flushPendingAuditErr()
		return
	}
	if marker {
		w.lastMarker = l.Text
	} else {
		w.lastMarker = ""
	}
	w.metrics.TranscriptWrote(1, n)
	// The audit record mirrors the line exactly as written above: same text,
	// same dedup (a compacted marker never existed as far as either file is
	// concerned), same timing.
	w.auditCaptionLine(l)
	w.mu.Unlock()
	w.flushPendingAuditErr()
}

// fail records a transcript write failure as a metric and as an audit error
// record. Caller holds w.mu; the transcript error must not retire the audit
// stream, which is why the audit write takes its own failure path.
func (w *Writer) fail(err error) {
	w.metrics.SetTranscriptError(err)
	rec := severityRecord{Level: "error", Message: "transcript write failed", Attrs: map[string]any{"err": err.Error()}}
	rec.auditHead = w.newHead(auditError, time.Time{}, 0, false)
	_ = w.writeAuditLocked(rec)
}

// Flush persists pending transcript and audit data so a following metrics
// snapshot includes any write failure discovered during shutdown.
func (w *Writer) Flush() { w.flush() }

func (w *Writer) flush() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	var auditErr error
	if err := w.txtBuf.Flush(); err != nil {
		w.fail(err)
	}
	if w.auditBuf != nil {
		if err := w.auditBuf.Flush(); err != nil && !w.auditDisabled {
			w.auditDisabled = true
			auditErr = err
		}
	}
	w.mu.Unlock()
	w.reportAuditErr(auditErr)
}

func (w *Writer) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	close(w.done)
	flushErr := w.txtBuf.Flush()
	if flushErr != nil {
		w.fail(flushErr)
	}
	closeErr := w.txt.Close()
	var auditErr error
	if w.auditBuf != nil {
		if err := w.auditBuf.Flush(); err != nil && !w.auditDisabled {
			w.auditDisabled = true
			auditErr = err
		}
		if cerr := w.auditFile.Close(); cerr != nil && auditErr == nil && !w.auditDisabled {
			auditErr = cerr
		}
	}
	pending := w.pendingAuditErr
	pendingReported := pending != nil
	w.mu.Unlock()

	if pendingReported {
		w.reportAuditErr(pending)
	}
	w.reportAuditErr(auditErr)

	if flushErr != nil {
		return flushErr
	}
	return closeErr
}
