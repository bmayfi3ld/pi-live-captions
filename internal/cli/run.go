package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"livecaption/internal/audio"
	"livecaption/internal/caption"
	"livecaption/internal/mdns"
	"livecaption/internal/metrics"
	"livecaption/internal/stt"
	"livecaption/internal/stt/deepgram"
	"livecaption/internal/stt/mock"
	"livecaption/internal/ui"
	"livecaption/internal/web"
)

// session is everything one run needs, assembled once and shared by both
// replay and live. The only real difference between the two commands is which
// audio.Source gets built.
type session struct {
	src     audio.Source
	monitor *audio.Monitor
	engine  stt.Engine
	hub     *caption.Hub
	writer  *caption.Writer
	server  *web.Server
	met     *metrics.Metrics
	term    *ui.Terminal
	log     *slog.Logger

	mdnsPub  *mdns.Publisher
	mdnsName string

	bannerFields []ui.BannerField
}

// buildOpts is the per-command part of the wiring.
type buildOpts struct {
	kind        string // "replay" or "live"
	sourceLabel string // banner value for the source row
	source      audio.Source
	monitor     *audio.Monitor
	mediaTotal  time.Duration
	conversion  string

	stt     STTFlags
	server  ServerFlags
	output  OutputFlags
	globals Globals
}

func newSession(o buildOpts, term *ui.Terminal, log *slog.Logger) (*session, error) {
	started := time.Now()
	met := metrics.New(Version, started.Format("2006-01-02T15-04-05"))
	met.SourceKind = o.kind
	met.SourceSpec = o.sourceLabel
	met.SourceFormat = o.conversion
	met.Engine = o.stt.Engine
	met.SetMediaTotal(o.mediaTotal)
	if o.monitor != nil {
		met.MonitorEnabled = true
	}

	hub := caption.NewHub(met)

	// Wired before anything starts so the very first SetSTTState call (idle ->
	// connecting) already reaches the viewer. Hub.PublishStatus takes its own
	// lock and never met's, so this can't deadlock against the metrics mutex.
	met.SetSTTStateHook(func(s metrics.ConnState) { hub.PublishStatus(s.String(), "") })

	engine, err := newEngine(o.stt.Engine, stt.Config{
		Format:   audio.PipelineFormat,
		Model:    o.stt.Model,
		Language: o.stt.Language,
		Keyterms: o.stt.Keyterm,
		APIKey:   o.stt.APIKey,
		Metrics:  met,
		Pause: stt.PauseConfig{
			Enabled: o.stt.AutoPause,
			Hold:    o.stt.SilenceHold,
		},
	})
	if err != nil {
		return nil, err
	}

	s := &session{
		src: o.source, monitor: o.monitor, engine: engine,
		hub: hub, met: met, term: term, log: log,
		mdnsName: o.server.MDNSName,
	}

	// Transcripts are recorded for every session unless explicitly disabled.
	if !o.output.NoTranscript {
		w, err := caption.NewWriter(o.output.TranscriptDir, started, met)
		if err != nil {
			return nil, err
		}
		s.writer = w
	}

	// One place decides what happens to a finalized line: it goes to the
	// terminal and to the transcript file.
	hub.OnFinal = func(l caption.Line) {
		term.Caption(time.Duration(l.OffsetMS)*time.Millisecond, l.Text)
		if s.writer != nil {
			s.writer.Write(l)
		}
	}

	srv, err := web.NewServer(web.Config{
		Addr:    o.server.Addr,
		Logo:    o.server.Logo,
		Hub:     hub,
		Metrics: met,
		Log:     log,
	})
	if err != nil {
		return nil, err
	}
	s.server = srv

	// Banner assembly. Everything the run depends on is shown before any
	// audio flows, so a misconfiguration is obvious immediately.
	fields := []ui.BannerField{{Label: "source", Value: o.kind + "  " + o.sourceLabel}}
	if o.monitor != nil {
		fields = append(fields, ui.BannerField{
			Label: "monitor",
			Value: audio.MonitorDescription,
			Note:  "perceived delay overstates actual by this much",
		})
	}
	sttNote := fmt.Sprintf("model=%s  language=%s", o.stt.Model, o.stt.Language)
	if len(o.stt.Keyterm) > 0 {
		sttNote += fmt.Sprintf("  keyterms=%d", len(o.stt.Keyterm))
	}
	fields = append(fields, ui.BannerField{Label: "stt", Value: o.stt.Engine, Note: sttNote})
	if s.writer != nil {
		fields = append(fields, ui.BannerField{Label: "transcript", Value: s.writer.Dir()})
	} else {
		fields = append(fields, ui.BannerField{Label: "transcript", Value: "disabled", Note: "--no-transcript"})
	}
	if o.server.Logo != "" {
		fields = append(fields, ui.BannerField{Label: "logo", Value: o.server.Logo})
	}
	base := browserURL(o.server.Addr)
	fields = append(fields,
		ui.BannerField{Label: "viewer", Value: base},
		ui.BannerField{Label: "admin", Value: base + "/admin"},
	)
	if o.server.MDNSName != "" {
		fields = append(fields, ui.BannerField{Label: "mdns", Value: o.server.MDNSName + ".local"})
	}
	s.bannerFields = fields
	return s, nil
}

// run drives the pipeline until the source ends or ctx is cancelled, then
// shuts every stage down in order so nothing in flight is lost.
func (s *session) run(ctx context.Context) error {
	s.term.Banner("livecaption "+Version, s.bannerFields)

	ln, err := s.server.Listen()
	if err != nil {
		return err
	}
	go func() {
		if err := s.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("web server stopped", "err", err)
		}
	}()
	s.mdnsPub = mdns.Start(s.mdnsName, s.log)

	if s.monitor != nil {
		if err := s.monitor.Start(ctx); err != nil {
			// Monitoring is a convenience; captions are the product.
			s.log.Warn("monitor playback unavailable", "err", err)
			s.monitor = nil
		}
	}

	frames, err := s.src.Start(ctx)
	if err != nil {
		return err
	}
	if s.monitor != nil {
		frames = s.monitor.Wrap(ctx, frames)
	}

	s.term.Ready("ready — Ctrl-C to stop")
	s.term.StartStatus(s.met.Snapshot)

	// The engine reads frames and writes transcripts; the hub consumes them.
	transcripts := make(chan stt.Transcript, 64)
	engineDone := make(chan error, 1)
	go func() { engineDone <- s.engine.Run(ctx, frames, transcripts) }()

	hubDone := make(chan struct{})
	go func() {
		defer close(hubDone)
		for t := range transcripts {
			// Publish first, then take the publish instant. Publish runs
			// synchronously all the way through broadcast (measured at 0.13ms
			// on loopback), so time.Now() here is that instant plus fan-out.
			// This makes the "assemble" phase very slightly over-count and the
			// viewer-side delivery figure very slightly under-count by that
			// same sub-millisecond amount — a real but negligible seam, and
			// cheaper than plumbing a callback out of the hub.
			s.hub.Publish(t)
			s.observeLatency(t, time.Now())
		}
	}()

	engineErr := <-engineDone
	close(transcripts)
	<-hubDone

	// Close any utterance still in progress so the tail is not lost.
	s.hub.Flush()

	if engineErr != nil && !errors.Is(engineErr, context.Canceled) {
		return engineErr
	}
	if err := s.src.Err(); err != nil {
		return err
	}
	return nil
}

// observeLatency records the wall-clock delay between the audio being
// captured and the caption segment for it arriving — the headline figure,
// and now the only one: every segment reaching the hub is already settled,
// so there is no earlier, more optimistic paint to track separately.
// Anchoring on the frame's capture instant — rather than on media time plus a
// stream origin — is what makes the figure survive auto-pause, reconnects,
// dropped frames and ffmpeg restarts, all of which move the recognizer's
// media clock relative to the wall.
//
// It additionally splits the total into upload / recognize / assemble phases
// using SentAt and publishedAt, when both are available.
func (s *session) observeLatency(t stt.Transcript, publishedAt time.Time) {
	// The empty-Text guard used to exist for Deepgram's synthetic
	// UtteranceEnd, which carried no media range and would let idx.At(0)
	// resolve an unrelated capture instant. UtteranceEnd is gone and
	// decodeTranscript already rejects a Results message with an empty
	// alternative, so nothing in this pipeline can reach here with an empty
	// Text today — this is now belt-and-braces against a future engine
	// emitting a synthetic zero-range result.
	if t.ReceivedAt.IsZero() || t.CapturedAt.IsZero() || t.Text == "" {
		return
	}
	s.met.ObserveLatency(t.ReceivedAt.Sub(t.CapturedAt))

	// The phase split needs SentAt as well; without it the total above is
	// still sound, we just can't attribute it.
	if t.SentAt.IsZero() || publishedAt.IsZero() {
		return
	}
	// upload + recognize + assemble must exactly equal publishedAt - CapturedAt:
	// the stacked bar on /admin sits directly under the headline total, so any
	// drift between them would be visible and wrong. Note the phase total
	// spans CapturedAt->publishedAt while the headline latency above spans
	// CapturedAt->ReceivedAt — that's intended, the bar shows one stage
	// further than the headline.
	s.met.ObservePhases(
		t.SentAt.Sub(t.CapturedAt),
		t.ReceivedAt.Sub(t.SentAt),
		publishedAt.Sub(t.ReceivedAt),
	)
}

// shutdown tears everything down and prints the summary. Called on the way out
// regardless of how the run ended.
func (s *session) shutdown() {
	if s.monitor != nil {
		_ = s.monitor.Close()
	}
	_ = s.src.Close()

	s.mdnsPub.Stop()

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.server.Shutdown(shutCtx)

	if s.writer != nil {
		if err := s.writer.Close(); err != nil {
			s.log.Warn("transcript close failed", "err", err)
		}
	}
	s.met.SetSTTState(metrics.StateClosed)
	s.term.Summary(s.met.Snapshot(), s.met.MonitorEnabled)
}

// browserURL turns a listen address into something clickable. ":8080" and
// "0.0.0.0:8080" both mean "this machine" to the person running the tool.
func browserURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// newEngine builds the recognizer named by --engine. The flag's enum keeps
// the default branch unreachable from the CLI; it exists so a bad programmatic
// caller gets a real error rather than a nil engine.
func newEngine(name string, cfg stt.Config) (stt.Engine, error) {
	switch name {
	case "deepgram":
		return deepgram.New(cfg), nil
	case "mock":
		return mock.New(cfg), nil
	default:
		return nil, fmt.Errorf("unknown stt engine %q (available: deepgram, mock)", name)
	}
}

// requireAPIKey fails early and clearly rather than letting the recognizer
// return an opaque 401 after audio has started flowing.
func requireAPIKey(engine, key string) error {
	if engine == "mock" || strings.TrimSpace(key) != "" {
		return nil
	}
	return errors.New("no Deepgram API key: set DEEPGRAM_API_KEY or pass --api-key " +
		"(or use --engine mock to run offline)")
}
