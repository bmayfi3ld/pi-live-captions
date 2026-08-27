# livecaption — design

Live captions for a room: audio leaves a soundboard over USB, a computer streams it to a
speech-to-text service, and the resulting text appears on a webpage within a second.

The constraint that shaped everything: the signal chain isn't available during development. So the
tool has a **replay mode** that streams an audio file through the *same* pipeline at true
wall-clock rate. If replay works end to end, connecting the USB converter is a change of
subcommand, not a change of code path.

---

## 1. Pipeline

One linear flow. Each stage is a package, connected by channels, independently testable.

```
  ┌──────────────┐  chan audio.Frame  ┌────────────┐  chan stt.Transcript  ┌─────────┐
  │ audio.Source │ ─────────────────► │ stt.Engine │ ────────────────────► │   Hub   │
  └──────────────┘  16 kHz mono s16le └────────────┘   settled segments    └────┬────┘
    file │ device                      deepgram │ mock                          │
         │                                                        ┌─────────────┼─────────────┐
         └──(tap)──► monitor                                      ▼             ▼             ▼
                     (speakers)                              SSE clients   transcript     metrics
                                                             viewer page     files       /admin
```

Two invariants hold the whole design together:

**Nothing downstream may block the pipeline.** A slow browser, a stalled sound card, or a failing
disk write must never apply backpressure to audio capture. Every fan-out point drops and counts
instead of blocking.

**Every stage has an offline equivalent.** `replay` substitutes for the soundboard, `--engine mock`
substitutes for Deepgram. The full system runs with no hardware, no network, and no API spend —
which is what makes it practical to develop and test.

---

## 2. Audio sources

Both sources shell out to **ffmpeg** and read raw **16 kHz mono signed 16-bit little-endian** PCM
from its stdout.

Why ffmpeg rather than cgo bindings (PortAudio, malgo): the binary stays pure Go with no C
toolchain; ffmpeg absorbs whatever sample rate and channel count the soundboard presents; and the
same code decodes MP3s for replay. One dependency covers capture, decode, resample, downmix and
playback.

| | `replay` (`internal/audio/file.go`) | `live` (`internal/audio/device.go`) |
|---|---|---|
| command | `ffmpeg -i <file> -ac 1 -ar 16000 -f s16le -` | `ffmpeg -f pulse -i <dev> -ac 1 -ar 16000 -f s16le -` |
| paced by | our scheduler | the sound card |
| failure mode | EOF | device disappears → restart with backoff |

### Pacing

ffmpeg decodes as fast as the pipe drains, so in replay mode *the reader* sets the rate. Each chunk
is held until an **absolute deadline** computed from a fixed start time:

```go
due := start.Add(time.Duration(n+1) * interval)
```

Not a `time.Ticker`. With a ticker, every late wakeup pushes all subsequent frames later and the
error accumulates; over a 32-minute file that drift is large enough to invalidate the simulation.
With absolute deadlines a slow iteration is corrected by the next one. `TestFileSourcePacing`
asserts media time and wall time stay within tolerance.

### Chunk size

`chunkSize` in `internal/audio/source.go`: **100 ms** (3200 bytes). Inside Deepgram's recommended
20–250 ms payload range — small enough not to add meaningful latency, large enough to avoid one
WebSocket frame every 20 ms. Fixed rather than configurable; no run ever wanted a different value.

### Live hardening

The capture path can degrade in ways that don't stop the audio, so each has a counter behind it:

- **ffmpeg exits** (USB unplugged) → relaunch with exponential backoff, `ffmpeg_restarts_total`.
  Media offset accumulates across restarts so timestamps stay monotonic.
- **ALSA xruns** → stderr is scanned for overrun/underrun tokens, `xruns_total`.
- **Bad device name** → caught at startup by a probe read, so a typo is an immediate clear error
  rather than an infinite restart loop.

### Monitor playback (`replay --monitor`)

To judge caption delay you need to hear the audio while watching the text land. The tap point is
the design decision: playing the original file with a separate player would drift against our own
release scheduler. Instead the monitor **tees the exact frames already emitted** into a second
ffmpeg writing to the speakers.

So what you hear is bit-identical to what the recognizer receives, released by the same clock. It's
the 16 kHz mono downmix, which is the point — bad source audio becomes *audible* instead of being
inferred from bad transcripts.

One honesty constraint: **playback adds a fixed ~80 ms buffer**, so perceived delay *overstates*
true caption latency by that much. The figure is printed in the banner rather than hidden, and
`/admin` reports measured latency for comparison. Playback goes to the default pulse sink; a
different sink means editing `Monitor.Start`.

The tap is a non-blocking send: a stalled sound card drops monitor frames
(`monitor_frames_dropped_total`) and never touches the caption path. A dead playback process is a
warning, not a session failure.

Replay-only. On `live`, monitoring the board feed through the same machine's speakers invites
acoustic feedback into the mics.

---

## 3. Speech-to-text abstraction

```go
type Engine interface {
	Name() string
	Run(ctx context.Context, frames <-chan audio.Frame, out chan<- Transcript) error
}
```

Adding a provider means writing one adapter and adding a case to `newEngine` in `internal/cli`.
There is no registry: a `switch` over three names does not need one, and the compiler catches a
missing case where a self-registering engine would only fail at run time.

What an adapter actually writes is the *protocol*, not the plumbing. Reconnect backoff, the silence
gate, the bounded audio ring and the latency anchoring are provider-neutral and live once, in
`stt.RunSession`:

```go
// Opens one connection and completes whatever handshake must precede audio.
type Dialer func(ctx context.Context) (*websocket.Conn, Session, error)

// One connection's protocol state; a fresh one per Dialer call.
type Session interface {
	SendAudio(ctx context.Context, pcm []byte) error
	Idle(ctx context.Context) error                        // keepalive, if the protocol has one
	Decode(data []byte) (t Transcript, ok bool, err error) // ok=false: nothing to publish
	Finish(ctx context.Context) error                      // polite end-of-stream
}
```

A provider's `Run` is then one line delegating to `stt.RunSession`. The split was made when
Speechmatics arrived: two providers is what turns "shared plumbing" from speculation into the
smaller amount of code. Two consequences worth naming:

- **`Decode`'s error is fatal** — it drops the connection. Acks, metadata, and undecodable frames
  are the provider's own noise to log and swallow with `ok=false, nil`. Only genuine protocol
  failures come back as errors.
- **The "settled text only" guarantee is enforced per-protocol**, inside each `Decode`. The driver
  publishes whatever it is handed, so each adapter drops its own revisable results (Deepgram's
  non-`is_final`, Speechmatics' `AddPartialTranscript`) before they get that far.

A `PermanentError` from the `Dialer` — a rejected key, an unknown model, a language the provider
does not have — stops the run on the first attempt instead of backing off forever against a typo.
Only providers can tell those apart from a network blip, so they do the classifying.

`Transcript` is pure observation, with no control flags for the hub to interpret: the engine only
ever emits text it will not revise, so there is nothing left to flag as final. It carries `Text`,
media-time `Start`/`Duration` for ordering, display, and gap arithmetic (§4), `Confidence`, and
`CapturedAt`/`ReceivedAt`/`SentAt` — the wall-clock instants latency is actually measured from (see
section 6) — so replay and live measure latency identically. Structure
(when a display row breaks, when a transcript line closes) is derived downstream in the hub, not
reported by the engine.

`Run` owns its own reconnect logic: it returns only when the context is cancelled or frames run
out, never on a dropped connection.

### Deepgram (`internal/stt/deepgram`)

`wss://api.deepgram.com/v1/listen`, header `Authorization: Token <key>`:

```
encoding=linear16&sample_rate=16000&channels=1&model=nova-3&language=en-US
&interim_results=false&punctuate=true&smart_format=true
```

**WebSocket library: `github.com/coder/websocket`.** Worth recording why, since gorilla/websocket
is the better-known name: **gorilla is archived and no longer actively developed.** coder/websocket
is the former `nhooyr.io/websocket`, adopted by Coder in 2024 and maintained. It also fits better —
`context.Context` on every read and write maps onto our cancellation-driven shutdown, and it
serializes concurrent writes internally (we have a PCM writer and a KeepAlive ticker sharing one
connection, which gorilla would require a hand-rolled mutex for).

Used directly rather than via `deepgram-go-sdk`: it's a single endpoint, the client is small, and
the official SDK has lagged on new streaming models. Reversible if that changes.

Structure (all of it in `stt.RunSession`, driven by this adapter): a writer goroutine (PCM → binary
frames, `{"type":"KeepAlive"}` every 5 s when idle) and a reader goroutine (JSON → `Transcript`). On
shutdown it sends `{"type":"CloseStream"}` and drains remaining results so the tail of a session
isn't lost. On disconnect it reconnects with exponential backoff (250 ms → 8 s, jittered), holding
~2 s of audio in a bounded drop-oldest ring so a brief blip loses nothing.

### Speechmatics (`internal/stt/speechmatics`)

`wss://global.rt.speechmatics.com/v2`, header `Authorization: Bearer <key>`. Hand-rolled against
`coder/websocket` for the same reasons as Deepgram, and again in preference to a vendor SDK.

Unlike Deepgram it has a handshake — `StartRecognition`, acknowledged with `RecognitionStarted` —
which the adapter completes inside its `Dialer`, so the driver only ever sees a connection that is
ready for audio. It also numbers its `AddAudio` messages, and `EndOfStream` has to report the final
count, so a session here holds real per-connection state where Deepgram's holds none.

```json
{"message":"StartRecognition",
 "audio_format":{"type":"raw","encoding":"pcm_s16le","sample_rate":16000},
 "transcription_config":{"language":"en","model":"enhanced",
                         "max_delay":1.0,"enable_partials":false,
                         "additional_vocab":[{"content":"<keyterm>"}]}}
```

**`max_delay` is load-bearing, and its default is wrong for us.** It is how long Speechmatics may
wait before committing a final, and it defaults to 4 s (valid range 0.7–4). `breakGap` is 1.5 s, so
the default puts the finalisation window *above* the gap that means "the speaker paused" — every
committed chunk would also read as a pause, which is exactly the ragged-rows bug described in §4.
Pinned to 1.0 s. It is the knob equivalent to Deepgram's `endpointing`: lower for sooner text in
smaller pieces, higher if phrases fragment across rows, but never within reach of `breakGap`.

Two gaps against the Deepgram engine, both deliberate:

- **No profanity filter.** Deepgram takes `profanity_filter=true`; Speechmatics has no equivalent
  one-line switch, so this engine currently does not filter. Not for lack of a mechanism — there
  are two, and either would work server-side if it becomes necessary:
  `transcript_filtering_config.replacements` takes `from`/`to` pairs where `from` may be an
  ECMAScript regex in `/…/` delimiters, so `/^[sS][hH][iI][tT]$/` handles both the case-sensitivity
  (replacement is case-sensitive) and the whole-word anchoring that stops "class" being masked for
  containing "ass"; separately, `results` carries a `tags: ["profanity"]` marker on individual
  words for en/es/it. What is *not* viable is substituting into the server's pre-assembled
  `transcript` string — there are no character offsets to join on, only text, and a plain
  `ReplaceAll` mangles innocent words. Either mechanism needs a word list, which for a church is a
  judgement call rather than boilerplate: "hell" and "damn" are sermon vocabulary, and "ass",
  "cock" and "prick" all have scriptural senses.
- **Confidence is averaged**, not reported. Speechmatics gives one confidence per word where
  Deepgram gives one per segment, so the adapter means the word-level figures (skipping punctuation
  and entities, which say nothing about how well speech was heard).

### Auto-pause

Deepgram bills by streamed duration, and a quiet room — before doors open, over a long
intermission, after the event ends but before someone remembers to Ctrl-C — streams silence at
exactly the same rate as speech. Auto-pause (`internal/stt/gate.go`) closes the recognizer
connection entirely during a confirmed stretch of silence and reopens it when audio returns, so
dead air costs nothing.

Silence is detected per frame as RMS level in dBFS, compared against `silenceThresholdDB`
(**-45**, a constant in `gate.go` — calibrated against the soundboard feed, so a materially
different input level needs it moved). A single quiet frame doesn't pause anything — the `Gate`
requires the level to stay at or below the threshold for `--silence-hold` (default **60 s**) of
*continuous media time* before flipping inactive, and resume is instant on the next frame that
crosses back above threshold. Media time, not wall clock, is what `Observe` keys off (frame
offsets, same as everywhere else in the pipeline) so a hold period behaves identically whether
audio arrives live or from a replayed file, and is deterministic in tests.

The pause **closes the WebSocket**, it doesn't just stop writing to it. Deepgram does document
`KeepAlive` as a way to hold a connection open without being charged for the idle time, so simply
withholding audio would probably also avoid the bill — but "probably" is doing a lot of work in
that sentence, and it stakes the cost of a long intermission on a provider's billing behaviour for
a connection we're deliberately not using. A closed socket cannot be billed by anyone, needs no
footnote, and doesn't change meaning if Deepgram's pricing does. Closing means a real reconnect
(fresh handshake, fresh `Authorization`) when speech resumes.
That reconnect is where the engine's existing ~2 s drop-oldest ring buffer (see Deepgram, above)
earns a second job: it keeps filling from live frames while the connection is down, so by the time
the redial completes there's already a couple of seconds of pre-roll queued to send, and the first
word after silence isn't clipped waiting on handshake latency.

`--auto-pause` / `--no-auto-pause` (default **on**) is the escape hatch for a venue where dead air
is meaningful (e.g. captioning is expected to keep running through a scripted silence) or for
debugging with a stable connection. `pauses_total` and `paused_sec` on `stt` in `/api/stats` (and
therefore `/admin` and the status line) count how often and how long, so the savings — or a gate
that's mistuned for a noisy room — are visible rather than inferred from a Deepgram invoice.

### Mock (`internal/stt/mock`)

Emits canned phrases as settled, punctuated segments — clause-sized chunks separated by realistic
gaps, only the last segment of a phrase carrying terminal punctuation — driven entirely by media
time from the frames, never wall clock, so output is reproducible in tests. The inter-phrase gap
(2s) is deliberately above `breakGap` (1.5s), so the mock exercises the pause break the same way a
real room does. This is what the web layer is developed against.

`mock` never goes quiet, so it cannot exercise auto-pause end to end. That path is covered by the
gate's own unit tests (`internal/stt/gate_test.go`) and by the engine-level pause tests in
`internal/stt/deepgram/deepgram_test.go`, which drive real silent PCM through a fake WebSocket
server — closer to the real thing than a synthetic level schedule was.

---

## 4. Caption hub

The pipeline used to paint Deepgram's `is_final: false` interim hypotheses — text the recognizer
would go on to revise. That cost the audience words swapping under them mid-sentence, and cost the
code a diffing typesetter that could retroactively re-tag an already-painted word as contradicted.
Deepgram's `is_final: true` is the fastest text it will ever commit to, so that is now the only
thing painted (§3's `interim_results=false`). The hub (`internal/caption/hub.go`) turns a stream of
these settled segments into a display and a transcript.

### The visual model

This is the part that drives every other decision, so it comes first.

**Text rolls.** Segments arrive as Deepgram finalizes each window and append to the current row. When the next word doesn't fit, the row freezes, the stack glides up one, and the word
starts the new row. Rows are always full edge to edge.

```
| ...thank you all for coming out  |
| today. We're going to start with |
| a few announcements before the   |   <- filling
```

**A real pause breaks the row.** The speaker stops for at least `breakGap` (1.5s).
The current row freezes where it is, even half-full, and the stack glides. New speech starts clean:

```
| a few announcements before the   |
| main session.                    |   <- frozen short, by the pause
| If you can't hear me at the back |   <- new thought, new row
```

Nothing else breaks a row. Not punctuation, not endpointing, not any other signal.

**Words never change once painted.** That is the whole point of dropping interims, and with an
append-only client (`internal/web/static/caption.js`) it is true by construction rather than by
careful diffing.

### One signal per job

The interim/`is_final`/`speech_final` model conflated three separate questions into one threshold.
Splitting them is the core of the redesign:

| job | signal | why |
|---|---|---|
| flush text to screen | every `is_final` segment | fastest never-revised text there is |
| break a display row | row width, **or** a `breakGap` gap (1.5s) | a caption stack should read as continuous, and reset only when the speaker actually stops |
| close a transcript line | terminal punctuation, or that same gap, or a 1000-char guard | a file read later wants sentences; a screen watched live wants continuity |

The gap is free: `stt.Transcript` already carries `Start` and `Duration`, so the hub computes
`t.Start - prevEnd` as arithmetic — no timers, no goroutines. A **negative** gap means the media
clock restarted (a reconnect or an auto-pause resume), which is a real discontinuity and breaks the
row too, so that case falls out of the same comparison (`isBreakLocked`) rather than needing its
own branch.

The break is evaluated, and the line it closes flushed, *before* the new segment is appended — that
ordering is the subtlest part of `Publish`: it's what makes the pause separate the two utterances
rather than land inside one. A segment arriving right after a pause can itself be a complete
sentence ("Good morning."), so `Publish` runs the punctuation/length check a second time,
unconditionally, after appending — a single call can close two transcript lines: the pause closes
the utterance already in progress, and the segment that follows closes itself
(`TestPauseAndPunctuationBothClose` in `hub_test.go`). Skipping that second check whenever the pause
already closed something would leave the new sentence open until some later segment happened to
arrive.

`Flush()` at shutdown calls the same close path unconditionally, so the tail of a session isn't
lost when the speaker was still talking.

**Fan-out never blocks.** Subscribers get a 16-deep buffered channel; one that can't keep up is
dropped and counted (`slow_disconnects_total`) rather than backpressured. `EventSource` reconnects
on its own and receives a fresh snapshot, so the cost of a drop is one round trip.

History is capped at 20 lines for late-joiner snapshots — well more than the 6 rows a viewer shows,
and the snapshot event is now its only reader (`Hub.Snapshot()` had no other callers and is gone).

### Recognizer cadence vs. `breakGap`

Two thresholds govern speech timing, and they belong to different systems. Deepgram's own
`endpointing` decides how fast it commits to a window — and therefore how fast text reaches the
screen at all, since finals are the only thing published. `breakGap` (`internal/caption/hub.go`,
1.5s) decides when a gap between two committed windows is long enough to mean the speaker actually
stopped, freezing a display row and closing a transcript line.

The invariant between them: **`breakGap` must stay comfortably above the endpointing window.** If
it doesn't, every chunk Deepgram commits to also reads as a pause, and the display goes back to
being ragged — the exact bug an early draft of this design shipped. Neither is a flag any more, so
the check that used to enforce it at parse time is gone; the constraint now lives here and in the
two constants. Raising server-side `endpointing` toward `breakGap` is the change to be careful
about.

### Signals we deliberately don't use

Four trades a future reader would otherwise re-litigate, each recorded with its condition for
reversal:

1. **`utterance_end_ms` / `vad_events`** — dropped. `UtteranceEnd` is derived from word-timing gaps
   in the interim stream, so it cannot be kept without turning `interim_results` back on at the
   wire and filtering revisable text back out in `decodeTranscript`. It's a backstop for *noisy*
   room-mic audio, where endpointing's energy-based VAD never sees true silence; this deployment
   runs off a soundboard or lapel feed, where the mic hears the speaker and not the room, so
   inter-phrase gaps are real digital silence and the hub's own gap arithmetic (above) already
   covers it. Reconsider if a future deployment target is a noisy room mic rather than a clean
   feed.
2. **`punctuate` / `smart_format`** — kept, but no longer cosmetic: they are what lets `closeLocked`
   detect terminal punctuation, so turning them off would silently degrade `transcript.txt` to the
   speech-gap fallback for every sentence.
3. **`speech_final`** — deliberately unused as an utterance boundary. At a short endpointing window it
   fires on every small hesitation; treating it as a boundary would shred `transcript.txt` into
   fragments, which is exactly what an earlier draft of this design did before the mistake was
   caught. It only becomes meaningful again if server-side endpointing is raised toward something closer to
   a full utterance pause, at which point revisiting it might be worthwhile.
4. **Interim results themselves (`is_final: false`)** — the whole point of this design: no revised
   text ever reaches the hub, so there is nothing to diff. Belt and braces: the engine sets
   `interim_results=false` explicitly on the wire *and* `readLoop` drops any non-final it receives
   anyway. The second guard is a trust boundary, not redundancy — a param silently ignored or a
   changed server default would otherwise put revisable text on screen, which the append-only
   typesetter has no way to take back.

   A prefix tracker briefly lived here (2026-08, removed): it consumed the interim stream and
   published the token prefix two consecutive interims agreed on, minus a holdback, to get text
   landing at speech cadence rather than in finalization-gated bursts. It worked, but it meant the
   engine's core promise — "everything published is settled" — rested on a heuristic about how
   Deepgram revises rather than on Deepgram's own `is_final`. Removed in favour of the simpler
   contract. Reconsider only if finalization-gated cadence proves too slow in a real venue, and
   prefer raising or lowering server-side `endpointing` first.

---

## 5. Web server

| Route | Purpose |
|---|---|
| `GET /` | Viewer page |
| `GET /events` | SSE stream: `snapshot` on connect, then incremental events |
| `GET /admin` | Metrics dashboard (no auth) |
| `GET /api/stats` | JSON metrics snapshot |
| `GET /api/time` | Server wall clock, for the viewer's own clock-offset estimation |
| `POST /api/viewer-latency` | Viewer-reported publish→paint latency (unauthenticated, rate-limited — see §6) |
| `GET /logo` | Logo image, registered only when `--logo` is set |
| `GET /healthz` | Liveness |

SSE rather than WebSocket: traffic is strictly one-way, and `EventSource` gives automatic
reconnection with no client-side logic to maintain. Headers set `no-cache` and `X-Accel-Buffering:
no`, every event is flushed immediately, and a `: ping` comment every 15 s keeps idle proxies from
closing the connection.

Event wire format:

```json
{"seq":42,"kind":"caption","text":"a few announcements before the","break":false,"at":"2026-08-19T09:31:05.912Z"}
{"seq":43,"kind":"caption","text":"main session.","break":false,"at":"2026-08-19T09:31:06.301Z"}
{"seq":44,"kind":"caption","text":"If you can't hear me at the back","break":true,"at":"2026-08-19T09:31:08.912Z"}
{"seq":45,"kind":"status","state":"reconnecting","detail":"stt websocket closed","at":"2026-08-19T09:31:10.442Z"}
{"seq":46,"kind":"status","state":"paused","detail":"","at":"2026-08-19T09:31:40.000Z"}
{"seq":47,"kind":"snapshot","text":"...history joined, plus the open committed text","state":"connected","at":"2026-08-19T09:31:41.000Z"}
```

`kind: "final"` and `kind: "interim"` are gone; there is one text-carrying event kind now,
`caption`. Its `text` is always the new segment only — never the accumulated utterance — so the
client can append it blindly, and `break` asks the viewer to freeze the current row before
appending: the speaker actually stopped (§4). `Event` dropped from ten fields to seven along with
`id`, `offset_ms`, `lines` and `pending`: a `caption` event carries nothing about utterance
structure any more, because there's no revision left to protect the client from.

`at` (the publish instant) is stamped on every event kind by `newEventLocked`, unconditionally —
there is no longer a "which kinds get a timestamp" question, since every event kind is
latency-relevant to some viewer measurement.

`detail` is deliberately left empty for `paused`: the server reports *that* the state changed, but
the wording shown for it ("no audio" on the viewer, "paused (no audio)" on `/admin`) is a
presentation decision that belongs to each page, not something baked into the wire event.

Pages are `//go:embed`-ed so the binary ships standalone; `go run` picks up edits while
iterating.

**Viewer.** The page typesets itself instead of trusting the browser to wrap: one
`CanvasRenderingContext2D` measures each candidate word against the row width before placing it —
a browser reflow would move words the reader has already passed sideways and downward, and
measuring client-side avoids that class of motion entirely.

The typesetter (`internal/web/static/caption.js`) is shared between the viewer and `/admin`, which
must render identically — one file, two mounts, `window.CaptionStack(opts)`, rather than the
`/admin` copy silently drifting from a bugfix the viewer got. It used to also be where Deepgram's
revisable hypotheses were diffed against what was already on screen — a word could change color,
get yanked back out of a frozen row, or get retroactively re-tagged as contradicted if a later
revision disagreed with it (`commonPrefixLen`, `removeFromActiveRow`, a three-state `live` /
`settled` / `stale` word model). All of that is gone: the server now only ever publishes committed,
never-revised text (§4), so the typesetter has exactly one job — lay words out left-to-right into
fixed-height rows, and glide the stack up by one row when the current row either runs out of width
or the server reports the speaker actually paused (`Event.Break`).

A row closes on exactly two triggers — the next word doesn't fit (`pushWord`), or the server sent
`break: true` (`breakRow`) — and both run the same `freezeAndGlide` path: the active row is marked
frozen, a new row is appended below it, and the whole stack glides up by exactly one row height. The
governing invariant: a word, once painted, is never touched again — it doesn't change color, doesn't
change font, doesn't move sideways or down. It only ever moves up, by construction rather than by
careful diffing, since `appendSegment` is the *only* way text enters and nothing calls it with
anything but a fresh segment. Row opacity by age is the only recency cue left, since there's no more
revision state for word color to carry.

`computeMetrics()` calibrates canvas measurement against a hidden probe row's real rendered width,
because canvas resolves a font stack independently of layout and an under-measure would clip a
word rather than wrap it. `retypeset()`, debounced off `ResizeObserver` and `visualViewport`
resize, is the only path that ever reflows already-painted text — everything else only appends —
and it no longer needs to replay utterance-boundary metadata to reconstruct rows after a resize,
because with words immutable and pause breaks not recorded per-word, row structure is purely a
function of width. `?lines=` means visible rows, not utterances.

The phone is the primary viewer, so it sets the defaults rather than being an afterthought: type
is larger in portrait (`4vw` is unreadable at phone widths), the visible row count is capped by
what actually fits so a short landscape screen loses a row instead of clipping one, and the page
holds a screen wake lock for as long as it's open and visible — a handset sleeping mid-event is
the likeliest way this interface fails. The Wake Lock API is secure-context only, and the standard
deployment is plain HTTP to a LAN address (adding TLS would mean a browser cert warning on every
phone at every event), so `navigator.wakeLock` is simply undefined there. The viewer falls back to
a silent looping `<video>` — the NoSleep.js technique — as the only mechanism that keeps a screen
on over insecure HTTP. That backend needs one user gesture to unlock video playback, met by a
one-time full-screen tap gate on first load. Playing the video is necessary but not sufficient:
each engine decides separately whether playback earns a screen-on grant, and a tiny muted
video-only file — the shape NoSleep.js ships — satisfies neither. Blink
(`VideoWakeLock::ShouldBeActive`) requires the element to be ≥75 % onscreen *and* to cover >20 %
of the viewport unless it is audible, so `#wakevid` is full-bleed at 1 % opacity behind the
captions. Gecko (`HTMLVideoElement::ShouldCreateVideoWakeLock`) requires
`HasVideo() && (mSrcStream || HasAudio())` — it deliberately ignores audio-less video, which is
"often used as a background image". WebKit (`HTMLMediaElement::shouldDisableSleep`) is stricter
still: it returns `SleepType::None` for any element with `loop()` set, and otherwise only disables
display sleep when `mediaType()` is `VideoAudio`, which `computeCanProduceAudio()` denies to a
muted element or a zero volume. So `wake.mp4`/`wake.webm` each carry a **silent audio track**, the
element loops by seeking rather than by the `loop` attribute, and it plays **unmuted on WebKit
only** — inaudible either way, but an unmuted element claims the audio session, a cost worth
paying only where it buys the lock. Three engines, three different tests; a change that satisfies
one can silently forfeit another, so treat all three as the asset's contract.

One trap worth naming: `/wake.mp4` and `/wake.webm` are unversioned URLs, so they are served
`no-cache` (ETag-revalidated) rather than `immutable`. A phone that cached the pre-audio-track
asset would otherwise keep it for a year and make any later fix look like a no-op (skippable with `?wake=0` for OBS sources and
unattended displays, which have nobody present to tap it). The same URL still serves a projector
and an OBS browser source.

**Admin** — polls `/api/stats` once a second. Simpler than SSE for a single operator client.

---

## 6. Metrics

`internal/metrics` holds one snapshot that the admin page, the status line and the shutdown summary
all read from — so they cannot disagree with each other.

The governing rule: **anything that can degrade silently gets a counter.** A session that quietly
dropped 3 % of its audio must not look identical to a clean one.

| group | fields |
|---|---|
| source | frames/bytes/seconds, `frames_dropped_total`, `ffmpeg_restarts_total`, `xruns_total`, `ffmpeg_last_stderr` |
| monitor | `enabled`, `device`, `buffer_ms`, `alive`, `frames_dropped_total` |
| stt | `state` (now including `paused`), `reconnects_total`, `buffer_drops_total`, `pauses_total`, `paused_sec`, `segments_total`, `lines_total`, `bytes_sent_total`, `last_error`, latency last/p50/p95/max/samples, per-phase (upload/recognize/assemble) latency last/p50/p95/max plus `phase_latency_samples` |
| web | `sse_clients`, `sse_clients_total`, `events_total`, `slow_disconnects_total`, viewer-reported latency last/p50/p95/max/samples, `viewer_reports_total` |
| transcript | `path`, `lines_written`, `bytes_written`, `last_write_error` |
| process | `version`, `session_id`, `started_at`, `uptime`, `goroutines`, `health` |

`source.frames_dropped_total` no longer has a caller. It used to double as the counter for the
Deepgram reconnect ring evicting buffered audio, but that's an STT-side event, not a source-side
one, so it moved to `stt.buffer_drops_total` — and an eviction that happens while the gate is
paused is the pre-roll buffer working as designed (see Auto-pause, above) and isn't counted at all
any more. The field stays in `Source` as the slot for a genuine source-side drop, should ffmpeg or
the capture backend ever need one; it just isn't the metric to watch for reconnect-driven loss —
`stt.buffer_drops_total` is.

**Latency** is `ReceivedAt − CapturedAt`: how far behind wall clock the caption for a given piece of
audio arrived. `CapturedAt` is the wall-clock instant the audio was released into the pipeline
(`audio.Frame.CapturedAt`), not a media-time offset from stream start — a per-connection
`anchorIndex` (`internal/stt/deepgram/anchor.go`) maps the media-time `start`/`duration` Deepgram
echoes back to that wall-clock instant, interpolating within the chunk it falls in. A stream-relative
anchor (`streamStart + Start + Duration`) doesn't work here: auto-pause, reconnects, ring evictions
and ffmpeg restarts all move the media clock relative to the wall, so on a long session with quiet
stretches a stream-relative figure grew without bound instead of reporting real latency. Anchoring to
the wall-clock capture instant sidesteps all of that.

The figures are windowed, not session-lifetime. `latencySeries` (`internal/metrics/metrics.go`)
keeps only samples from the trailing `latencyWindow` (5 minutes), capped at `latencyCap` (512
samples) as a memory bound; both percentiles and `max` are computed over that window on read,
and trimming happens on every read too, so an idle session's window actually empties out rather
than showing stale figures forever. There used to be a non-decaying session-lifetime max; it's
gone on purpose. With `--auto-pause` on by default, `writeLoop` flushes up to ~2 s of
genuinely-old ring pre-roll after every resume (§3), so the first segment after each silence is a
true but large latency reading — under a non-decaying max that dragged `p95`/`max` up permanently
on a schedule tied to room silence, not to any actual pipeline problem. Windowing lets that spike
age out instead of defining the headline figure for the rest of the session.

There is one latency series now, not two (`stt.latency_*_ms`), because there is only one kind of
segment left to time: every transcript the pipeline emits is already settled, so the headline
figure *is* time-to-first-pixels — there is no earlier, more optimistic interim paint to also
track, and no need to reconcile two figures that used to describe different moments. `observeLatency`
(`internal/cli/run.go`) measures `ReceivedAt − CapturedAt` for every transcript with non-empty
`Text`. (The empty-`Text` guard is now belt-and-braces rather than load-bearing: it used to exist
because Deepgram's synthetic `UtteranceEnd` transcript carried no real media range and could let
`anchorIndex.At(0)` resolve an unrelated capture instant; `UtteranceEnd` is gone — see §4's "Signals
we deliberately don't use" — and `decodeTranscript` already rejects a Results message with an empty
alternative, so nothing in this pipeline can reach the guard today. It stays as a defense against a
future engine emitting a synthetic zero-range result.)

`observeLatency` additionally splits the total into three phases using
`Transcript.SentAt` — the wall-clock instant the audio was handed to the recognizer's socket,
carried alongside `CapturedAt` through `anchorIndex.Add`/`At` — plus the hub's own publish
instant: upload (`CapturedAt → SentAt`), recognize (`SentAt → ReceivedAt`), assemble
(`ReceivedAt → publish`). `Metrics.ObservePhases` records all three under one lock or none at
all, so the split is exact *per sample*: for any one segment, upload + recognize + assemble equals
that segment's own `CapturedAt`→publish span. A partial write would let a segment describe a
different transcript than its neighbours, which is why the three are all-or-nothing.

Note the limit of that guarantee: it is per-sample, not per-percentile. The `/admin` waterfall
draws p50 per phase, and percentiles of three separate series do not add — the p50 of upload plus
the p50 of recognize is not the p50 of the total, since the slowest upload and the slowest
recognition need not have happened on the same caption. The bar therefore shows the *composition*
of a typical caption, not an arithmetic decomposition of the headline figure above it, and its
segments are scaled against the sum of the p50s rather than against that headline. Exposed as
`stt.{upload,recognize,assemble}_latency_{last,p50,p95,max}_ms` plus
`stt.phase_latency_samples`. Capture itself (ADC → `CapturedAt`) is not part of this split and
remains unmeasured; the waterfall draws it as an explicitly-labelled, fixed-width hatched segment
rather than pretending to scale it.

The browser leg — publish to actual paint — is measured by the viewer itself and reported
voluntarily: `GET /api/time` lets the page estimate its clock offset against the server (an
uncorrected phone clock is routinely seconds off and would otherwise leak straight into the
figure as fake latency), and `POST /api/viewer-latency` accepts the viewer's own
`requestAnimationFrame`-measured publish→paint span, throttled to 1/sec and withheld until an
offset has actually been measured. It's the first POST route in the codebase, and unauthenticated
on the LAN by design, so it carries its own guards: a bounded request body, a finite-and-in-range
check on the value, and a rate limit shared across all clients rather than per-client. Landed as
`web.viewer_latency_*_ms` and `web.viewer_reports_total`.

See `specs/2026-08-20_analysis_caption_latency_measurement.md` for the full accuracy analysis
this anchor came from, updated 2026-08-22: the anchor-bug findings, the windowed ring/max, and the
browser-side instrumentation described above are all now fixed. That record's interim/final split
was itself superseded shortly after by the removal of interim results altogether (see §4, above) —
the spec's own S5 section carries a note pointing here. Only the capture side — ADC/USB/audio-server
delay before `CapturedAt`, and recovering ffmpeg's
own PTS — remains open, deliberately deferred as the smallest term in the latency budget; see
"Remaining work" there.

**`Snapshot.Health`** (`"closed"` / `"paused"` / `"degraded"` / `"ok"`) is the server-computed
answer to "what is happening right now," and it's what `/admin`'s badge switches on — a closed or
paused connection reports exactly that rather than a generic "degraded," and once the link is up
again the badge recovers instead of latching at "degraded" for the rest of the session. A point
event (a buffer drop, a reconnect) only holds `Health` at `"degraded"` for `degradedWindow` (60 s)
after it happens, so a single blip from an hour ago doesn't flag the badge forever — that's the fix
for the earlier permanently-latched "Degraded" state. The one asymmetry worth knowing: a standing
transcript write error holds `Health` at `"degraded"` regardless of that window, because unlike a
point event it's a condition, not something that happened once — it stays true for as long as
writes are actually failing, and aging it out on the same timer would let the badge go green while
the transcript diagnostic panel right below it is still showing a live error.

**`Snapshot.Clean()`**, by contrast, answers "was this whole session clean": it reports whether
every degradation counter was zero from start to finish, and never recovers once something has gone
wrong, unlike `Health`. It's the cumulative counterpart to `Health`'s live snapshot — for a caller
that wants a single end-of-session yes/no rather than the moment-to-moment picture.

---

## 7. Transcripts

**On by default for every session.** Recording is the expected behaviour, not something to remember
to enable. `--transcript-dir` (default `./transcripts`, or `LIVECAPTION_TRANSCRIPT_DIR`) relocates
it; `--no-transcript` is the explicit opt-out.

Per session, `<dir>/<YYYY-MM-DDTHH-MM-SS>/`:

- `transcript.txt` — `[00:12:34] text`, for humans
- `transcript.jsonl` — one record per line with offsets and timestamps, for tooling

Both `O_APPEND`, buffered, flushed every 2 s and on close, so a crash keeps what already landed.
Write failures surface as a metric rather than an error — losing the transcript must not end a live
event.

---

## 8. CLI

`github.com/alecthomas/kong`. Struct tags declare env binding, enum validation, file-existence
checks (`--logo`, like `replay`'s file argument) and grouped help, removing a page of hand-written
validation. Subcommands rather than one flag selecting a source, because replay and live take genuinely
disjoint options.

```bash
livecaption devices                              # find the soundboard
livecaption replay recording.mp3 --engine mock   # primary dev loop, no API cost
livecaption replay recording.mp3 --monitor       # hear it while watching captions land
livecaption live --device <soundboard>           # the real thing
```

Shutdown: SIGINT/SIGTERM → cancel → ffmpeg stops → frames channel closes → engine sends
`CloseStream` and drains → hub flushes the open utterance → transcript flushed → HTTP server
`Shutdown` with a 5 s grace. A second Ctrl-C exits immediately.

---

## 9. Terminal output

The governing rule: **captions are data, logs are diagnostics.** Finalized captions go to
**stdout**; everything else to **stderr**. So `livecaption replay f.mp3 > captions.txt 2> run.log`
splits cleanly and piping never mixes the two.

`internal/ui` owns the terminal behind one mutex — the status line and the log handler both target
stderr, and without a single owner they interleave into garbage.

Log rendering resolves by TTY detection:

| | captions | status line | logs |
|---|---|---|---|
| **terminal** | coloured | pinned, redrawn 2×/sec | pretty, coloured, relative timestamps |
| **piped / systemd** | plain | none | `slog` JSON |

```
livecaption 0.1.0
  source      replay  recording.mp3 (31:51, 44100 Hz stereo -> 16000 Hz mono)
  monitor     pulse:default (~80ms buffer)   perceived delay overstates actual by this much
  stt         deepgram  model=nova-3  language=en-US
  transcript  ./transcripts/2026-08-19T09-31-05
  viewer      http://localhost:8080
  admin       http://localhost:8080/admin

ready — Ctrl-C to stop
[00:00:12] Good morning everyone, thanks for coming out today.
▶ 00:04:31 / 31:51 │ stt ● connected │ lat 340ms p95 610ms │ 2 viewers │ 47 lines
```

**Only closed transcript lines are printed to the terminal, never individual segments** — at
a stable-prefix cadence a line can be several segments, and printing each as it lands would flood the
scrollback; the web page is what the segment-by-segment view is for.

Log levels, each earning its place:

- `error` — the run cannot continue (ffmpeg won't start, key rejected, port in use)
- `warn` — degraded but running (reconnect, xrun, viewer dropped). *The level that matters
  mid-event*, and the one that gets a coloured glyph.
- `info` *(default)* — lifecycle only; a two-hour event should produce well under 50 lines
- `debug` — per-utterance results, backoff decisions, ffmpeg stderr, SSE connect/disconnect

No per-frame logging at any level — at 100 ms chunks that's 10 lines/sec of noise.

---

## 10. Operating notes

- **Feed a mono aux/matrix send of the mics, not the main mix.** Music beds and effects degrade
  accuracy badly and `-ac 1` will happily downmix all of it. This is the single biggest accuracy
  lever in the project — larger than any model or parameter choice.
- **`--keyterm`** with the event's proper nouns (names, places, in-house terms) is the second
  biggest lever, and costs nothing.
- **Deepgram bills by streamed audio duration**, and replay at 1.0× costs exactly what live costs.
  Use `--engine mock` for all UI work.
- If captions feel late, Deepgram's `endpointing` is the knob (set it in `dialURL`): lower puts text
  on screen sooner in smaller pieces, at the cost of `segments_total / lines_total` climbing on
  `/admin` as phrases fragment.
  `breakGap` is the companion knob for when a pause reads as a paragraph break rather than a
  breath — see §4. Tune both by ear with `--monitor` *before* the event.
- 401 on first connect is the most likely first-run failure — check `DEEPGRAM_API_KEY`.

## 11. Layout

```
cmd/livecaption/main.go     entrypoint, signal handling
internal/cli/               kong command tree, shared wiring
internal/ui/                terminal ownership, slog handler
internal/audio/             Source interface, ffmpeg plumbing, file/device/monitor
internal/stt/               Engine interface + silence gate; deepgram/, mock/
internal/caption/           hub (utterance assembly, fan-out), transcript writer
internal/metrics/           counters, windowed latency series, snapshot
internal/web/               routes, SSE, embedded viewer + admin pages
```
