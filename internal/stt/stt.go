// Package stt defines the speech-to-text abstraction. Adding a provider means
// writing one Engine and registering it; nothing else in the pipeline changes.
package stt

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"livecaption/internal/audio"
	"livecaption/internal/metrics"
)

// Transcript is one settled segment of speech: the engine only emits text it
// will not revise, so this is pure observation — what was heard and when —
// with no control flags left for the hub to interpret. Structure (row breaks,
// transcript line breaks) is derived downstream from Start/Duration and
// punctuation, not reported here.
type Transcript struct {
	Text string
	// Start and Duration are media time, so latency can be measured the same
	// way for a replayed file and a live capture.
	Start      time.Duration
	Duration   time.Duration
	Confidence float64
	ReceivedAt time.Time
	// CapturedAt is when the audio this transcript covers was released into the
	// pipeline. Zero when the engine could not resolve it; latency is then simply
	// not recorded, because a wrong number is worse than no number.
	CapturedAt time.Time
	// SentAt is when the audio this transcript covers was released to the
	// recognizer's socket. Together with CapturedAt and ReceivedAt it splits
	// total latency into upload / recognition phases. Zero when the engine
	// could not resolve it, in which case the split is simply not recorded.
	SentAt time.Time
}

// End is the media time of the last sample this transcript covers.
func (t Transcript) End() time.Duration { return t.Start + t.Duration }

// Config is what every engine needs to know to start recognizing.
type Config struct {
	Format   audio.Format
	Model    string
	Language string
	Keyterms []string // event-specific proper nouns
	APIKey   string
	Metrics  *metrics.Metrics
	Pause    PauseConfig
	// Endpointing is how long Deepgram waits for silence before finalizing a
	// chunk of speech. Lower puts words on screen sooner in smaller pieces;
	// see cli.STTFlags.Endpointing for the validated range.
	Endpointing time.Duration
}

// Engine consumes PCM frames and emits transcripts.
//
// Implementations own their own reconnect logic: Run should return only when
// ctx is cancelled or the frames channel closes, not on a dropped connection.
// Run must never block indefinitely on the frames channel — a slow engine must
// drop audio rather than stall capture.
type Engine interface {
	Name() string
	Run(ctx context.Context, frames <-chan audio.Frame, out chan<- Transcript) error
}

// Factory builds an engine from config.
type Factory func(Config) (Engine, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register makes an engine available by name. Called from provider package
// init functions, which main imports for side effects.
func Register(name string, f Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[name] = f
}

// New builds a registered engine.
func New(name string, cfg Config) (Engine, error) {
	registryMu.RLock()
	f, ok := registry[name]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown stt engine %q (available: %v)", name, Names())
	}
	return f(cfg)
}

// Names lists registered engines, for error messages and help text.
func Names() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
