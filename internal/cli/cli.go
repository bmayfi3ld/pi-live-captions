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
	Engine   string   `default:"deepgram" enum:"deepgram,mock" group:"Speech-to-text" help:"Recognizer to use. 'mock' runs offline with no API cost."`
	APIKey   string   `env:"DEEPGRAM_API_KEY" group:"Speech-to-text" help:"Deepgram API key."`
	Model    string   `default:"nova-3" group:"Speech-to-text" help:"Deepgram model."`
	Language string   `default:"en-US" group:"Speech-to-text" help:"Recognition language."`
	Keyterm  []string `group:"Speech-to-text" help:"Proper noun to bias recognition toward. Repeatable."`

	AutoPause   bool          `default:"true" negatable:"" group:"Speech-to-text" help:"Stop the recognizer connection while the audio is silent, so a quiet room costs nothing."`
	SilenceHold time.Duration `name:"silence-hold" default:"60s" group:"Speech-to-text" help:"How long the audio must stay silent before the connection is paused."`
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
	Addr     string `default:":8080" group:"Server" help:"Listen address for the viewer and admin pages."`
	Lines    int    `default:"3" group:"Server" help:"Caption rows visible on the viewer page."`
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
		kong.Description("Live captions: stream audio to a speech-to-text service and serve the text to a webpage."),
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
