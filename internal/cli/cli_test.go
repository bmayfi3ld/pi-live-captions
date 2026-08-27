package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"livecaption/internal/stt"
)

func writeTempFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "audio.mp3")
	if err := os.WriteFile(p, []byte("not really audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestMissingFileIsRejected relies on kong's existingfile type, so a typo is
// caught before ffmpeg is spawned.
func TestMissingFileIsRejected(t *testing.T) {
	if _, _, err := Parse([]string{"replay", "/nonexistent/nope.mp3"}); err == nil {
		t.Error("expected a missing file to be rejected")
	}
}

// TestMissingLogoIsRejected relies on kong's existingfile type, same as
// TestMissingFileIsRejected, so a typo'd --logo path fails at parse time
// rather than as a broken image mid-event.
func TestMissingLogoIsRejected(t *testing.T) {
	file := writeTempFile(t)
	if _, _, err := Parse([]string{"replay", file, "--logo", "/does/not/exist"}); err == nil {
		t.Error("expected a missing --logo file to be rejected")
	}
}

func TestEnumsAreEnforced(t *testing.T) {
	file := writeTempFile(t)
	for _, args := range [][]string{
		{"replay", file, "--engine", "nonsense"},
		{"live", "--device", "x", "--backend", "nonsense"},
		{"replay", file, "--log-level", "nonsense"},
	} {
		if _, _, err := Parse(args); err == nil {
			t.Errorf("expected %v to be rejected", args)
		}
	}
}

// TestAPIKeyComesFromEnvironment keeps the key out of shell history.
func TestAPIKeyComesFromEnvironment(t *testing.T) {
	file := writeTempFile(t)
	t.Setenv("DEEPGRAM_API_KEY", "secret-from-env")

	_, c, err := Parse([]string{"replay", file})
	if err != nil {
		t.Fatal(err)
	}
	if c.Replay.APIKey != "secret-from-env" {
		t.Errorf("APIKey = %q, want it bound from DEEPGRAM_API_KEY", c.Replay.APIKey)
	}
}

func TestLiveRequiresDevice(t *testing.T) {
	if _, _, err := Parse([]string{"live"}); err == nil {
		t.Error("expected 'live' without --device to be rejected")
	}
}

// TestTranscriptsDefaultOn guards the intent that recording is the default
// behaviour, not something to remember to switch on.
func TestTranscriptsDefaultOn(t *testing.T) {
	file := writeTempFile(t)
	_, c, err := Parse([]string{"replay", file})
	if err != nil {
		t.Fatal(err)
	}
	if c.Replay.NoTranscript {
		t.Error("transcripts should be enabled unless --no-transcript is given")
	}
	if c.Replay.TranscriptDir == "" {
		t.Error("transcript dir should have a default")
	}
}

func TestRequireAPIKey(t *testing.T) {
	if err := requireAPIKey("mock", ""); err != nil {
		t.Errorf("mock engine should not need a key: %v", err)
	}
	if err := requireAPIKey("deepgram", ""); err == nil {
		t.Error("deepgram without a key should fail fast")
	} else if !strings.Contains(err.Error(), "DEEPGRAM_API_KEY") {
		t.Errorf("error should name the env var, got: %v", err)
	}
	if err := requireAPIKey("deepgram", "k"); err != nil {
		t.Errorf("deepgram with a key should pass: %v", err)
	}
}

// TestAutoPauseDefaults guards the intent that auto-pause is on by default
// with sensible values, not something an operator has to remember to enable.
func TestAutoPauseDefaults(t *testing.T) {
	file := writeTempFile(t)
	_, c, err := Parse([]string{"replay", file})
	if err != nil {
		t.Fatal(err)
	}
	if !c.Replay.AutoPause {
		t.Error("auto-pause should default to enabled")
	}
	if c.Replay.SilenceHold != 60*time.Second {
		t.Errorf("SilenceHold default = %v, want 60s", c.Replay.SilenceHold)
	}
}

// TestAutoPauseNegatable checks --no-auto-pause turns it off, matching how
// deepgram.go decides whether to build an enabled Gate.
func TestAutoPauseNegatable(t *testing.T) {
	file := writeTempFile(t)
	_, c, err := Parse([]string{"replay", file, "--no-auto-pause"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Replay.AutoPause {
		t.Error("--no-auto-pause should disable AutoPause")
	}
}

// TestSilenceHoldValidation covers the > 0 constraint on --silence-hold.
func TestSilenceHoldValidation(t *testing.T) {
	file := writeTempFile(t)
	if _, _, err := Parse([]string{"replay", file, "--silence-hold", "0s"}); err == nil {
		t.Error("--silence-hold 0s should be rejected")
	}
	if _, _, err := Parse([]string{"replay", file, "--silence-hold", "5s"}); err != nil {
		t.Errorf("--silence-hold 5s should be accepted: %v", err)
	}
}

// TestNewEngineRejectsUnknown covers the branch --engine's enum makes
// unreachable from the CLI but a programmatic caller can still hit.
func TestNewEngineRejectsUnknown(t *testing.T) {
	if _, err := newEngine("nope", stt.Config{}); err == nil {
		t.Error("newEngine should reject an unregistered name")
	}
	for _, name := range []string{"deepgram", "mock"} {
		if _, err := newEngine(name, stt.Config{}); err != nil {
			t.Errorf("newEngine(%q) = %v, want an engine", name, err)
		}
	}
}

func TestBrowserURL(t *testing.T) {
	cases := map[string]string{
		":8080":          "http://localhost:8080",
		"0.0.0.0:8080":   "http://localhost:8080",
		"127.0.0.1:9000": "http://127.0.0.1:9000",
	}
	for in, want := range cases {
		if got := browserURL(in); got != want {
			t.Errorf("browserURL(%q) = %q, want %q", in, got, want)
		}
	}
}
