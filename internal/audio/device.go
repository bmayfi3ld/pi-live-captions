package audio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"time"
)

// DeviceConfig configures live capture from an audio input.
type DeviceConfig struct {
	Device  string // pulse sink/source name, or ALSA "hw:2,0"
	Backend string // "pulse" or "alsa"
	Log     *slog.Logger
	// Stream, when set, receives an MP3 encode of the captured audio off a
	// second ffmpeg output. Nil disables the second output entirely, leaving
	// the args byte-identical to a capture-only run.
	Stream *Broadcaster
	// BlockedReason is set when enumeration rejected this exact device. A
	// blocked source never launches ffmpeg and remains alive until cancellation.
	BlockedReason string

	OnFrame        func(nbytes int, offset time.Duration)
	OnXrun         func()
	OnRestart      func()
	OnStderr       func(string)
	OnAvailability func(state, reason string)
}

// DeviceSource captures live audio via ffmpeg.
//
// Unlike FileSource there is no pacing to do: the sound card releases samples
// at wall-clock rate and the reader simply keeps up. The work here is staying
// alive — if the USB interface is unplugged, ffmpeg exits, and we relaunch with
// backoff rather than ending the session.
type DeviceSource struct {
	cfg DeviceConfig

	mu   sync.Mutex
	proc *proc

	// offset accumulates across restarts so media time stays monotonic even
	// though each ffmpeg process starts counting from zero.
	offset time.Duration
}

func NewDeviceSource(cfg DeviceConfig) *DeviceSource {
	if cfg.Backend == "" {
		cfg.Backend = "pulse"
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &DeviceSource{cfg: cfg}
}

// DeviceCallbacks are the metric hooks for live capture. Every one of these
// corresponds to a way the capture path can degrade without the audio simply
// stopping, which is exactly what needs a counter behind it.
type DeviceCallbacks struct {
	OnFrame        func(nbytes int, offset time.Duration)
	OnXrun         func()
	OnRestart      func()
	OnStderr       func(string)
	OnAvailability func(state, reason string)
}

// SetCallbacks registers metric hooks. Set before Start.
func (s *DeviceSource) SetCallbacks(c DeviceCallbacks) {
	s.cfg.OnFrame = c.OnFrame
	s.cfg.OnXrun = c.OnXrun
	s.cfg.OnRestart = c.OnRestart
	s.cfg.OnStderr = c.OnStderr
	s.cfg.OnAvailability = c.OnAvailability
}

func (s *DeviceSource) Describe() string {
	return fmt.Sprintf("%s:%s (-> %s)", s.cfg.Backend, s.cfg.Device, trimDepth(PipelineFormat.String()))
}

func (s *DeviceSource) Start(ctx context.Context) (<-chan Frame, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, fmt.Errorf("start ffmpeg (is it installed and on PATH?): %w", err)
	}

	out := make(chan Frame)
	if s.cfg.BlockedReason != "" {
		go func() {
			<-ctx.Done()
			close(out)
		}()
		return out, nil
	}

	go func() {
		defer close(out)
		backoff := 250 * time.Millisecond
		for ctx.Err() == nil {
			err := s.captureOnce(ctx, out)
			if ctx.Err() != nil {
				return
			}
			if err == nil {
				err = errors.New("capture ended unexpectedly")
			}
			reason := err.Error()
			if s.cfg.OnAvailability != nil {
				s.cfg.OnAvailability("unavailable", reason)
			}
			s.cfg.Log.Warn("audio capture unavailable; retrying",
				"backend", s.cfg.Backend, "device", s.cfg.Device,
				"diagnostic", reason, "action", "retry", "retry_in", backoff)
			if s.cfg.OnRestart != nil {
				s.cfg.OnRestart()
			}
			if !sleep(ctx, backoff) {
				return
			}
			if backoff *= 2; backoff > 8*time.Second {
				backoff = 8 * time.Second
			}
		}
	}()
	return out, nil
}

func (s *DeviceSource) ffmpegArgs() ([]string, bool) {
	args := []string{"-hide_banner", "-loglevel", "error", "-f", s.cfg.Backend, "-i", s.cfg.Device}
	// ALSA can repeat packet timestamps. Derive output timestamps from sample
	// count so neither muxer receives duplicate DTS values.
	args = append(args, "-af", "asetpts=N/SR/TB", "-ac", "1", "-ar", "16000", "-f", "s16le", "-")
	// Second output off the same decode: full-quality MP3 for listeners.
	// Channels stay as the source's; -ar 44100 is a guard, since libmp3lame
	// rejects odd rates.
	aux := s.cfg.Stream != nil
	if aux {
		args = append(args, "-af", "asetpts=N/SR/TB", "-f", "mp3", "-b:a", "128k", "-ar", "44100", "pipe:3")
	}
	return args, aux
}

// captureOnce runs one ffmpeg lifetime.
func (s *DeviceSource) captureOnce(ctx context.Context, out chan<- Frame) error {
	args, aux := s.ffmpegArgs()
	p, err := startFFmpeg(ctx, procOpts{
		args:     args,
		extraOut: aux,
		log:      s.cfg.Log,
		onXrun:   s.cfg.OnXrun,
		onStderr: s.cfg.OnStderr,
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.proc = p
	s.mu.Unlock()

	buf := make([]byte, PipelineFormat.BytesFor(chunkSize))
	defer p.Close()
	capturing := false

	// One Run per ffmpeg lifetime: a capture restart ends this Run and the
	// next one picks up, while listeners' HTTP connections outlive both.
	if aux {
		go s.cfg.Stream.Run(ctx, p.aux)
	}

	for {
		read, err := io.ReadFull(p.stdout, buf)
		if read > 0 {
			if !capturing {
				capturing = true
				if s.cfg.OnAvailability != nil {
					s.cfg.OnAvailability("capturing", "")
				}
			}
			s.mu.Lock()
			s.offset += PipelineFormat.Duration(read)
			offset := s.offset
			s.mu.Unlock()

			if s.cfg.OnFrame != nil {
				s.cfg.OnFrame(read, offset)
			}
			select {
			case out <- Frame{
				PCM:        append([]byte(nil), buf[:read]...),
				Offset:     offset,
				CapturedAt: time.Now(),
			}:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				if msg := p.LastStderr(); msg != "" {
					return errors.New(msg)
				}
				return errors.New("capture ended unexpectedly")
			}
			return err
		}
	}
}

// Err always reports nil: live capture restarts ffmpeg forever rather than
// giving up, so the only way out of Start's loop is ctx cancellation. Present
// to satisfy Source, which FileSource does use meaningfully.
func (s *DeviceSource) Err() error { return nil }

func (s *DeviceSource) Close() error {
	s.mu.Lock()
	p := s.proc
	s.mu.Unlock()
	if p != nil {
		return p.Close()
	}
	return nil
}
