package audio

import (
	"bytes"
	"context"
	"math"
	"sync"
	"testing"
	"time"
)

func TestNoiseGate(t *testing.T) {
	g, err := NewNoiseGate(NoiseSettings{-45, 5})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	quiet, loud := squareWave(1600, 30), squareWave(1600, 3000)
	process := func(pcm []byte, seconds int, wantOpen bool) {
		t.Helper()
		original := bytes.Clone(pcm)
		f := Frame{PCM: pcm, Offset: time.Duration(seconds) * time.Second, CapturedAt: now}
		got := g.process(f, now)
		if !got.NoiseGated || got.NoiseGateOpen != wantOpen || got.Offset != f.Offset || got.CapturedAt != f.CapturedAt || len(got.PCM) != len(pcm) {
			t.Fatalf("frame metadata/open mismatch at %d", seconds)
		}
		if !bytes.Equal(pcm, original) {
			t.Fatal("mutated original PCM")
		}
		want := pcm
		if !wantOpen {
			want = make([]byte, len(pcm))
		}
		if !bytes.Equal(got.PCM, want) {
			t.Fatalf("wrong filtered PCM at %d", seconds)
		}
	}
	process(quiet, 0, false)
	process(loud, 1, true)
	for at := 2; at < 6; at++ {
		process(quiet, at, true)
	}
	process(quiet, 6, false) // Constant whispering cannot refresh release.
	process(loud, 7, true)
	process(loud, 11, true)
	process(quiet, 15, true)
	process(quiet, 16, false)
	process(loud, 20, true)
	process(quiet, 0, true) // Rebase a restarted source clock.
	process(quiet, 5, false)

	if err := g.Set(NoiseSettings{RMSDBFS(quiet), 0}); err != nil {
		t.Fatal(err)
	}
	process(quiet, 6, false) // Equality is not above threshold.
	process(loud, 7, true)
	process(quiet, 8, false)
	if err := g.Set(NoiseSettings{-45, 60}); err != nil {
		t.Fatal(err)
	}
	process(quiet, 9, false) // Longer release alone never reopens.
	process(loud, 10, true)
	if err := g.Set(NoiseSettings{-45, 1}); err != nil {
		t.Fatal(err)
	}
	process(quiet, 11, false) // Shorter release applies to the next frame.
	if err := g.Set(NoiseSettings{-70, 5}); err != nil {
		t.Fatal(err)
	}
	process(quiet, 12, true) // Lowering threshold opens on new input.
	if err := g.Set(NoiseSettings{0, 0}); err != nil {
		t.Fatal(err)
	}
	process(loud, 13, false)
}

func TestNoiseMeters(t *testing.T) {
	g, err := NewNoiseGate(NoiseSettings{-45, 5})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if s := g.snapshot(now); !s.Stale || !s.LastSampleAt.IsZero() || s.PeakDBFS != -100 {
		t.Fatal(s)
	}
	// A negative full-scale sample catches signed overflow in peak detection.
	g.process(Frame{PCM: []byte{0, 128, 0, 0}}, now)
	g.process(Frame{PCM: squareWave(1600, 30)}, now.Add(500*time.Millisecond))
	for range 2 {
		s := g.snapshot(now.Add(time.Second))
		if s.PeakDBFS != 0 || math.Abs(s.RMSDBFS-RMSDBFS(squareWave(1600, 30))) > 0.01 || s.Stale {
			t.Fatal(s)
		}
	}
	s := g.snapshot(now.Add(1200 * time.Millisecond))
	if s.PeakDBFS >= -50 {
		t.Fatal("old peak did not expire", s)
	}
	if s = g.snapshot(now.Add(3 * time.Second)); !s.Stale || s.PeakDBFS != -100 {
		t.Fatal(s)
	}
	if peakDBFS([]byte{0, 0, 255}) != -100 {
		t.Fatal("odd trailing byte counted")
	}
}

func TestNoiseSettingsValidationAndConcurrency(t *testing.T) {
	initial := NoiseSettings{-45, 5}
	g, err := NewNoiseGate(initial)
	if err != nil {
		t.Fatal(err)
	}
	for _, settings := range []NoiseSettings{{math.NaN(), 5}, {math.Inf(1), 5}, {-101, 5}, {1, 5}, {-45, -1}, {-45, 61}, {-45, math.NaN()}, {-45, math.Inf(1)}} {
		if g.Set(settings) == nil {
			t.Fatalf("accepted %+v", settings)
		}
		if g.Snapshot().Settings != initial {
			t.Fatal("invalid update changed settings")
		}
		if _, err := NewNoiseGate(settings); err == nil {
			t.Fatal("constructor accepted invalid settings")
		}
	}
	var wg sync.WaitGroup
	for worker := range 3 {
		wg.Go(func() {
			for i := range 100 {
				switch worker {
				case 0:
					if err := g.Set(NoiseSettings{-float64(i), float64(i % 60)}); err != nil {
						t.Error(err)
					}
				case 1:
					g.Process(Frame{PCM: squareWave(16, 30), Offset: time.Duration(i) * time.Second})
				case 2:
					g.Snapshot()
				}
			}
		})
	}
	wg.Wait()
	if g.Snapshot().Startup != initial {
		t.Fatal("startup defaults mutated")
	}
}

func TestNoiseWrapCancellation(t *testing.T) {
	g, err := NewNoiseGate(NoiseSettings{-45, 5})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan Frame, 1)
	in <- Frame{PCM: squareWave(16, 30)}
	out := g.Wrap(ctx, in)
	f := <-out
	if !f.NoiseGated || f.NoiseGateOpen {
		t.Fatal(f)
	}
	cancel()
	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("unexpected frame")
		}
	case <-time.After(time.Second):
		t.Fatal("wrapper did not stop")
	}
}
