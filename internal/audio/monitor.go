package audio

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
)

// monitorBufferMS is the playback buffer depth. Perceived caption delay
// overstates the real figure by this much, which is why MonitorDescription
// states it on the banner rather than hiding it.
const monitorBufferMS = 80

// MonitorDescription is the banner value for the monitor row. Playback is
// fixed at the default pulse sink, so there is nothing per-run to render.
const MonitorDescription = "pulse:default (~80ms buffer)"

// MonitorConfig configures speaker playback of the streamed audio.
type MonitorConfig struct {
	Log *slog.Logger

	OnDrop  func()
	OnAlive func(bool)
}

// Monitor plays the exact frames the pipeline is sending to the STT service,
// so caption delay can be judged by ear.
//
// The tap point is deliberate: playing the original file with a separate
// player would drift against our scheduler, so what you heard would not line
// up with what was sent. Teeing the frames we already emit means what you hear
// is bit-identical to what the
// recognizer receives, released by the same clock. It also means you hear the
// 16 kHz mono downmix, which is the point — bad source audio becomes audible
// instead of being inferred from bad transcripts.
type Monitor struct {
	cfg  MonitorConfig
	ch   chan []byte
	proc *proc

	// mu guards ch against the shutdown race: Tap sends from the pipeline
	// goroutine while Close runs on the main one, and shutdown() closes the
	// monitor *before* the audio source, so frames are still arriving. Only a
	// sender may close a channel, so Close takes this lock to make itself one.
	mu     sync.Mutex
	closed bool
}

func NewMonitor(cfg MonitorConfig) *Monitor {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	// Roughly two seconds of frames. Deep enough to ride out a scheduling
	// hiccup, shallow enough that we notice a genuinely stalled sink.
	return &Monitor{cfg: cfg, ch: make(chan []byte, 20)}
}

// SetCallbacks registers metric hooks. Set before Start.
func (m *Monitor) SetCallbacks(onDrop func(), onAlive func(bool)) {
	m.cfg.OnDrop, m.cfg.OnAlive = onDrop, onAlive
}

// Start launches the playback process and begins consuming tapped frames.
func (m *Monitor) Start(ctx context.Context) error {
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "s16le", "-ar", "16000", "-ac", "1", "-i", "-",
		"-f", "pulse",
		"-buffer_duration", strconv.Itoa(monitorBufferMS),
		"-name", "livecaption monitor",
		"default",
	}

	p, err := startFFmpeg(ctx, procOpts{args: args, wantStdin: true, log: m.cfg.Log})
	if err != nil {
		return fmt.Errorf("start monitor playback: %w", err)
	}
	m.proc = p
	m.setAlive(true)

	go func() {
		defer m.setAlive(false)
		for {
			select {
			case pcm, ok := <-m.ch:
				if !ok {
					return
				}
				if _, err := p.stdin.Write(pcm); err != nil {
					// A dead speaker must never end the session; captions
					// are the product, monitoring is a convenience.
					if ctx.Err() == nil {
						m.cfg.Log.Warn("monitor playback stopped", "err", err)
					}
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return nil
}

// Tap offers a frame to the monitor without ever blocking. If playback has
// stalled the frame is dropped and counted; the caption path is never held up
// by the sound card. After Close it is a no-op — a frame arriving during
// shutdown is neither played nor counted as a drop, since nothing degraded.
func (m *Monitor) Tap(pcm []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	select {
	case m.ch <- pcm:
	default:
		if m.cfg.OnDrop != nil {
			m.cfg.OnDrop()
		}
	}
}

// Wrap forwards frames unchanged while tapping each one for playback. It adds
// no latency to the main path: the tap is a non-blocking send.
func (m *Monitor) Wrap(ctx context.Context, in <-chan Frame) <-chan Frame {
	out := make(chan Frame)
	go func() {
		defer close(out)
		for f := range in {
			m.Tap(f.PCM)
			select {
			case out <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

func (m *Monitor) setAlive(v bool) {
	if m.cfg.OnAlive != nil {
		m.cfg.OnAlive(v)
	}
}

func (m *Monitor) Close() error {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		close(m.ch)
	}
	m.mu.Unlock()

	if m.proc != nil {
		return m.proc.Close()
	}
	return nil
}
