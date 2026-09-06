package caption

import (
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"livecaption/internal/metrics"
	"livecaption/internal/stt"
)

func TestMarkersAndReconnect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := metrics.New("test", "markers")
		h := NewHub(m)
		w, err := NewWriter(t.TempDir(), time.Now(), m)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = w.Close() })
		h.OnFinal = w.Write
		h.OnMarker = w.Write

		checkSnapshot := func(want string) {
			t.Helper()
			ch, cancel := h.Subscribe()
			defer cancel()
			events := drain(ch)
			if len(events) != 2 || events[0].Kind != KindStatus || events[1].Kind != KindMusic || events[1].State != want || !events[0].Snapshot || !events[1].Snapshot {
				t.Fatalf("snapshot = %+v, want status and music %s", events, want)
			}
		}
		checkSnapshot("off")
		h.Publish(stt.Transcript{Words: stt.Untimed("before song"), Start: time.Second, Duration: time.Second, Speaker: 2})
		h.SetMusic(true, 2*time.Second)
		h.SetMusic(true, 3*time.Second) // repeated edge must not write again
		checkSnapshot("on")
		h.SetMusic(false, 3*time.Second) // music ends while the viewer is disconnected
		time.Sleep(musicEndHold)
		synctest.Wait()
		checkSnapshot("off")
		h.Publish(stt.Transcript{Words: stt.Untimed("before quiet"), Start: 4 * time.Second, Duration: time.Second, Speaker: 1})
		h.PublishStatus("paused")
		h.PublishStatus("paused")
		checkSnapshot("off")
		h.PublishStatus("connected")
		h.Publish(stt.Transcript{Words: stt.Untimed("after quiet."), Start: 6 * time.Second})
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(w.Dir(), "transcript.txt"))
		if err != nil {
			t.Fatal(err)
		}
		want := "[00:01] [S2] before song\n[00:02] ♪ music ♪\n[00:04] [S1] before quiet\n[00:05] — silence —\n[00:06] after quiet.\n"
		if string(data) != want {
			t.Fatalf("transcript = %q, want %q", data, want)
		}
		if m.Snapshot().STT.Lines != 3 {
			t.Fatal("markers counted as recognized speech")
		}
	})
}
