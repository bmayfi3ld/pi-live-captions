package metrics

import (
	"testing"
	"time"
)

func TestLatencySessionMaxSurvivesWindowExpiry(t *testing.T) {
	m := New("test", "test")
	old := time.Now().Add(-2 * latencyWindow)
	m.latRecognize.observe(9*time.Second, old)
	m.latViewer.observe(3*time.Second, old)
	m.latRecognize.observe(100*time.Millisecond, time.Now())
	m.latViewer.observe(20*time.Millisecond, time.Now())
	s := m.Snapshot()
	if s.STT.RecognizeLatencySessionMax != 9000 || s.Web.ViewerLatencySessionMax != 3000 || s.STT.RecognizeLatencyMax != 100 || s.Web.ViewerLatencyMax != 20 {
		t.Fatalf("incorrect rolling/session maxima: %+v / %+v", s.STT, s.Web)
	}
	m.latRecognize.observe(10*time.Second, time.Now())
	if s := m.Snapshot(); s.STT.RecognizeLatencyLast != 10000 || s.STT.RecognizeLatencySessionMax != 10000 {
		t.Fatal("new peak did not update last value and session maximum")
	}
	if New("test", "new").Snapshot().STT.RecognizeLatencySessionMax != 0 {
		t.Fatal("new session retained previous peak")
	}
}
