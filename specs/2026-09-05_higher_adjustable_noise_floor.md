# Higher adjustable noise floor

## Issue and goal

The microphone picks up whispering during long stretches without intended speech. Give the operator visibility into incoming audio levels and live controls to suppress quiet audio sent for captioning, without cutting normal speech or requiring a restart to tune settings.

This is an amplitude gate, not whisper detection, speaker selection, or a privacy guarantee. A nearby whisper can be louder than wanted speech. The operating assumption is that a conservative threshold can separate these levels well enough during extended silence.

## Agreed scope

- Display incoming RMS and recent peak levels in dBFS on the admin page.
- Show the active threshold on the meter scales, with a numeric label.
- Adjust both threshold and gate release time live from the admin page.
- Show the active settings, startup defaults, and built-in defaults.
- Filter only the audio sent to STT; leave `/audio.mp3` unchanged.
- Keep connection auto-pause separate from the audio gate.
- Live changes are session-only. Restart restores CLI/environment settings.
- Do not retract captions or retroactively reprocess buffered/sent audio.

## Gate behavior

Measure each original input frame before filtering:

1. A frame whose RMS is strictly above the threshold opens the gate immediately and restarts the release timer.
2. Below-threshold frames continue passing while the release timer has not expired.
3. Once the release time since the last above-threshold frame expires, replace below-threshold PCM with silence.
4. Below-threshold whispering does not reset the timer. Above-threshold whispering does reset it and can reopen the gate.
5. Start closed until the first above-threshold frame. Do not add an attack delay.

Use media time for release timing, following the existing silence-gate pattern. Handle backwards offsets by rebasing timing rather than retaining a stale deadline. Preserve frame sizes, offsets, and capture timestamps; never drop frames to implement suppression.

A relatively long release is intentional: protect word endings and natural pauses, accepting that whispers pass during that interval. The goal is suppression during extended quiet periods, not between every word.

### Settings and defaults

Proposed initial defaults, subject to real-microphone tuning:

| Setting | Built-in default | Accepted range | Meaning |
|---|---|---|---|
| Caption threshold | -35 dBFS | -100 to 0 dBFS | Open when frame RMS exceeds this level. Higher/less negative suppresses more audio. |
| Gate release time | 3 seconds | 0 to 60 seconds | Keep passing audio this long after the last above-threshold frame. Zero closes on the first below-threshold frame. |

These meanings will also be on the admin page as a tooltip to explain the levels and directions

Expose startup flags `--noise-threshold-dbfs` and `--noise-release`, with the existing automatic `LIVECAPTION_*` environment equivalents. The -35 dBFS and 3-second defaults are operator-selected starting points, not measured whisper/speech calibration.

At -100 dBFS effectively all nonzero 16-bit input passes; at 0 dBFS the gate cannot open. Explain the upper endpoint as effectively muting caption input. Validate finite values and ranges in both CLI and HTTP paths, including explicit zero values.

Live Apply updates both settings together. They take effect on the next input frame. Keep the last above-threshold timestamp: shortening release can close the gate on that frame, but changing release alone must not reopen an already closed gate without above-threshold input. Threshold changes do not reinterpret historical frames. Display the confirmed server values, not merely what the browser attempted to send.

## Admin meters and controls

Place these in a dedicated **Caption audio gate** card next to Source, not inside the Source or MP3 listener-stream card:

- **Input RMS (dBFS):** original, pre-filter frame level; this is what the gate compares.
- **Recent peak (dBFS):** highest absolute sample level over approximately the last second, so brief overloads remain visible with the existing one-second polling.
- Both meters use the same -100 to 0 dBFS scale and show a labeled threshold marker. Explain that the threshold operates on RMS, not individual peaks.
- Show numeric levels and threshold alongside the visual meters; do not rely on color alone.
- Show **caption gate open/closed**, separately from STT connected/paused. Because this is totally separate, and STT paused should always lag the gate being closed.
- Label peak proximity to 0 dBFS as low headroom, not proof that the original analog input is undistorted.
- Before the first sample, or when samples stop arriving, show unavailable/stale rather than a misleading live value. Include sample freshness in stats.

Provide labeled numeric controls for threshold and release time, and an **Apply** button. Show startup and built-in defaults alongside them, plus: “Changes apply to this session only; restart restores startup settings.” Do not overwrite an operator's in-progress edits on every stats poll; keep active values visible separately. Disable mutation controls when admin controls are unavailable and report Apply failures clearly.

This is digital input level, not calibrated room loudness (dB SPL), and not an automatic estimate of ambient noise floor. The operator observes quiet-room, whisper, and intended-speech levels to tune the threshold.

## Pre-roll and in-flight audio

The existing STT ring retains approximately two seconds of audio during auto-pause to protect speech onset on reconnection. This is more than a few milliseconds already in flight: if filtering happened only while connected, unfiltered whispering retained during a pause could be sent later.

The simple solution is placement: filter new frames before they enter that ring, continuously whether connected, reconnecting, or paused. No special buffer cleanup or retrospective filtering is needed.

After a settings change, previously queued or sent audio may still produce captions briefly. Accept this; do not flush the buffer or retract captions. Already suppressed audio cannot be recovered by pre-roll, so overly aggressive thresholds can still remove quiet speech onset. That is a tuning limitation, not a reason to add a second unfiltered pre-roll mechanism.

## Implementation plan

### 1. Startup configuration and shared gate settings

- Add the two flags and validation in `internal/cli/cli.go`; wire their startup values in `internal/cli/run.go`.
- Add a small concrete audio-gate implementation under `internal/audio/`, with synchronized settings and frame-driven state. No new dependencies or generic processing framework.
- Share that instance between the processing path and admin handlers through session wiring. Apply threshold and release atomically; do not use metrics as mutable configuration storage.
- Keep the existing `internal/stt/gate.go` connection-pause mechanism and its 60-second default hold separate. Do not repurpose its hold as the noise-gate release time.

### 2. Meter and filter on the common input path

- In `internal/cli/run.go`, process source frames before the engine, covering live/replay and all engines, including mock.
- Reuse `audio.RMSDBFS` from `internal/audio/level.go`; add the small sample-peak calculation needed for headroom display.
- Measure original PCM, record levels/freshness, then gate frames before STT buffering. Metering must continue while STT is paused and when auto-pause is disabled.
- Keep the optional playback monitor on the original input for diagnosis. Do not mutate PCM shared with the monitor; replace the frame's buffer when silencing it.
- Leave the separate FFmpeg MP3 output untouched.
- Carry each frame's caption-gate decision alongside its filtered PCM. The existing auto-pause gate uses that decision instead of independently comparing against -45 dBFS: its hold starts after caption-gate closure, and opening the caption gate resumes recognition. This honors the requirement that STT pause lag caption-gate closure even with a low caption threshold or a long release. Frames without noise-gate metadata retain the original RMS-based behavior.

### 3. Metrics and authenticated live settings

- Extend `internal/metrics/metrics.go` and `/api/stats` with original input RMS, recent peak, last-sample time, and the effective gate state/settings needed by the UI. Keep settings reads authoritative through the shared gate, not a separately writable copy.
- Recent-peak reads must not reset the peak for other admin clients; maintain a short time-based window independent of polling.
- Add an authenticated `POST /api/noise-gate` in `internal/web/server.go`, reusing the existing admin control guard. Accept both settings in a bounded JSON body; reject missing, malformed, non-finite, or out-of-range values before changing either setting. Require JSON and reject cross-origin mutation requests rather than accepting form submissions with browser-cached Basic credentials.
- Return effective settings after Apply. Log operator setting changes, not every audio frame.
- No disk writes, environment-file rewriting, new database, or new streaming endpoint.

### 4. Admin UI

- Extend `internal/web/static/admin.html` using native HTML/CSS controls and the existing one-second stats poll.
- Add aligned meter scales and threshold markers, numeric readouts, sample freshness, and gate state.
- Add both live controls, defaults, session-only notice, and Apply feedback. Reuse the existing server-enabled admin-control mechanism.

### 5. Verification

Add focused runnable coverage in the existing Go test setup:

- Above/below/equal threshold, release expiry, zero release, and backwards media offsets.
- Sustained below-threshold whisper-level input closes the gate even though samples remain nonzero; periodic above-threshold input keeps it open.
- Live changes while open and closed, with concurrent processing/settings reads.
- Frame timing/length preservation and no mutation of original PCM.
- Filtered pre-roll and interaction with existing auto-pause/resume; no buffering path bypasses filtering.
- RMS/peak readings, recent-peak expiry, and missing/stale input representation.
- HTTP auth/disabled controls, validation, and all-or-nothing updates in `internal/web/server_test.go`.

Run `go build ./...`, `go test ./...`, `go test -race ./...`, and `golangci-lint run ./...` after implementation. Do not start the application binary. Browser meter/control behavior and actual microphone tuning require user verification against their running instance.

## Challenges: effort and mitigation

Effort is relative implementation/testing work, not a delivery estimate.

| Challenge | Effort | Mitigation level | Decision |
|---|---|---|---|
| Existing auto-pause does not suppress quiet audio | Medium | High | Separate pre-buffer PCM gate; retain connection auto-pause. |
| Losing quiet words or endings | Low code; field tuning | Partial to high | Conservative threshold and live-adjustable, generous release. Quiet speech onset can still be lost. |
| Continuous whispering | Field tuning | Conditional | Below-threshold whispers never refresh release. Above-threshold whispers remain indistinguishable from wanted speech. |
| Pre-roll bypassing suppression | Low with correct placement | High | Filter before buffering; no retrospective cleanup. |
| Queued captions after Apply | No extra work | Accepted limitation | New settings affect new frames only. |
| Meter accuracy and overload visibility | Low–medium | High for digital levels; partial for upstream distortion | Pre-filter RMS, recent peak, freshness, clearly labeled dBFS. |
| Live threshold and release controls | Medium | High | Synchronized settings, authenticated validated Apply, confirmed UI state. |
| Durable live settings | Additional operational complexity | Deferred | Session-only changes; CLI/environment startup defaults. |
| Listener-audio suppression | Additional audio-routing work | Out of scope | `/audio.mp3` remains original audio, explicitly documented. |

## Deferred

- Persisting admin changes across restarts.
- Automatic noise-floor estimation, adaptive thresholds, or speech/whisper classification.
- Filtering the listener MP3 stream.
- Retroactive buffer processing or caption retraction.
- Calibrated acoustic loudness measurement or a guarantee of upstream clipping detection.

Revisit these only if actual operation shows sufficient value to justify the added complexity.
