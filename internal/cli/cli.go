// Package cli defines the command surface.
//
// Subcommands rather than a --source flag, because replay and live take
// genuinely disjoint options: --speed and --monitor are meaningless for a
// capture device, and --device is meaningless for a file.
package cli

import (
	"fmt"
	"time"

	"github.com/alecthomas/kong"
)

// Version is set at build time via -ldflags.
var Version = "0.1.0"

// CLI is the root command tree.
type CLI struct {
	Replay  ReplayCmd  `cmd:"" help:"Replay an audio file at wall-clock rate to simulate a live feed."`
	Live    LiveCmd    `cmd:"" help:"Caption live audio from a capture device."`
	Devices DevicesCmd `cmd:"" help:"List available audio capture devices."`

	Globals
}

// Globals apply to every command.
type Globals struct {
	Version  kong.VersionFlag `help:"Print version and exit."`
	LogLevel string           `enum:"debug,info,warn,error" default:"info" group:"Logging" help:"Diagnostic verbosity. debug also prints the live caption stream to stdout."`
	Verbose  bool             `short:"v" group:"Logging" help:"Shorthand for --log-level=debug, which also shows live captions on stdout."`
	NoColor  bool             `env:"NO_COLOR" group:"Logging" help:"Disable coloured output."`
}

// STTFlags configure the speech-to-text backend.
type STTFlags struct {
	Engine string `default:"deepgram" enum:"deepgram,mock,speechmatics" group:"Speech-to-text" help:"Recognizer to use. 'mock' runs offline with no API cost."`
	// No env tag: kong would take the first variable that happens to be set,
	// which silently hands a Deepgram key to Speechmatics for anyone with both
	// in their environment (or a .env). resolveSTTDefaults reads the one
	// belonging to the engine actually selected.
	APIKey string `group:"Speech-to-text" help:"API key for the selected engine ($DEEPGRAM_API_KEY / $SPEECHMATICS_API_KEY)."`
	// Model and Language deliberately have no default tag: the right value
	// depends on the engine (nova-3/en-US for Deepgram, enhanced/en for
	// Speechmatics). resolveSTTDefaults fills the blank once --engine is
	// known, before anything reads these.
	Model    string   `group:"Speech-to-text" help:"Recognition model. Defaults to the selected engine's."`
	Language string   `group:"Speech-to-text" help:"Recognition language. Defaults to the selected engine's."`
	Keyterm  []string `group:"Speech-to-text" help:"Proper noun to bias recognition toward. Repeatable."`
	// A file rather than more --keyterm flags because the useful lists are
	// hundreds of terms long (Speechmatics takes 1000), which no command line
	// wants to carry.
	KeytermFile string `name:"keyterm-file" type:"existingfile" group:"Speech-to-text" help:"File of keyterms, one per line, blank lines and # comments ignored. Most-likely-spoken first: a list longer than the engine accepts is cut from the end."`

	AutoPause   bool          `default:"true" negatable:"" group:"Speech-to-text" help:"Stop the recognizer connection while the audio is silent, so a quiet room costs nothing."`
	SilenceHold time.Duration `name:"silence-hold" default:"60s" group:"Speech-to-text" help:"How long the audio must stay silent before the connection is paused."`
	Diarize     bool          `default:"true" negatable:"" group:"Speech-to-text" help:"Attribute segments to speakers, where the selected engine supports it."`
	MusicDetect bool          `name:"music-detect" default:"true" negatable:"" group:"Speech-to-text" help:"Suppress captions while the recognizer reports music (Speechmatics only). Use --no-music-detect if it triggers on speech."`
}

// Validate rejects a silence hold that can't work.
func (f *STTFlags) Validate() error {
	if f.SilenceHold <= 0 {
		return fmt.Errorf("--silence-hold must be positive (got %s)", f.SilenceHold)
	}
	return nil
}

// ServerFlags configure the caption web server.
type ServerFlags struct {
	// The admin password has no flag of its own on purpose (see Parse's
	// description); it is named here so it appears in the help of the
	// subcommands people actually run, not just the bare root help.
	Addr     string `default:":8080" group:"Server" help:"Listen address for the viewer and admin pages. Set $ADMIN_PASSWORD to enable the admin clear-screen control and require basic auth (user: admin) for /admin."`
	Logo     string `type:"existingfile" group:"Server" help:"Image shown in the viewer's top-right corner."`
	MDNSName string `name:"mdns-name" default:"livecaptions" group:"Server" help:"Advertise <name>.local via mDNS (avahi-publish) for as long as the server runs. Empty disables."`
}

// OutputFlags configure transcript recording, which is on by default.
type OutputFlags struct {
	TranscriptDir string `default:"./transcripts" type:"path" env:"LIVECAPTION_TRANSCRIPT_DIR" group:"Output" help:"Directory holding per-session transcript folders."`
	NoTranscript  bool   `group:"Output" help:"Disable transcript recording for this session."`
}

// ReplayCmd streams an audio file through the pipeline at wall-clock rate.
type ReplayCmd struct {
	File    string `arg:"" type:"existingfile" help:"Audio file to replay."`
	Monitor bool   `group:"Monitor" help:"Play the streamed audio over speakers to judge caption delay by ear."`

	STTFlags    `embed:""`
	ServerFlags `embed:""`
	OutputFlags `embed:""`
}

// LiveCmd captures from an audio device.
type LiveCmd struct {
	Device  string `required:"" group:"Audio" help:"Capture device. Run 'livecaption devices' to list."`
	Backend string `default:"pulse" enum:"pulse,alsa" group:"Audio" help:"Capture backend."`

	STTFlags    `embed:""`
	ServerFlags `embed:""`
	OutputFlags `embed:""`
}

// DevicesCmd lists capture inputs.
type DevicesCmd struct{}

// Parse builds the kong context.
func Parse(args []string) (*kong.Context, *CLI, error) {
	var cli CLI
	parser, err := kong.New(&cli,
		kong.Name("livecaption"),
		// The env-only settings are listed here because kong can only document
		// what has a flag, and these deliberately have none: an API key or an
		// admin password passed as a flag lands in every ps listing and shell
		// history on the machine.
		kong.Description("Live captions: stream audio to a speech-to-text service and serve the text to a webpage.\n\n"+
			"Environment (no flag equivalent):\n"+
			"  DEEPGRAM_API_KEY / SPEECHMATICS_API_KEY  key for the selected --engine\n"+
			"  ADMIN_PASSWORD                           enables the /admin clear-screen control and\n"+
			"                                           guards /admin with basic auth (user: admin);\n"+
			"                                           unset leaves the control disabled"),
		kong.UsageOnError(),
		kong.ConfigureHelp(kong.HelpOptions{Compact: true, FlagsLast: true}),
		kong.Vars{"version": Version},
	)
	if err != nil {
		return nil, nil, err
	}
	ctx, err := parser.Parse(args)
	return ctx, &cli, err
}
