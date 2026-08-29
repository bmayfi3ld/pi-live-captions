// Package web serves the caption viewer, the admin metrics page and the SSE
// stream that drives them.
package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"livecaption/internal/caption"
	"livecaption/internal/metrics"
)

//go:embed static
var embedded embed.FS

// maxLogoBytes keeps a stray high-res logo from bloating server memory and
// every subsequent response; 2 MiB is generous for a corner-of-screen image.
const maxLogoBytes = 2 << 20

// Config configures the caption server.
type Config struct {
	Addr string
	Logo string // path to an image shown in the viewer's top-right corner
	// AdminPassword guards the admin page and the control API (user "admin").
	// Empty disables the control API outright rather than leaving it open —
	// the page itself stays reachable, since it is read-only.
	AdminPassword string
	Hub           *caption.Hub
	Metrics       *metrics.Metrics
	Log           *slog.Logger
}

// Server owns the HTTP surface.
type Server struct {
	cfg     Config
	http    *http.Server
	log     *slog.Logger
	logoSet bool

	viewerLatMu       sync.Mutex
	viewerLatWindowAt time.Time
	viewerLatCount    int
}

func NewServer(cfg Config) (*Server, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}

	static, err := fs.Sub(embedded, "static")
	if err != nil {
		return nil, fmt.Errorf("embedded assets: %w", err)
	}

	s := &Server{cfg: cfg, log: cfg.Log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/time", s.handleTime)
	mux.HandleFunc("POST /api/viewer-latency", s.handleViewerLatency)
	mux.Handle("POST /api/clear", s.requireAdmin(http.HandlerFunc(s.handleClear)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	adminPage, err := fs.ReadFile(static, "admin.html")
	if err != nil {
		return nil, fmt.Errorf("read admin.html: %w", err)
	}
	// The clear control is enabled by the server, once, at startup — not by
	// the page asking. With no password the marker stays a comment and the
	// button in the shipped markup is permanently disabled, with no request
	// that could fail, lag, or race it into looking live.
	if cfg.AdminPassword != "" {
		adminPage = bytes.Replace(adminPage, []byte(adminControlsMarker),
			[]byte(`<script>window.ADMIN_CONTROLS = true;</script>`), 1)
	}
	admin := blobHandler(adminPage, "text/html; charset=utf-8", "no-cache")
	// Only guarded once a password exists: with none set the page is still
	// worth reading (it is metrics only), and requireAdmin's no-password
	// branch would otherwise 503 the whole page instead of just the control.
	if cfg.AdminPassword != "" {
		admin = s.requireAdmin(admin)
	}
	mux.Handle("GET /admin", admin)
	// "/{$}" is an exact match, not a prefix: an unknown path falls through to
	// the mux's own 404 instead of silently rendering the viewer.
	mux.Handle("GET /{$}", pageHandler(static, "index.html"))

	for _, a := range []struct{ name, ctype string }{
		{"caption.js", "text/javascript; charset=utf-8"},
		{"wake.mp4", "video/mp4"},
		{"wake.webm", "video/webm"},
	} {
		body, err := fs.ReadFile(static, a.name)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", a.name, err)
		}
		mux.Handle("GET /"+a.name, blobHandler(body, a.ctype, "no-cache"))
	}

	if cfg.Logo != "" {
		body, ctype, err := readLogo(cfg.Logo)
		if err != nil {
			return nil, err
		}
		// Unlike the embedded assets, the logo is a stable per-event file the
		// viewer re-fetches on every reload, so it earns a real max-age.
		mux.Handle("GET /logo", blobHandler(body, ctype, "public, max-age=300"))
		s.logoSet = true
	}

	s.http = &http.Server{
		Handler: mux,
		// No write timeout: SSE connections are meant to stay open for the
		// length of an event.
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s, nil
}

// pageHandler serves one HTML file. Routing is the mux's job — both patterns
// registered above are exact matches, so nothing else reaches here.
func pageHandler(static fs.FS, name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := fs.ReadFile(static, name)
		if err != nil {
			http.Error(w, "asset not found: "+name, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(body)
	})
}

// blobHandler serves a fixed byte slice read once at startup, so nothing
// touches the disk on the hot path and the ETag can be computed up front. A
// logo swapped mid-session therefore has no effect until restart.
//
// The embedded assets pass "no-cache" deliberately rather than caching
// immutably: the bytes are fixed for a given binary, but the URL is not
// versioned, so a build that changes an asset (adding the silent audio track
// Gecko and WebKit require to grant the wake lock, say, or shipping a
// caption.js fix) would otherwise never reach a phone that had already cached
// the old file — the exact failure that made an earlier wake-lock fix look
// like a no-op. no-cache still lets the ETag turn the repeat request into a
// 304, and every asset here is a few KB on a LAN.
func blobHandler(body []byte, contentType, cacheControl string) http.Handler {
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:])[:16] + `"`

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", cacheControl)
		w.Header().Set("ETag", etag)
		// A fresh Reader per request: bytes.Reader carries a read position,
		// so sharing one across concurrent requests would race.
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
	})
}

// readLogo loads the operator-supplied logo and resolves its content type,
// rejecting anything large enough to be a mistake.
func readLogo(path string) ([]byte, string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read logo %s: %w", path, err)
	}
	if len(body) > maxLogoBytes {
		return nil, "", fmt.Errorf("logo %s is %d bytes, exceeds %d byte limit", path, len(body), maxLogoBytes)
	}
	return body, logoContentType(path, body), nil
}

// logoContentType prefers the file extension over sniffing, since a
// hand-picked logo is far more likely to have a correct extension than the
// magic-byte heuristics in http.DetectContentType are to guess right for it.
func logoContentType(path string, body []byte) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".svg":
		return "image/svg+xml"
	default:
		return http.DetectContentType(body)
	}
}

// Listen binds the address up front, so "port already in use" is reported
// before the banner claims the server is ready.
func (s *Server) Listen() (net.Listener, error) {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", s.cfg.Addr, err)
	}
	return ln, nil
}

func (s *Server) Serve(ln net.Listener) error { return s.http.Serve(ln) }

func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

// handleEvents streams caption events over Server-Sent Events.
//
// SSE rather than WebSocket because the traffic is strictly one-way and
// EventSource reconnects on its own — a browser that drops gets a fresh
// snapshot with no client-side reconnect logic to maintain.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Tell nginx and friends not to buffer, which would defeat the point.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	fmt.Fprint(w, "retry: 3000\n\n")
	flusher.Flush()

	events, unsubscribe := s.cfg.Hub.Subscribe()
	defer unsubscribe()

	s.log.Debug("viewer connected", "remote", r.RemoteAddr)
	defer s.log.Debug("viewer disconnected", "remote", r.RemoteAddr)

	// Comment-only heartbeat so idle proxies don't close the connection.
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()

	ctx := r.Context()
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			buf, err := json.Marshal(ev)
			if err != nil {
				s.log.Debug("encode event", "err", err)
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", buf); err != nil {
				return
			}
			flusher.Flush()
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-ctx.Done():
			return
		}
	}
}

// requireAdmin gates the operator controls behind HTTP basic auth.
//
// The threat model is one person stumbling onto the admin page during an
// event on the same LAN, not a determined attacker — basic auth over a local
// network covers exactly that and nothing more. With no password configured
// the control is refused rather than left open: an unauthenticated stranger
// blanking the screen mid-event is the failure this exists to prevent.
func (s *Server) requireAdmin(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AdminPassword == "" {
			http.Error(w, "admin controls disabled: set ADMIN_PASSWORD", http.StatusServiceUnavailable)
			return
		}
		user, pass, ok := r.BasicAuth()
		// Both halves compared in constant time, and both always compared:
		// returning early on a wrong username would leak which half failed.
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(adminUser)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(s.cfg.AdminPassword)) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="livecaption"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// adminControlsMarker is the placeholder in admin.html that NewServer swaps
// for the enabling script. It must stay in sync with the comment in that file.
const adminControlsMarker = "<!--ADMIN_CONTROLS-->"

// adminUser is fixed: one operator, one password, and a configurable username
// would be a second env var that adds nothing to the threat model above.
const adminUser = "admin"

// handleClear wipes every connected viewer's screen. No body, no parameters —
// the operator saw something that shouldn't be up there and wants it gone.
func (s *Server) handleClear(w http.ResponseWriter, r *http.Request) {
	s.cfg.Hub.Clear()
	s.log.Info("screen cleared by admin", "remote", r.RemoteAddr)
	w.WriteHeader(http.StatusNoContent)
}

// handleStats returns the metrics snapshot the admin page polls.
func (s *Server) handleStats(w http.ResponseWriter, _ *http.Request) {
	snap := s.cfg.Metrics.Snapshot()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(snap)
}

// handleConfig exposes the per-deployment facts the viewer can't know at build
// time: whether a logo was supplied, and which version is serving it. Row count
// is deliberately not here — it's a fixed constant the page owns, so shipping it
// over the wire would only make the page wait on a fetch to lay itself out.
func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	logo := ""
	if s.logoSet {
		logo = "/logo"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"version": s.cfg.Metrics.Version,
		"logo":    logo,
	})
}

// handleTime reports the server's own clock so the viewer can estimate its
// offset against it. Without this, a phone's clock skew (routinely seconds,
// sometimes minutes on a device that has never synced NTP) would leak
// straight into the viewer-latency measurement, and the page would end up
// reporting clock drift instead of the network-plus-render time it actually
// wants.
func (s *Server) handleTime(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"now": time.Now().Format(time.RFC3339Nano),
	})
}

// maxViewerLatencyBody bounds the request body a viewer can send to report
// its paint latency. The payload is one small number; a much larger body can
// only be a mistake or an attempt to make this unauthenticated endpoint do
// work it shouldn't.
const maxViewerLatencyBody = 4 << 10

// maxViewerLatencyMS rejects a report a real network-plus-render span could
// never produce. A tab backgrounded for an hour and then foregrounded will
// otherwise report its dormant time as "latency" and single-handedly poison
// the p95 the whole admin page reads.
const maxViewerLatencyMS = 60000

// viewerLatencyRateLimit and viewerLatencyRateWindow cap how many reports
// this handler will accept across ALL clients combined. The endpoint is
// unauthenticated on a LAN by design — anything on the network can POST to
// it — so a hostile or simply buggy client looping the request must not be
// able to spin the handler or the metrics lock arbitrarily fast.
const (
	viewerLatencyRateLimit  = 20
	viewerLatencyRateWindow = time.Second
)

// allowViewerLatencyReport is a simple fixed-window counter guarded by a
// mutex: cheap, dependency-free, and more than adequate for a limit this
// coarse (20/sec) — a token bucket would be more precise but this endpoint
// doesn't need the precision.
func (s *Server) allowViewerLatencyReport(now time.Time) bool {
	s.viewerLatMu.Lock()
	defer s.viewerLatMu.Unlock()
	if now.Sub(s.viewerLatWindowAt) >= viewerLatencyRateWindow {
		s.viewerLatWindowAt = now
		s.viewerLatCount = 0
	}
	if s.viewerLatCount >= viewerLatencyRateLimit {
		return false
	}
	s.viewerLatCount++
	return true
}

// handleViewerLatency accepts a viewer's own measurement of the span between
// a caption being published and it actually painting on that viewer's
// screen — the one leg of the pipeline the server cannot measure itself,
// since it has no visibility into a browser's render loop.
//
// This is the first POST route in the codebase, and the first endpoint
// reachable by anything that can talk to the LAN without authentication, so
// every guard here exists to stop a malformed or malicious body from
// corrupting the metrics every other viewer and the admin page rely on.
func (s *Server) handleViewerLatency(w http.ResponseWriter, r *http.Request) {
	// A viewer that has been asleep, or is simply hostile, must not be able
	// to make this handler read an unbounded body into memory.
	r.Body = http.MaxBytesReader(w, r.Body, maxViewerLatencyBody)

	var body struct {
		PaintMS float64 `json:"paint_ms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		// Both of JSON's "not a real number" edge cases land here already:
		// a magnitude like 1e999 makes strconv.ParseFloat return ErrRange,
		// which the encoding/json package surfaces as a decode error rather
		// than silently rounding to +Inf, and a bare `NaN` token is not
		// valid JSON syntax at all (JSON has no NaN/Infinity literals), so
		// it fails during tokenizing. Neither ever reaches body.PaintMS.
		http.Error(w, "malformed body", http.StatusBadRequest)
		return
	}
	// Defense in depth, not a path exercised by encoding/json today: if a
	// future Go version or a different decoder ever let a non-finite value
	// through, it must not be allowed to poison the metrics below it.
	if math.IsNaN(body.PaintMS) || math.IsInf(body.PaintMS, 0) {
		http.Error(w, "paint_ms must be finite", http.StatusBadRequest)
		return
	}
	if body.PaintMS < 0 || body.PaintMS > maxViewerLatencyMS {
		http.Error(w, "paint_ms out of range", http.StatusBadRequest)
		return
	}

	if !s.allowViewerLatencyReport(time.Now()) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}

	s.cfg.Metrics.ObserveViewerLatency(time.Duration(body.PaintMS * float64(time.Millisecond)))
	w.WriteHeader(http.StatusNoContent)
}
