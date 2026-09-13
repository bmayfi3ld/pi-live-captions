## Context

See `proposal.md` for motivation. This crosses CLI startup, capture, metrics, and admin rendering, so a design is warranted.

Observed flow:
- `LiveCmd.Run` enumerates devices and returns immediately on `ResolveDevice` rejection, before constructing the session. That validation prevents PipeWire/Pulse from silently substituting a default input. Empty enumeration intentionally warns and proceeds; ALSA `hw:`/`plughw:` aliases bypass enumeration.
- `session.run` starts HTTP before `Source.Start`, but a source error unwinds the session and shuts HTTP down.
- `DeviceSource.Start` probes synchronously and returns its error. Only after a successful probe does it start its existing retry loop (250 ms exponential backoff capped at 8 seconds).
- `captureOnce` already reports PCM frames, restarts, xruns, and stderr through callbacks. `ffmpeg.go` is shared by live capture, replay, and other subprocess users; device policy belongs in `device.go`, not the shared launcher.
- Metrics has source identity and historical stderr, but no current input fault. Health currently gives paused STT precedence over degradation, and restart events age out after 60 seconds.
- Admin `#health` already maps `degraded` to amber. `#card-source` shows `source.spec` and historical stderr; stats are polled. `deploy/README.md` steps 5–6 cover service-user discovery and configuration.
- There are no local OpenSpec main specs. Existing code/tests explicitly assert fail-fast startup; those contracts must change. Existing wrong-device validation tests remain valid.

## Goals / Non-Goals

**Goals:** Keep one source of truth in metrics; reuse the current session and retry machinery; ensure an input fault remains visible independently of STT or historical counters.

**Non-Goals:** General startup-error recovery, changed authentication or liveness semantics, runtime config reload, input selection UI, device rediscovery polling, new retry controllers, USB detection heuristics, a console restart button, or changes to replay behavior.

## Decisions

### 1. Preserve enumeration safety, but retain a web-only session on rejection

Keep `ResolveDevice` behavior intact. In `LiveCmd.Run`, capture its error instead of returning, construct the normal session, and initialize a missing-input fault before serving HTTP. Carry that rejection into the concrete device source as an initial blocked reason. A blocked source returns a frame channel that remains empty until context cancellation, then closes; it never launches capture. The normal session can remain alive without inventing an alternate HTTP-only server or a new `Source` implementation.

Mark this state restart-required and log the rejection once with backend, device, reason, and action. Do not periodically enumerate to clear it. Retain the existing warning-and-proceed behavior for empty enumeration, which is not proof of absence.

Alternative rejected: simply downgrade validation to a warning and launch FFmpeg anyway. That reintroduces the known wrong-input Pulse fallback. Rediscovery would add recovery machinery the request explicitly declines.

### 2. Let ordinary capture enter its existing retry loop immediately

For unblocked sources, remove the synchronous capture-only probe gate and its now-unused probe helpers/argument branches. Start the existing loop directly and keep its cadence and cancellation behavior. Preserve a cheap FFmpeg executable preflight so a missing executable remains a startup prerequisite error rather than an input-missing condition. Keep other existing CLI/configuration validation unchanged.

Add a concrete device availability callback alongside the existing metric hooks. Report `unavailable` with the capture error when an attempt fails (including an unexpected EOF with no stderr); report `capturing` on actual PCM delivery, not on process launch. Initialize unblocked live sources as `starting` before HTTP startup; starting is not proof of missing hardware. Clear the current fault on recovered frames, not on a successful subprocess launch. Keep historical stderr independent.

The callback handles runtime disconnects too because they already travel through the same loop. Existing warning logs gain explicit backend/device and recovery action. No new loop or configuration reload is introduced. Shared `ffmpeg.go` needs no device-state policy; any necessary changes there must stay narrowly about preserving existing diagnostic/process behavior.

Alternative rejected: hold every failed source forever until restart. It discards recovery already provided by the capture loop. Also reject inferring availability from frame totals, elapsed silence, or stderr text: totals are historical, silent PCM is valid, and stderr can report unrelated warnings.

### 3. Add current source state to the existing stats snapshot

Use additive source fields:
- `state`: `starting`, `capturing`, `missing`, or `unavailable` for live sources; omitted/empty for replay.
- `error`: current input-fault diagnostic, empty outside a fault.
- `restart_required`: true for enumeration-rejected sources; false for the existing retry path.

Keep `source.spec` unchanged. Store mutable state under the existing metrics mutex and wire updates through the concrete device callbacks. Avoid importing metrics into audio.

Health precedence becomes: closed session; active live input fault => degraded; paused STT; existing recent/persistent degradation rules; ok. This narrowly changes pause precedence for an actual missing/unavailable input without changing pause precedence for old drop events or transcript errors. Clearing the input fault need not immediately turn health green: the existing recent-restart window and unrelated faults still apply.

Alternative rejected: rely on `FFmpegRestart` alone. It neither identifies the device problem nor survives STT pause precedence or a non-retrying validation rejection.

### 4. Extend the existing Source card rather than build another status panel

Render the new live-source state and current diagnostic next to the existing selected device. Preserve `#health` rendering. Use text content for device/error strings, never interpolate them as HTML.

Use a failure-only visible help paragraph without a card `title` or hover tooltip. Wording is conditional rather than attempting to infer USB hardware from an ALSA/Pulse identifier:

> If this is a USB device, check that it is plugged in. Otherwise, or if that does not help, SSH into the appliance and follow deploy/README.md steps 5–6 to find and configure the input.

Append one of:
- Blocked validation: `After correcting the connection or configuration, restart the server.`
- Retrying capture: `The server retries this configured input automatically and can recover when it is available again. If you change the input configuration, restart the server.`

Treat restart as an appliance-level operator action: using the appliance power button to restart it is the standard method. Operators familiar with `systemctl` can instead restart the service, but operator-facing guidance simply says "restart the server" without prescribing either mechanism. A future console button will restart the server without restarting the appliance; this change adds no button, endpoint, or supporting restart machinery.

Clear the fault help when capture resumes. Historical stderr may remain, clearly separated from the current state. Replay and responses without the additive fields must not show a fabricated missing-input warning.

## Risks / Trade-offs

- [Enumeration-rejected devices do not auto-recover] → Explicit restart guidance; avoids a new rediscovery loop and wrong-input fallback. This is deliberately different from capture-open failures, which already have retries.
- [Empty enumeration cannot prove a device missing, and Pulse can substitute defaults] → Preserve the existing best-effort validation limitation; never bypass a known rejection. Stronger device identity verification is outside this change.
- [A blocked source leaves STT running without frames] → Keep its channel open until cancellation, matching existing retry outages; verify session lifetime with the mock engine and normal shutdown rather than adding STT lifecycle policy.
- [FFmpeg capture diagnostics cannot reliably distinguish missing hardware from permission/busy errors] → Use `Missing` only for enumeration rejection and `Unavailable` for capture failures, with the real diagnostic. Do not add brittle stderr classifiers.
- [Concurrent callbacks or retries could leave stale state] → Guard metrics updates and test failure-to-frame transitions, cancellation, and recent-event health separately.
- [Removing the probe changes existing tests and eliminates its five-second read timeout] → Verify prompt asynchronous startup and cancellation; do not introduce a speculative stalled-device watchdog. Missing-device open failures are the target, not indefinitely silent/blocking drivers.
- [Visible browser rendering cannot be verified from this environment] → Test the DOM rendering with existing Node/VM patterns, and leave actual browser/hardware verification to the user.

## Migration Plan

No persisted schema or configuration migration. Update `CHANGELOG.md` and the deployment troubleshooting table during implementation, documenting the two recovery paths and restart after configuration edits. Deploy via the existing package/service restart flow. Rollback to the previous package restores fail-fast behavior; no data conversion is needed. No kiosk package or kiosk changelog changes are required.

Verification uses `go build ./...`, `go test ./...`, `golangci-lint run ./...`, and dependency-free Node checks. HTTP coverage belongs in `internal/web/server_test.go` using in-process servers. Never start the application binary or a background server to verify this change.
