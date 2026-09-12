## 1. Current input health

- [ ] 1.1 Add synchronized live-source state, current error, and restart-required fields to `internal/metrics/metrics.go` and its JSON snapshot, preserving source identity and replay defaults. Verify metrics tests cover initial state, active fault, and clearing a fault while retaining historical stderr.
- [ ] 1.2 Make active live-input faults override paused STT health but not closed-session health. Verify table-driven metrics tests cover all STT states, an expired degradation timestamp, silence/normal pause, recovery with a recent restart, and unrelated existing degradation precedence.

## 2. Degraded startup and existing capture retries

- [ ] 2.1 In `internal/cli/commands.go`, retain enumeration rejection as a blocked-device reason rather than returning before session creation; initialize metrics before HTTP starts and wire concrete device callbacks. Verify CLI tests with deterministic device listings preserve selected identity, keep rejection restart-required, and retain empty-enumeration warnings and existing `ResolveDevice` safeguards.
- [ ] 2.2 In `internal/audio/device.go`, support the blocked-source path without launching capture, keeping its frame channel open until context cancellation. Verify an audio test proves no capture launch, no fallback, and prompt channel closure on cancellation.
- [ ] 2.3 Remove the fatal capture probe gate and now-unused probe-only helpers/argument branches; preserve missing-FFmpeg prerequisite errors and enter the existing retry loop for unblocked sources. Add availability callbacks for failed capture and actual PCM recovery; keep existing backoff and frame/MP3 behavior. Replace `TestStartOnBadDeviceFailsPromptly` with deterministic failure/retry/recovery/cancellation checks, update argument/callback tests, and verify `go test ./internal/audio` passes without requiring attached input hardware.
- [ ] 2.4 Include backend, selected device, diagnostic, and retry/restart action in warning-level logs. Verify captured-log tests cover enumeration rejection, capture failure including EOF without stderr, and bounded retry behavior without adding a logging loop.

## 3. Admin feedback and integration coverage

- [ ] 3.1 Update `internal/web/static/admin.html` Source card with current state/error, failure-only native hover text, and the same visible troubleshooting guidance. Preserve the configured device and existing health icon, distinguish restart-required from retrying, clear current fault help on recovery, and render diagnostics as text. Use only "restart the server" for restart guidance, without a shell command; do not add the future console restart button or supporting endpoint/machinery. Extend the dependency-free Node/VM checks to verify that wording, both failure modes, recovery, replay/older snapshots, and HTML-like diagnostic strings; run `node internal/web/admin_viewer_test.js`.
- [ ] 3.2 Add HTTP coverage in `internal/web/server_test.go` for `/admin`, `/api/stats`, viewer, and unchanged `/healthz` while metrics reports missing/unavailable input; verify new source fields and existing authentication with in-process servers via `go test ./internal/web`.
- [ ] 3.3 Add a CLI/session regression using the mock engine and deterministic unavailable input to show both startup failure paths keep the session alive until cancellation, without launching the app binary or external service. Verify `go test ./internal/cli` and ensure test listeners/goroutines are cleaned up.

## 4. Documentation and final verification

- [ ] 4.1 Update `deploy/README.md` troubleshooting with reachable degraded startup, Source-card guidance, the two recovery paths, and restart after changing device settings. Add the primary application entry in `CHANGELOG.md`; verify both documents agree with the spec and no kiosk files change.
- [ ] 4.2 Run `go build ./...`, `go test ./...`, `golangci-lint run ./...`, and `node internal/web/admin_viewer_test.js`; fix findings in touched Go files and record results. Do not start the application, `just run`, a built binary, or a background server.
- [ ] 4.3 Deliver a user-run browser/appliance checklist covering missing-input boot, selected-device visibility, degraded icon, hover/non-hover guidance, USB reconnect on the retrying path, appliance power-button restart on the validation-blocked path (service restart is an alternative for operators familiar with `systemctl`), and runtime unplug/replug. Explicitly record that real browser/hardware behavior remains unverified here; use the user's existing instance rather than starting one.
