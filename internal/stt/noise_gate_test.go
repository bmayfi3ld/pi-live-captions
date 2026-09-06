package stt

import (
	"bytes"
	"context"
	"testing"
	"time"

	"livecaption/internal/audio"
	"livecaption/internal/metrics"
)

func TestNoiseGatePreRollAndPause(t *testing.T) {
	noise, err := audio.NewNoiseGate(audio.NoiseSettings{ThresholdDBFS: -70, ReleaseSec: 5})
	if err != nil {
		t.Fatal(err)
	}
	pause := NewGate(PauseConfig{Enabled: true, Hold: time.Second})
	buf := newRing(4, metrics.New("test", "test"), pause)
	feed := func(at time.Duration, pcm []byte) {
		t.Helper()
		frames := make(chan audio.Frame, 1)
		frames <- audio.Frame{Offset: at, PCM: pcm}
		close(frames)
		<-startDrain(context.Background(), noise.Wrap(context.Background(), frames), pause, buf)
	}
	// ~-60 dBFS opens the caption gate despite being below old auto-pause -45.
	feed(0, []byte{30, 0})
	feed(2*time.Second, []byte{0, 0})
	if !pause.Active() {
		t.Fatal("auto-pause preceded noise gate closure")
	}
	feed(5*time.Second, []byte{1, 0}) // Caption gate closes, connection hold begins.
	if !pause.Active() {
		t.Fatal("connection hold was skipped")
	}
	feed(6*time.Second, []byte{1, 0})
	if pause.Active() {
		t.Fatal("connection did not pause after hold")
	}
	feed(7*time.Second, []byte{1, 0})
	feed(8*time.Second, []byte{30, 0}) // Resume, retaining filtered pre-roll.
	if !pause.Active() {
		t.Fatal("low-level caption input failed to resume")
	}
	first, ok := buf.pop()
	if !ok || !bytes.Equal(first.pcm, []byte{0, 0}) {
		t.Fatal("pre-roll leaked quiet PCM")
	}
	second, ok := buf.pop()
	if !ok || !bytes.Equal(second.pcm, []byte{30, 0}) {
		t.Fatal("resume lost opening frame")
	}
}
