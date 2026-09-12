## Why

An unavailable configured input currently terminates startup, leaving a headless appliance operator without the web console that could explain the fault. Keep the appliance reachable and make the missing input actionable in the existing admin status and Source card.

## What Changes

- Continue serving the web console when live-input validation or opening fails; retain the configured device identity and report persistent degraded health.
- Show an explicit missing/unavailable input state in the admin Source card, using the existing overall health icon rather than adding another status system.
- Add failure-only hover guidance, also accessible without hover: check USB connections; otherwise SSH into the appliance and follow device discovery/configuration in `deploy/README.md`.
- Reuse existing FFmpeg capture retries where safe. A device rejected by enumeration remains blocked until restart, preserving the PulseAudio wrong-device fallback safeguard without adding device rediscovery. Guidance distinguishes automatic retries from restart-required failures; configuration changes always require restart.
- Log the configured backend/device, failure reason, and recovery action at warning level.
- Leave configuration reload, input switching, new recovery loops, and unrelated startup failures out of scope. A future console button to restart the server without restarting the appliance is also out of scope.

## Capabilities

### New Capabilities

- `live-input-availability`: Reachable degraded startup, persistent input health, accurate admin guidance, and existing-path recovery for unavailable live inputs.

### Modified Capabilities

None. The local OpenSpec capability inventory is empty.

## Impact

- `internal/cli/commands.go` and session wiring in `internal/cli/run.go`: retain validation diagnostics without terminating the web session or opening a known-rejected input.
- `internal/audio/device.go`: replace the fatal device-opening probe gate with the existing capture retry path and expose input availability. `internal/audio/devices.go` validation remains authoritative; `internal/audio/ffmpeg.go` remains the shared subprocess/stderr implementation, not a home for device-specific recovery policy.
- `internal/metrics/metrics.go`: additive source availability/recovery fields in `/api/stats` and persistent degraded health, including while STT is paused.
- `internal/web/static/admin.html`: existing Source card and health indicator; existing authentication unchanged.
- Focused audio, CLI, metrics, HTTP, and dependency-free JavaScript checks; `deploy/README.md` troubleshooting and `CHANGELOG.md` updated during implementation. No dependencies, new endpoints, or kiosk changes.
