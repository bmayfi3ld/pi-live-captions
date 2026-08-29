# livecaption

Streams audio — from a soundboard's USB output or from an audio file — to a speech-to-text
service and serves the resulting captions to a webpage in near real time, for live-event
captioning. See [DESIGN.md](DESIGN.md) for the architecture and the reasoning behind it.

## Prerequisites

- Go 1.26+
- `ffmpeg` and `ffprobe` on `PATH` (used for capture, decode, resample, and playback)
- An API key for one of the recognizers, set via its env var (there is no flag; a key on the
  command line lands in `ps` and shell history):
  [Deepgram](https://deepgram.com) (`DEEPGRAM_API_KEY`, the default engine) or
  [Speechmatics](https://speechmatics.com) (`SPEECHMATICS_API_KEY`, `--engine speechmatics`)

`--model` and `--language` default to whatever the selected engine expects (`nova-3` / `en-US` for
Deepgram, `enhanced` / `en` for Speechmatics), so switching engines needs only `--engine`.

`--engine mock` needs neither a key nor network access — it emits canned transcripts driven by
media time, which is what makes the tool testable without hardware or API spend.

## Build

```bash
go build -o livecaption ./cmd/livecaption
./livecaption --version
```

Or run directly with `go run ./cmd/livecaption <command>`.

## Commands

### `devices` — find the soundboard

```bash
livecaption devices
```

Lists capture inputs per backend (`pulse`, `alsa`) with the name to pass to `--device`.
Enumeration is best-effort (it shells out to `ffmpeg -sources`); an empty list on your machine
just means that backend didn't answer, not that you have no audio hardware.

### `replay` — the no-cost dev loop

Streams an audio file through the real pipeline at wall-clock rate, so everything downstream
behaves exactly as it will with the live feed:

```bash
livecaption replay recording.mp3 --engine mock --no-transcript
```

This is the primary development loop: no API key, no network, no recognizer spend, and output is
reproducible run to run since the mock engine is driven by media time, not the wall clock. The
file is released at true wall-clock rate, so a half-hour recording takes half an hour — that is
the point, since it is what makes the dev loop predict the live run.

To judge caption delay by ear, add `--monitor`:

```bash
livecaption replay recording.mp3 --monitor
```

This tees the exact frames sent to the recognizer into a second ffmpeg writing to your speakers —
what you hear is bit-identical to what the recognizer receives. It plays to the default pulse sink
and adds a printed ~80 ms of playback buffer, so perceived delay slightly overstates true caption
latency. Use it to tune `--keyterm` and to get a feel for lag
*before* an event, not during one.

### `live` — the real thing

```bash
livecaption live --device <name-from-devices> --keyterm "Anthropic" --keyterm "Claude"
```

`--device` is validated against `livecaption devices` output before capture starts (best-effort;
if enumeration fails outright for the chosen backend, validation is skipped with a logged warning
rather than blocking the run), and then confirmed with a probe read before the session begins — so
a typo is a clear startup error, not room noise captioned as your event.

### Auto-pause

Both `live` and `replay` close the recognizer connection during silence and reopen it when audio
returns, so a quiet room doesn't rack up recognizer charges (see DESIGN.md §3 for how it decides
what counts as silence and why closing rather than idling the connection). It's on by default:

| flag | default | effect |
|---|---|---|
| `--auto-pause` / `--no-auto-pause` | on | enable/disable the feature |
| `--silence-hold` | `60s` | how long silence has to hold (in media time) before pausing |

The silence threshold itself is fixed at -45 dBFS (`silenceThresholdDB` in `internal/stt/gate.go`);
a materially hotter or colder feed needs that constant moved and a rebuild. Turn the feature off
with `--no-auto-pause` for a venue where dead air should still keep the connection warm, or raise
`--silence-hold` if pauses are firing during ordinary pauses for breath.
`/admin` and the status line report `pauses_total` and `paused_sec` so a mistuned gate is visible
rather than inferred from the recognizer bill.

### Speech timing

A pause of at least 1.5s counts as the speaker actually stopping: it freezes the caption row where
it is and closes a transcript line. That threshold is the `breakGap` constant in
`internal/caption/hub.go` — venue-tuned, but not a flag (see DESIGN.md §4).

Every engine publishes only settled results — Deepgram's `is_final`, Speechmatics' `AddTranscript`
— so a word never changes once it's on screen. Cadence is therefore governed by the recognizer's
own finalisation window: Deepgram's `endpointing`, left at the server default, or Speechmatics'
`max_delay`, pinned to 1.0s so it stays clear of the 1.5s speech-pause threshold below. Watch
`Segments / lines` on `/admin` to check fragmentation: roughly 1–3 segments per line is healthy,
and a ratio climbing well past that means phrases are splitting on every hesitation.

### Speakers

Diarization is on by default. Both engines label the speaker for every word they return
(Deepgram `diarize`, Speechmatics `diarization: speaker`), so a segment that spans a turn is
split into one caption per speaker rather than being credited to whoever started it.

| flag | default | effect |
|---|---|---|
| `--diarize` / `--no-diarize` | on | ask the recognizer who is speaking |

A change of speaker always breaks the caption row — two people's words never share a line — and
closes a transcript line. On screen the change is marked by a small coloured numbered dot in a
left gutter, on the first row of the new turn only; continuation rows are unmarked, so a
single-speaker session shows nothing at all. The gutter itself is always reserved — it sits in
the page margin rather than in front of the text, and never appears or disappears mid-session,
which would slide painted words sideways. The number is the speaker,
the colour is a fast hint that the turn changed; six colours cycle, and the number stays
authoritative past that. `transcript.txt` spells the same thing out as an `[S2]` prefix.

Speaker labels are cluster indices, not identities: they are stable within a connection and
renumbered after a reconnect. Turn it off with `--no-diarize` if a venue sees the extra
recognition delay it can cost.

### Music

Speechmatics can flag music in the feed (its audio-events detector); Deepgram has no
equivalent, so the flag does nothing there. While music is playing captions are suppressed —
sung lyrics come back as garble — and the status indicator reads `♪ music` so a frozen screen
reads as deliberate rather than broken. The open transcript line is closed at the first note,
so the sentence spoken before the song isn't glued to whatever follows it.

| flag | default | effect |
|---|---|---|
| `--music-detect` / `--no-music-detect` | on | suppress captions while the recognizer reports music |

Speechmatics warns the detector can be over-sensitive — congregational singing is exactly what
it is for, but a loud room or an instrument under speech can trip it. Each event is logged at
info level with its time and confidence; if it is swallowing speech in your venue, run with
`--no-music-detect`.

## Transcripts

On by default — every session writes to `./transcripts/<YYYY-MM-DDTHH-MM-SS>/`, both
`transcript.txt` (human-readable, timestamped, with a `[S2]` prefix when the speaker is known, for
tooling). Change the location with `--transcript-dir` or `LIVECAPTION_TRANSCRIPT_DIR`; disable
with `--no-transcript`.

## Viewer and admin

The viewer page is served at the `--addr` you configured (default `http://localhost:8080/`). It's
a bottom-anchored rolling caption window sized in `vw`, so the same URL works on a phone, a
projector, or as an OBS browser source. `--logo <file>` puts an image in the top-right corner
beside the connection dot. Query parameters:

| param | effect |
|---|---|
| `?lines=N` | number of caption rows shown (default 6) |
| `?size=N` | base font size in `vw` |
| `?theme=light` | light theme (default is dark) |
| `?logo=0` | hides the logo (e.g. for OBS, where branding is composited downstream) |
| `?wake=0` | disables the screen wake lock entirely, gate included (OBS browser sources, wall-mounted displays — nobody there to tap it) |

Over plain HTTP — the standard LAN deployment — the browser's Wake Lock API is unavailable outside
a secure context, so the viewer falls back to a silent looping video to keep the screen on. That
needs one tap to unlock playback, so the page shows a one-time "Tap to start" gate before captions
become visible; captions are already streaming in behind it, so tapping reveals current state
rather than an empty page. `?wake=0` skips this entirely.

`/admin` also carries one operator control: **Clear screen**, which blanks every connected viewer
immediately (for when something lands on screen that shouldn't stay there). It POSTs to
`/api/clear`, and the server closes the in-progress transcript line as it goes, so the cleared
text still reaches `transcript.txt`. That control is the reason for `ADMIN_PASSWORD`: set it and
both `/admin` and `/api/clear` require HTTP basic auth with user `admin`; leave it unset and the
page stays open but the clear button renders greyed out, explaining on hover that `ADMIN_PASSWORD`
is unset (the API refuses it with 503 regardless). Basic auth over the LAN is the whole
threat model — it exists so a stranger who stumbles onto the page mid-event can't blank the
screen, not to withstand a determined attacker.

`/admin` is otherwise a metrics dashboard polling `/api/stats` once a second — restarts, xruns,
STT reconnects, buffer drops, auto-pause count and total paused time, SSE client counts, and
latency: caption-segment percentiles over a trailing 5-minute window headline the page — since
every segment reaching the display is already settled text, that figure *is* time-to-first-pixels,
with no separate interim reading to reconcile it against — alongside a second row for
viewer-reported publish→paint latency, which a viewer measures at the moment the word leaves its
paced display queue for the screen and therefore includes the cadence backlog, not just the wire
hop. Plus a waterfall breaking a segment's latency into upload /
recognize / assemble phases (with the unmeasured capture leg drawn as a labelled hatched segment)
and the separately-sampled viewer leg set off by a gap. A `Segments / lines` stat shows
`segments_total` against `lines_total` — the fragmentation readout: roughly
1–3 segments per line is healthy, and a ratio climbing well past that means phrases are splitting
on every hesitation. A status badge at the top reads `ok` /
`degraded` / `paused` / `closed`: an auto-pause
shows as "STT Paused," not "Degraded" — it's expected, money-saving behaviour, not a fault — and a
past blip (a reconnect, a buffer drop) only holds the badge at "Degraded" briefly rather than for
the rest of the session. Check it during an event to confirm nothing is degrading silently.

## stdout vs stderr

Finalized captions go to stdout; everything else (logs, status line) goes to stderr, so they split
cleanly:

```bash
livecaption replay recording.mp3 > captions.txt 2> run.log
```

At the default log level, stdout carries no captions — watching a session only shows the status
line on stderr, so the terminal doesn't fill up with caption text. Pass `-v` / `--log-level=debug`
to get the live stream on stdout (e.g. for the redirect above). Either way, every line is also
written to `transcripts/<session>/transcript.txt`.

## Running an event: a short checklist

- **Feed a mono aux/matrix send of the mics, not the main mix.** `-ac 1` will happily downmix
  music and effects along with speech; this is the single biggest accuracy lever in the project.
- **Set `--keyterm` for every proper noun** in the event (names, places, in-house terms) — costs
  nothing, helps a lot. For a long list, put one term per line in a file and pass
  `--keyterm-file`; `keyterms-esv.txt` is the 1000-term list for reading the ESV. Order it
  most-likely-spoken first: Speechmatics takes 1000 terms, Deepgram only the first 400, and the cut
  comes off the end.
- **Do a `--monitor` dry run beforehand** (on `replay`, with representative audio) to hear and tune
  perceived delay before you're live.
- **Check `/admin` shows a clean run** — no restarts, no reconnects, no buffer drops — before
  trusting the feed.

## Troubleshooting

- **401 on first connect** — check the env var for your engine, `DEEPGRAM_API_KEY` or
  `SPEECHMATICS_API_KEY`. The run stops immediately rather than retrying, and so
  does a rejected `--model` / `--language`, which each engine names its own way.
- **`unknown stt engine`** — only `deepgram`, `speechmatics` and `mock` exist; check `--engine`.
- **First connect is slow with a big `--keyterm-file`** — Speechmatics builds the dictionary before
  it acknowledges the session, up to 15 s the first time. It caches identical lists for 24 h, so
  later connections (including every `--auto-pause` redial) are quick. Don't edit the list between
  runs on the day for no reason: any change is a new dictionary and a new cold start.
- **No devices listed by `devices`** — confirm `ffmpeg` is on `PATH` and a sound server (PulseAudio
  / PipeWire) is running; `alsa` enumeration commonly comes back empty even when ALSA devices work
  fine, so also try known names like `hw:0,0` or `default` directly with `live --backend alsa`.
- **Captions lagging** — text lands when the recognizer finalizes a window, so that window is the
  knob: `endpointing` in `dialURL` for Deepgram, `maxDelay` in `speechmatics.go` for Speechmatics.
  Watch `Segments / lines` on `/admin`: a ratio climbing well past a few segments per line means
  phrases are fragmenting, i.e. the window is too aggressive.
  Check the latency waterfall on `/admin` to see which leg (upload / recognize / assemble) is
  slow, and see DESIGN.md §4.
- **First word after a quiet spell is missing/late, or the connection pauses during ordinary
  pauses for breath** — auto-pause; loosen it with a longer `--silence-hold`, or disable it with
  `--no-auto-pause`. See "Auto-pause" above.
