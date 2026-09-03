package cli

import (
	"os"
	"path/filepath"
	"slices"
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
	t.Setenv("DEEPGRAM_API_KEY", "secret-from-env")

	f := STTFlags{Engine: "deepgram"}
	resolveSTTDefaults(&f)
	if f.APIKey != "secret-from-env" {
		t.Errorf("APIKey = %q, want it read from DEEPGRAM_API_KEY", f.APIKey)
	}
}

// TestAPIKeyIsPerEngine is a regression guard for a live run that died on a
// 401: with both variables bound to one flag, whichever was set first won, so
// a shell (or .env) carrying DEEPGRAM_API_KEY silently handed a Deepgram key
// to Speechmatics. The key must come from the selected engine's variable only.
func TestAPIKeyIsPerEngine(t *testing.T) {
	t.Setenv("DEEPGRAM_API_KEY", "dg-key")
	t.Setenv("SPEECHMATICS_API_KEY", "sm-key")

	for engine, want := range map[string]string{
		"deepgram":     "dg-key",
		"speechmatics": "sm-key",
	} {
		f := STTFlags{Engine: engine}
		resolveSTTDefaults(&f)
		if f.APIKey != want {
			t.Errorf("%s picked up %q, want %q", engine, f.APIKey, want)
		}
	}

	// The exact failure: only the other engine's key is in the environment.
	t.Setenv("SPEECHMATICS_API_KEY", "")
	f := STTFlags{Engine: "speechmatics"}
	resolveSTTDefaults(&f)
	if f.APIKey == "dg-key" {
		t.Error("speechmatics fell back to DEEPGRAM_API_KEY; that 401s at the recognizer")
	}
	if err := requireAPIKey(f.Engine, f.APIKey); err == nil {
		t.Error("a missing SPEECHMATICS_API_KEY should fail before any audio flows")
	}
}

// TestKeytermFileIsFoldedIn covers the whole point of the flag: a list too long
// to pass as flags arrives intact, keeps its order (the engines cut from the
// end), and picks up whatever --keyterm also supplied.
func TestKeytermFileIsFoldedIn(t *testing.T) {
	p := filepath.Join(t.TempDir(), "keyterms.txt")
	body := "# a comment\n\nHabakkuk\n  Melchizedek  \n# another\nBeth-shemesh\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	_, cli, err := Parse([]string{"replay", writeTempFile(t), "--engine", "mock",
		"--keyterm", "Anthropic", "--keyterm-file", p})
	if err != nil {
		t.Fatal(err)
	}
	f := cli.Replay.STTFlags
	if err := resolveSTTDefaults(&f); err != nil {
		t.Fatal(err)
	}
	want := []string{"Anthropic", "Habakkuk", "Melchizedek", "Beth-shemesh"}
	if !slices.Equal(f.Keyterm, want) {
		t.Errorf("Keyterm = %q, want %q", f.Keyterm, want)
	}

	// A file that exists but says nothing is a mistake worth reporting: it
	// otherwise looks exactly like a run with keyterms working.
	empty := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(empty, []byte("# nothing but comments\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := resolveSTTDefaults(&STTFlags{Engine: "mock", KeytermFile: empty}); err == nil {
		t.Error("an empty keyterm file should be an error, not a silent no-op")
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
	if err := requireAPIKey("speechmatics", ""); err == nil {
		t.Error("speechmatics without a key should fail fast")
	} else if !strings.Contains(err.Error(), "SPEECHMATICS_API_KEY") {
		t.Errorf("error should name the engine's own env var, got: %v", err)
	}
}

// TestSTTDefaultsArePerEngine guards the reason --model and --language carry
// no default tag: nova-3 and en-US are Deepgram's names, and sending either to
// Speechmatics is an immediate invalid_model / invalid_language.
func TestSTTDefaultsArePerEngine(t *testing.T) {
	cases := []struct {
		engine, model, language string
	}{
		{"deepgram", "nova-3", "en-US"},
		{"speechmatics", "enhanced", "en"},
	}
	for _, c := range cases {
		f := STTFlags{Engine: c.engine}
		resolveSTTDefaults(&f)
		if f.Model != c.model || f.Language != c.language {
			t.Errorf("%s defaults = %q/%q, want %q/%q", c.engine, f.Model, f.Language, c.model, c.language)
		}
	}

	// An explicit flag always wins over the engine's default.
	f := STTFlags{Engine: "deepgram", Model: "nova-2", Language: "fr"}
	resolveSTTDefaults(&f)
	if f.Model != "nova-2" || f.Language != "fr" {
		t.Errorf("explicit flags overwritten: got %q/%q", f.Model, f.Language)
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
// stt.RunSession decides whether to build an enabled Gate.
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
	for _, name := range []string{"deepgram", "speechmatics", "mock"} {
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

// TestFlagsReadEnvVars pins the LIVECAPTION_* names the deployed systemd unit
// configures the service with. It takes no arguments beyond the subcommand, so
// a kong upgrade that changed DefaultEnvars' naming would silently leave every
// box running on defaults; this fails instead.
func TestFlagsReadEnvVars(t *testing.T) {
	t.Setenv("LIVECAPTION_DEVICE", "plughw:CARD=Device,DEV=0")
	t.Setenv("LIVECAPTION_ADDR", ":80")
	t.Setenv("LIVECAPTION_MDNS_NAME", "captions")
	t.Setenv("LIVECAPTION_BACKEND", "alsa")
	t.Setenv("LIVECAPTION_AUDIO_STREAM", "false")

	_, c, err := Parse([]string{"live"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Live.Device != "plughw:CARD=Device,DEV=0" {
		t.Errorf("Device = %q, want it from LIVECAPTION_DEVICE", c.Live.Device)
	}
	if c.Live.Backend != "alsa" {
		t.Errorf("Backend = %q, want alsa", c.Live.Backend)
	}
	if c.Live.Addr != ":80" {
		t.Errorf("Addr = %q, want :80", c.Live.Addr)
	}
	if c.Live.MDNSName != "captions" {
		t.Errorf("MDNSName = %q, want captions", c.Live.MDNSName)
	}
	if c.Live.AudioStream {
		t.Error("AudioStream = true, want LIVECAPTION_AUDIO_STREAM=false to disable it")
	}
}

// TestNoColorKeepsItsUnprefixedName guards the one flag with an explicit env
// tag: DefaultEnvars must not rename NO_COLOR, which is a cross-tool
// convention rather than ours to prefix.
func TestNoColorKeepsItsUnprefixedName(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("LIVECAPTION_DEVICE", "default")

	_, c, err := Parse([]string{"live"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !c.NoColor {
		t.Error("NO_COLOR did not disable colour")
	}
}

// TestEmptySettingIsIgnored covers the half-edited EnvironmentFile line
// (LIVECAPTION_LOGO= with nothing after it). Without dropEmptyEnvars kong
// resolves "" against existingfile, hits the working directory, and refuses to
// start with "exists but is a directory".
func TestEmptySettingIsIgnored(t *testing.T) {
	t.Setenv("LIVECAPTION_DEVICE", "default")
	t.Setenv("LIVECAPTION_LOGO", "")
	t.Setenv("LIVECAPTION_KEYTERM_FILE", "")

	_, c, err := Parse([]string{"live"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Live.Logo != "" || c.Live.KeytermFile != "" {
		t.Errorf("Logo = %q, KeytermFile = %q, want both empty", c.Live.Logo, c.Live.KeytermFile)
	}
}

// TestEmptyMDNSNameDisablesIt is the exception to TestEmptySettingIsIgnored:
// an empty name is the documented way to switch the advertisement off, so it
// must not be swallowed as "unset" and fall back to the default.
func TestEmptyMDNSNameDisablesIt(t *testing.T) {
	t.Setenv("LIVECAPTION_DEVICE", "default")
	t.Setenv("LIVECAPTION_MDNS_NAME", "")

	_, c, err := Parse([]string{"live"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Live.MDNSName != "" {
		t.Errorf("MDNSName = %q, want empty to disable the advertisement", c.Live.MDNSName)
	}
}
