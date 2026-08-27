package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"livecaption/internal/caption"
	"livecaption/internal/metrics"
	"livecaption/internal/stt"
)

// startTestServer boots the server on an ephemeral port using the same
// Listen/Serve path main.go uses, so the tests exercise the real HTTP
// surface rather than a mux built by hand.
func startTestServer(t *testing.T, cfg Config) (baseURL string, hub *caption.Hub, m *metrics.Metrics) {
	t.Helper()
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:0"
	}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ln, err := s.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go s.Serve(ln)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.Shutdown(ctx)
	})
	return "http://" + ln.Addr().String(), cfg.Hub, cfg.Metrics
}

func newTestConfig() Config {
	m := metrics.New("test-version", "test-session")
	return Config{
		Hub:     caption.NewHub(m),
		Metrics: m,
	}
}

func TestHealthzReturns200(t *testing.T) {
	base, _, _ := startTestServer(t, newTestConfig())
	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestUnknownPathReturns404 guards against a typo'd URL silently rendering
// the viewer, which would be confusing rather than obviously wrong.
func TestUnknownPathReturns404(t *testing.T) {
	base, _, _ := startTestServer(t, newTestConfig())
	resp, err := http.Get(base + "/this-route-does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestAPIStatsReturnsSnapshotJSON is what the admin page polls once a second;
// it must decode straight into the Snapshot shape.
func TestAPIStatsReturnsSnapshotJSON(t *testing.T) {
	base, _, _ := startTestServer(t, newTestConfig())
	resp, err := http.Get(base + "/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var snap metrics.Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Version != "test-version" {
		t.Errorf("version = %q, want %q", snap.Version, "test-version")
	}
}

// TestAPIStatsCarriesAutoPauseFields covers the two new /api/stats fields
// auto-pause needs: pauses_total (how many times the link auto-paused) and
// paused_sec (total time spent paused, including a pause in progress), plus
// "paused" as a valid stt.state value. All three ride the same Snapshot the
// admin page and status line read from, so if they decode here they're
// available everywhere.
func TestAPIStatsCarriesAutoPauseFields(t *testing.T) {
	cfg := newTestConfig()
	base, _, m := startTestServer(t, cfg)

	m.SetSTTState(metrics.StatePaused)
	m.STTPauseBegin()

	resp, err := http.Get(base + "/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var snap metrics.Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if snap.STT.State != "paused" {
		t.Errorf("stt.state = %q, want %q", snap.STT.State, "paused")
	}
	if snap.STT.Pauses != 1 {
		t.Errorf("stt.pauses_total = %d, want 1", snap.STT.Pauses)
	}
	// The pause begun above is still open (no matching STTPauseEnd), so
	// paused_sec must already reflect it rather than staying at zero until
	// the pause closes.
	if snap.STT.PausedSec <= 0 {
		t.Errorf("stt.paused_sec = %v, want > 0 for a pause in progress", snap.STT.PausedSec)
	}
}

// TestEventsRelaysPausedStatus proves the SSE contract for auto-pause: a
// "paused" status published to the hub — as internal/cli/run.go's
// metrics-state hook now does for real, per Hub.PublishStatus's doc comment
// on what wires into it — must reach a connected subscriber as a "status"
// event with state "paused" and, per the contract, an empty detail: wording
// ("no audio" / "paused (no audio)") is each page's job, not the server's.
func TestEventsRelaysPausedStatus(t *testing.T) {
	cfg := newTestConfig()
	base, hub, _ := startTestServer(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	nextSSEData(t, r) // discard the initial snapshot

	hub.PublishStatus("paused", "")

	data := nextSSEData(t, r)
	var ev caption.Event
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		t.Fatalf("status event is not valid JSON: %v (%q)", err, data)
	}
	if ev.Kind != caption.KindStatus {
		t.Fatalf("event kind = %q, want %q", ev.Kind, caption.KindStatus)
	}
	if ev.State != "paused" {
		t.Errorf("event state = %q, want %q", ev.State, "paused")
	}
	if ev.Detail != "" {
		t.Errorf("event detail = %q, want empty — the page decides the wording", ev.Detail)
	}
}

// TestEventsSnapshotReplaysLastStatus is the actual regression guard for the
// hub-memory bug: publish "paused" BEFORE any client connects, then confirm
// the very first event a new subscriber receives — the snapshot — already
// carries state "paused". TestEventsRelaysPausedStatus above connects first
// and publishes second, so it cannot catch a client that missed the edge
// (e.g. a page loaded mid-pause, or an EventSource that reconnected mid-pause
// after a phone's screen slept).
func TestEventsSnapshotReplaysLastStatus(t *testing.T) {
	cfg := newTestConfig()
	base, hub, _ := startTestServer(t, cfg)

	hub.PublishStatus("paused", "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)

	data := nextSSEData(t, r)
	var ev caption.Event
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		t.Fatalf("snapshot event is not valid JSON: %v (%q)", err, data)
	}
	if ev.Kind != caption.KindSnapshot {
		t.Fatalf("event kind = %q, want %q", ev.Kind, caption.KindSnapshot)
	}
	if ev.State != "paused" {
		t.Errorf("snapshot state = %q, want %q — a late joiner must learn the current state from the snapshot, not wait for a change that may never come", ev.State, "paused")
	}
}

// TestEventsSnapshotOmitsStateWhenNonePublished confirms the replay in
// Subscribe doesn't change the wire format for the common case: a client
// connecting before any status has ever been published still gets a
// snapshot with no "state" field at all (omitempty), not state: "".
func TestEventsSnapshotOmitsStateWhenNonePublished(t *testing.T) {
	base, _, _ := startTestServer(t, newTestConfig())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)

	data := nextSSEData(t, r)
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		t.Fatalf("snapshot event is not valid JSON: %v (%q)", err, data)
	}
	if _, present := raw["state"]; present {
		t.Errorf("snapshot JSON has a %q key when no status was ever published, want it omitted entirely: %q", "state", data)
	}
}

// TestEventsHeadersAndSnapshotFirst covers the SSE contract every viewer
// relies on: the right headers to survive proxies, and a snapshot as the
// very first event so a page load is never blank.
func TestEventsHeadersAndSnapshotFirst(t *testing.T) {
	cfg := newTestConfig()
	// Seed history before any client connects, so the snapshot has to carry
	// something for a late joiner.
	cfg.Hub.Publish(stt.Transcript{Text: "already said"})
	base, _, _ := startTestServer(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
	if xa := resp.Header.Get("X-Accel-Buffering"); xa != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", xa)
	}

	data := nextSSEData(t, bufio.NewReader(resp.Body))
	var ev caption.Event
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		t.Fatalf("first event is not valid JSON: %v (%q)", err, data)
	}
	if ev.Kind != caption.KindSnapshot {
		t.Fatalf("first event kind = %q, want snapshot", ev.Kind)
	}
	if ev.Text != "already said" {
		t.Errorf("snapshot text = %q, want the seeded history %q", ev.Text, "already said")
	}
}

// TestEventsDeliversPublishedEvents proves the stream isn't just the initial
// snapshot: events published after connecting must reach the client too.
func TestEventsDeliversPublishedEvents(t *testing.T) {
	cfg := newTestConfig()
	base, hub, _ := startTestServer(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	nextSSEData(t, r) // discard the initial snapshot

	hub.Publish(stt.Transcript{Text: "live line"})

	data := nextSSEData(t, r)
	var ev caption.Event
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		t.Fatalf("published event is not valid JSON: %v (%q)", err, data)
	}
	if ev.Kind != caption.KindCaption || ev.Text != "live line" {
		t.Errorf("event = %+v, want a caption event with text %q", ev, "live line")
	}
}

// TestEventsCarrySpeakerOnTheWire pins the JSON, not the struct: the viewer
// reads ev.speaker off the raw payload, and `omitempty` on an int means an
// unknown speaker must vanish from the wire rather than arrive as a literal
// 0 the client would have to know to ignore. Both halves are checked here
// because a wrong tag breaks exactly one of them.
func TestEventsCarrySpeakerOnTheWire(t *testing.T) {
	cfg := newTestConfig()
	base, hub, _ := startTestServer(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	nextSSEData(t, r) // discard the initial snapshot

	hub.Publish(stt.Transcript{Text: "the guest speaks", Speaker: 2})
	data := nextSSEData(t, r)
	if !strings.Contains(data, `"speaker":2`) {
		t.Errorf("caption event = %q, want it to carry \"speaker\":2", data)
	}

	// A segment the recognizer could not attribute. Speaker 0 is this
	// pipeline's "unknown" sentinel, never a real speaker, so it must not
	// appear at all — a `"speaker":0` on the wire would draw a badge for a
	// turn change that never happened.
	hub.Publish(stt.Transcript{Text: "unattributed audio", Start: 200 * time.Millisecond})
	data = nextSSEData(t, r)
	if strings.Contains(data, "speaker") {
		t.Errorf("caption event = %q, want no speaker field for an unknown speaker", data)
	}
}

// TestClientDisconnectCleansUpSubscription is the fan-out half of "nothing
// downstream may block the pipeline": a browser that goes away must not
// leak its subscriber slot or leave the SSE client gauge stuck non-zero.
func TestClientDisconnectCleansUpSubscription(t *testing.T) {
	cfg := newTestConfig()
	base, _, m := startTestServer(t, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(resp.Body)
	nextSSEData(t, r) // wait for the subscription to actually be established

	if got := m.SSEClients(); got != 1 {
		t.Fatalf("sse_clients = %d after connect, want 1", got)
	}

	resp.Body.Close()
	cancel()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if m.SSEClients() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("sse_clients = %d after disconnect, want 0", m.SSEClients())
}

// TestLogoUnsetReturns404 makes sure an unconfigured logo falls through to
// pageHandler's catch-all 404 rather than serving something unexpected.
func TestLogoUnsetReturns404(t *testing.T) {
	base, _, _ := startTestServer(t, newTestConfig())
	resp, err := http.Get(base + "/logo")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestLogoServedWithETagAndConditionalRequest covers the whole contract: a
// configured logo is served with the right Content-Type and a stable ETag,
// and a follow-up conditional request gets 304 rather than the body again.
func TestLogoServedWithETagAndConditionalRequest(t *testing.T) {
	cfg := newTestConfig()
	cfg.Logo = "testdata/logo.png"
	base, _, _ := startTestServer(t, cfg)

	resp, err := http.Get(base + "/logo")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("ETag header is empty, want a value")
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) == 0 {
		t.Error("logo body is empty, want image bytes")
	}

	req, _ := http.NewRequest(http.MethodGet, base+"/logo", nil)
	req.Header.Set("If-None-Match", etag)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Errorf("conditional status = %d, want 304", resp2.StatusCode)
	}
}

// TestAPIConfigReportsLogoState covers both sides of the logo contract the
// viewer relies on: "" unset, "/logo" set.
func TestAPIConfigReportsLogoState(t *testing.T) {
	base, _, _ := startTestServer(t, newTestConfig())
	resp, err := http.Get(base + "/api/config")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["logo"] != "" {
		t.Errorf(`logo = %v, want ""`, got["logo"])
	}

	cfg := newTestConfig()
	cfg.Logo = "testdata/logo.png"
	base2, _, _ := startTestServer(t, cfg)
	resp2, err := http.Get(base2 + "/api/config")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var got2 map[string]any
	if err := json.NewDecoder(resp2.Body).Decode(&got2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got2["logo"] != "/logo" {
		t.Errorf(`logo = %v, want "/logo"`, got2["logo"])
	}
}

// TestWakeVideoServedWithContentType covers Backend B's asset route: the
// silent looping video that keeps a phone screen on over plain HTTP must be
// reachable at a fixed URL with the right Content-Type so it can autoplay.
func TestWakeVideoServedWithContentType(t *testing.T) {
	base, _, _ := startTestServer(t, newTestConfig())
	resp, err := http.Get(base + "/wake.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "video/mp4" {
		t.Errorf("Content-Type = %q, want video/mp4", ct)
	}
	// The URL carries no version, so an immutable response would pin an
	// outdated asset on every phone that ever loaded the page — the failure
	// mode that made the audio-track fix below look like it had done
	// nothing.
	if cc := resp.Header.Get("Cache-Control"); strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, want a revalidating directive at an unversioned URL", cc)
	}
}

// TestCaptionJSServedWithContentType covers the /caption.js route added
// alongside the wake-video assets: staticAssetHandler now serves more than
// one kind of file, and the typesetter both pages depend on must actually be
// reachable at a fixed URL with a script Content-Type, or the page loads
// with a blank stack and no console hint why.
func TestCaptionJSServedWithContentType(t *testing.T) {
	base, _, _ := startTestServer(t, newTestConfig())
	resp, err := http.Get(base + "/caption.js")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("Content-Type = %q, want a text/javascript prefix", ct)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("ETag header is empty, want a value")
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("CaptionStack")) {
		t.Error("caption.js body does not mention CaptionStack, want the factory this route exists to serve")
	}

	req, _ := http.NewRequest(http.MethodGet, base+"/caption.js", nil)
	req.Header.Set("If-None-Match", etag)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Errorf("conditional status = %d, want 304", resp2.StatusCode)
	}
}

// TestWakeAssetsCarryAudioTrack pins the least obvious property of the wake
// assets. Gecko grants the screen lock only for video that HasAudio(), and
// WebKit only for an element whose mediaType() is VideoAudio, so a re-encode
// that drops the silent audio track would leave playback working and the
// screen quietly sleeping on two of the three engines. Sniffing the box /
// element name is enough to catch that without decoding the file.
func TestWakeAssetsCarryAudioTrack(t *testing.T) {
	base, _, _ := startTestServer(t, newTestConfig())
	for _, tc := range []struct{ path, marker string }{
		{"/wake.mp4", "soun"},      // hdlr box handler_type for an audio track
		{"/wake.webm", "OpusHead"}, // Opus codec private data
	} {
		resp, err := http.Get(base + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(body, []byte(tc.marker)) {
			t.Errorf("%s carries no audio track (no %q); Firefox and Safari will not hold the screen lock", tc.path, tc.marker)
		}
	}
}

// TestAPITimeReturnsParseableRFC3339Nano covers the sole contract the viewer
// depends on for its clock-offset estimate: the body decodes and "now"
// parses as RFC3339Nano. A malformed or wrongly-formatted timestamp here
// would silently poison every downstream viewer-latency measurement rather
// than fail loudly, so this is the one thing worth pinning.
func TestAPITimeReturnsParseableRFC3339Nano(t *testing.T) {
	base, _, _ := startTestServer(t, newTestConfig())
	resp, err := http.Get(base + "/api/time")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got struct {
		Now string `json:"now"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, err := time.Parse(time.RFC3339Nano, got.Now); err != nil {
		t.Errorf("now = %q does not parse as RFC3339Nano: %v", got.Now, err)
	}
}

// TestViewerLatencyAcceptsValidReport covers the success path end to end:
// a valid POST returns 204 and the value actually reaches the metrics the
// admin page reads, via the same Snapshot() the rest of the surface uses.
func TestViewerLatencyAcceptsValidReport(t *testing.T) {
	base, _, m := startTestServer(t, newTestConfig())
	resp, err := http.Post(base+"/api/viewer-latency", "application/json", strings.NewReader(`{"paint_ms": 123.5}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	snap := m.Snapshot()
	if snap.Web.ViewerLatencyLast != 123.5 {
		t.Errorf("viewer_latency_last_ms = %v, want 123.5", snap.Web.ViewerLatencyLast)
	}
	if snap.Web.ViewerLatencyCount != 1 {
		t.Errorf("viewer_latency_samples = %d, want 1", snap.Web.ViewerLatencyCount)
	}
	if snap.Web.ViewerReports != 1 {
		t.Errorf("viewer_reports_total = %d, want 1", snap.Web.ViewerReports)
	}
}

// TestViewerLatencyRejectsBadReports is the guard-rail table: every shape of
// bad input the endpoint must reject with 400, each leaving the metrics
// untouched — an unauthenticated LAN endpoint must not let a malformed or
// hostile body poison the p95 every viewer's paint figure feeds into.
func TestViewerLatencyRejectsBadReports(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"malformed JSON", `{"paint_ms": `},
		{"negative", `{"paint_ms": -1}`},
		{"over max", `{"paint_ms": 60001}`},
		// Go's json decoder rejects 1e999 outright (strconv.ParseFloat
		// returns ErrRange, which encoding/json surfaces as a decode error)
		// rather than rounding it to +Inf, so this exercises the same
		// "malformed body" branch as truncated JSON above, not the
		// IsInf/IsNaN guard.
		{"overflow to Inf", `{"paint_ms": 1e999}`},
		// A bare NaN token is not valid JSON syntax at all (JSON has no
		// NaN/Infinity literals), so this fails at tokenizing, again via the
		// "malformed body" branch rather than the IsNaN guard.
		{"NaN literal", `{"paint_ms": NaN}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, _, m := startTestServer(t, newTestConfig())
			resp, err := http.Post(base+"/api/viewer-latency", "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
			snap := m.Snapshot()
			if snap.Web.ViewerLatencyCount != 0 {
				t.Errorf("viewer_latency_samples = %d, want 0", snap.Web.ViewerLatencyCount)
			}
		})
	}
}

// TestViewerLatencyWrongMethodRejected confirms the route is registered
// POST-only. Go 1.22+'s ServeMux normally answers a method mismatch with
// 405, but only when nothing else matches the path; this codebase also
// registers a catch-all "GET /" (pageHandler, serving index.html or 404 for
// an unknown path), which is itself a valid match for a GET request and
// therefore wins before the mux's 405 logic ever triggers — confirmed with
// a standalone net/http/httptest repro alongside this route both with and
// without a catch-all present. So GET /api/viewer-latency actually reaches
// pageHandler and gets its 404, not a 405 from the mux. Either way, the
// point of this test — a GET must not be treated as a valid latency
// report — holds, so it's asserted directly rather than pinned to 405.
func TestViewerLatencyWrongMethodRejected(t *testing.T) {
	base, _, m := startTestServer(t, newTestConfig())
	resp, err := http.Get(base + "/api/viewer-latency")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		t.Errorf("status = %d, want anything but 204 — a GET must never be accepted as a report", resp.StatusCode)
	}
	snap := m.Snapshot()
	if snap.Web.ViewerLatencyCount != 0 {
		t.Errorf("viewer_latency_samples = %d, want 0 after a GET", snap.Web.ViewerLatencyCount)
	}
}

// TestViewerLatencyOversizedBodyRejected proves the MaxBytesReader guard is
// actually wired up: a body past the 4 KiB cap must be rejected rather than
// read into memory, which matters because this endpoint has no auth on a
// LAN and is reachable by anything that can send it a request.
func TestViewerLatencyOversizedBodyRejected(t *testing.T) {
	base, _, m := startTestServer(t, newTestConfig())
	// Pad well past the 4 KiB cap with a value that would otherwise be
	// perfectly valid JSON, so a pass here can only be explained by the
	// size limit, not by the value/shape guards.
	huge := `{"paint_ms": 1, "pad": "` + strings.Repeat("x", 8192) + `"}`
	resp, err := http.Post(base+"/api/viewer-latency", "application/json", bytes.NewReader([]byte(huge)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	snap := m.Snapshot()
	if snap.Web.ViewerLatencyCount != 0 {
		t.Errorf("viewer_latency_samples = %d, want 0", snap.Web.ViewerLatencyCount)
	}
}

// nextSSEData reads one "data: ..." payload from an SSE stream, skipping the
// retry directive and ping comments that aren't a message.
func nextSSEData(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	var data []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if err == io.EOF && len(data) > 0 {
				return strings.Join(data, "\n")
			}
			t.Fatalf("reading SSE stream: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if len(data) > 0 {
				return strings.Join(data, "\n")
			}
			continue
		}
		if after, ok := strings.CutPrefix(line, "data: "); ok {
			data = append(data, after)
		}
	}
}
