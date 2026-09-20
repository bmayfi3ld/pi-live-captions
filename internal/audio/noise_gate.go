package audio

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"sync"
	"time"
)

const (
	DefaultNoiseThresholdDBFS = -50.0
	DefaultNoiseRelease       = 5 * time.Second
)

// NoiseSettings controls caption audio, not the separate listener MP3 stream.
type NoiseSettings struct {
	ThresholdDBFS float64 `json:"threshold_dbfs"`
	ReleaseSec    float64 `json:"release_sec"`
}

func (s NoiseSettings) Validate() error {
	if math.IsNaN(s.ThresholdDBFS) || math.IsInf(s.ThresholdDBFS, 0) || s.ThresholdDBFS < -100 || s.ThresholdDBFS > 0 {
		return fmt.Errorf("noise threshold must be finite and between -100 and 0 dBFS")
	}
	if math.IsNaN(s.ReleaseSec) || math.IsInf(s.ReleaseSec, 0) || s.ReleaseSec < 0 || s.ReleaseSec > 60 {
		return fmt.Errorf("noise release must be finite and between 0 and 60 seconds")
	}
	return nil
}

type NoiseSnapshot struct {
	Settings     NoiseSettings `json:"settings"`
	Startup      NoiseSettings `json:"startup"`
	Defaults     NoiseSettings `json:"defaults"`
	Open         bool          `json:"open"`
	RMSDBFS      float64       `json:"rms_dbfs"`
	PeakDBFS     float64       `json:"peak_dbfs"`
	LastSampleAt time.Time     `json:"last_sample_at"`
	Stale        bool          `json:"stale"`
}

type peakSample struct {
	at time.Time
	db float64
}

// NoiseGate serializes frame processing and live settings. PCM is never mutated:
// the playback monitor may still own the original buffer.
type NoiseGate struct {
	mu                    sync.Mutex
	settings, startup     NoiseSettings
	open                  bool
	lastAbove, lastOffset time.Duration
	lastSample            time.Time
	rms                   float64
	peaks                 []peakSample

	// OnEdge fires exactly once per effective open/close transition, with the
	// frame's source position, the settings governing the transition, and the
	// wall-clock instant. The audit stream records these; nothing else
	// subscribes. Called with g.mu released, and only after Wrap/Process
	// begins, so assignment needs no synchronization.
	OnEdge func(open bool, at time.Duration, settings NoiseSettings, observed time.Time)
	// OnSettings fires after a successful Set, before any frame is processed
	// with the new values, so the audit stream records the configuration
	// ahead of the transitions that rely on it.
	OnSettings func(settings NoiseSettings)
}

func NewNoiseGate(settings NoiseSettings) (*NoiseGate, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	return &NoiseGate{settings: settings, startup: settings, rms: silenceFloor}, nil
}

func (g *NoiseGate) Set(settings NoiseSettings) error {
	if err := settings.Validate(); err != nil {
		return err
	}
	g.mu.Lock()
	g.settings = settings
	g.mu.Unlock()
	if g.OnSettings != nil {
		g.OnSettings(settings)
	}
	return nil
}

func (g *NoiseGate) Snapshot() NoiseSnapshot { return g.snapshot(time.Now()) }

func (g *NoiseGate) snapshot(now time.Time) NoiseSnapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.trimPeaks(now)
	peak := silenceFloor
	for _, p := range g.peaks {
		peak = max(peak, p.db)
	}
	return NoiseSnapshot{Settings: g.settings, Startup: g.startup,
		Defaults: NoiseSettings{DefaultNoiseThresholdDBFS, DefaultNoiseRelease.Seconds()},
		Open:     g.open, RMSDBFS: g.rms, PeakDBFS: peak, LastSampleAt: g.lastSample,
		Stale: g.lastSample.IsZero() || now.Sub(g.lastSample) > 2*time.Second}
}

func (g *NoiseGate) trimPeaks(now time.Time) {
	for len(g.peaks) > 0 && now.Sub(g.peaks[0].at) > time.Second {
		g.peaks = g.peaks[1:]
	}
}

func peakDBFS(pcm []byte) float64 {
	peak := 0.0
	for i := 0; i+1 < len(pcm); i += 2 {
		peak = max(peak, math.Abs(float64(int16(binary.LittleEndian.Uint16(pcm[i:])))))
	}
	if peak == 0 {
		return silenceFloor
	}
	return max(silenceFloor, 20*math.Log10(peak/fullScale16))
}

func (g *NoiseGate) Process(f Frame) Frame { return g.process(f, time.Now()) }

func (g *NoiseGate) process(f Frame, now time.Time) Frame {
	db, peak := RMSDBFS(f.PCM), peakDBFS(f.PCM)
	g.mu.Lock()
	if f.Offset < g.lastOffset {
		g.lastAbove = f.Offset
	}
	g.lastOffset = f.Offset
	g.lastSample, g.rms = now, db
	g.trimPeaks(now)
	g.peaks = append(g.peaks, peakSample{now, peak})
	wasOpen := g.open
	if db > g.settings.ThresholdDBFS {
		g.open, g.lastAbove = true, f.Offset
	} else if g.open && f.Offset-g.lastAbove >= time.Duration(g.settings.ReleaseSec*float64(time.Second)) {
		g.open = false
	}
	edge := g.open != wasOpen
	settings := g.settings
	at, observed := f.Offset, now
	f.NoiseGated, f.NoiseGateOpen = true, g.open
	if !g.open {
		f.PCM = make([]byte, len(f.PCM))
	}
	g.mu.Unlock()
	if edge && g.OnEdge != nil {
		g.OnEdge(g.open, at, settings, observed)
	}
	return f
}

func (g *NoiseGate) Wrap(ctx context.Context, in <-chan Frame) <-chan Frame {
	out := make(chan Frame)
	go func() {
		defer close(out)
		for {
			select {
			case f, ok := <-in:
				if !ok {
					return
				}
				f = g.Process(f)
				select {
				case out <- f:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}
