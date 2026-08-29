package stt

import (
	"sync"
	"time"

	"livecaption/internal/audio"
)

// silenceThresholdDB is the dBFS at or below which a frame counts as silence.
// Low enough not to trip on room tone and HVAC, high enough that genuine dead
// air is recognized before it racks up recognizer charges. Calibrated against
// the soundboard feed: a materially hotter or colder input needs this moved.
const silenceThresholdDB = -45.0

// PauseConfig controls automatic pausing of the recognizer connection while
// the audio is silent.
type PauseConfig struct {
	Enabled bool
	Hold    time.Duration // continuous silence (media time) before pausing
}

// Gate tracks whether the stream currently carries audio worth transcribing.
// Transitions are driven by media time taken from frame offsets, never wall
// clock, so a live feed and a test feeding synthetic offsets behave
// identically — and a test can cover a minute of hold without waiting one.
type Gate struct {
	cfg PauseConfig

	mu        sync.Mutex
	active    bool
	inSilence bool
	silStart  time.Duration // media offset the current silence run began at
	changed   chan struct{}
}

// NewGate builds a Gate that starts Active: we have not heard silence yet, so
// the caller should connect immediately and only pause once the hold elapses.
func NewGate(cfg PauseConfig) *Gate {
	return &Gate{cfg: cfg, active: true, changed: make(chan struct{})}
}

// Observe feeds a real frame, computing its RMSDBFS. Reports whether Active()
// changed as a result.
func (g *Gate) Observe(f audio.Frame) bool {
	return g.ObserveLevel(audio.RMSDBFS(f.PCM), f.Offset)
}

// ObserveLevel feeds a level at a media time, reporting whether Active()
// changed as a result.
func (g *Gate) ObserveLevel(db float64, at time.Duration) bool {
	if !g.cfg.Enabled {
		return false
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	changed := false
	if db > silenceThresholdDB {
		// Resume must be instant: a delayed reaction here clips the first
		// word of the returning speech.
		g.inSilence = false
		if !g.active {
			g.active = true
			changed = true
		}
	} else {
		// ObserveLevel takes the media time from its caller, so a source that
		// restarts its clock hands us an offset behind the one the current
		// silence run began at. Re-baseline on that rather than measuring the
		// hold against the stale offset: the difference would go negative and
		// the gate would stop pausing for the rest of the run.
		if !g.inSilence || at < g.silStart {
			g.inSilence = true
			g.silStart = at
		}
		if g.active && at-g.silStart >= g.cfg.Hold {
			g.active = false
			changed = true
		}
	}

	if changed {
		close(g.changed)
		g.changed = make(chan struct{})
	}
	return changed
}

// Active reports whether audio is present and the connection should be kept
// transcribing.
func (g *Gate) Active() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.active
}

// Changed returns the current arm channel, closed on the next transition and
// replaced by a fresh one, so callers can select on it repeatedly.
func (g *Gate) Changed() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.changed
}
