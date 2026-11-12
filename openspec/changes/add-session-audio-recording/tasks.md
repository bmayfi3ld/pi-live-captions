# Tasks

## 1. Output configuration

- [ ] 1.1 Add default-on negatable `--audio-recording` and positive `--transcript-max-storage=50GB` to `internal/cli/cli.go`, with environment support and checked integer-byte/decimal-unit parsing; extend `cli_test.go` for defaults, opt-outs, flag precedence, accepted units, invalid values, and overflow, and verify `go test ./internal/cli` passes.
- [ ] 1.2 Document the two flags, units, effective recording/no-transcript behavior, and independent HTTP stream setting in `docs/usage.md`; verify the documented names/defaults match parser tests and generated help without starting a session.

## 2. Startup retention

- [ ] 2.1 Add a retention helper under `internal/caption` that measures regular-file bytes without following symlinks, collects opaque immediate child directories, and orders them by modification time then name; verify unit tests cover missing/empty roots, nested files, loose root files, legacy bundles, symlinks, ordering ties, and size overflow.
- [ ] 2.2 Implement whole-directory pruning until usage is strictly below the budget, with scan-before-delete, first-error termination, and removed-count/reclaimed-byte results; verify deterministic small-byte tests cover below/equal/over budget, multiple removals, unknown artifacts, scan/deletion errors, partial cleanup, and an unsatisfiable budget without deleting loose files or external targets.
- [ ] 2.3 Invoke retention in `newSession` before `caption.NewWriter`, skip it entirely for `--no-transcript`, and report cleanup results/errors; verify session tests show cleanup precedes directory creation, audio opt-out still prunes, failures prevent capture/new directories, and no runtime retention hook is installed.
- [ ] 2.4 Document the destructive default, mtime ordering, dedicated single-writer root, startup-only threshold, legacy-directory deletion, and failure policy in `docs/usage.md`; verify each retention contract has both a test and matching operator guidance.

## 3. Bounded MP3 recorder and diagnostics

- [ ] 3.1 Add the asynchronous recorder under `internal/audio` with a fixed eight-chunk queue, immutable byte ownership, a single file-writing worker, idempotent completion, and byte/error reporting; verify unit tests for exact ordered bytes, no-listener recording, EOF drain, and repeated close using injectable writers.
- [ ] 3.2 Implement nonblocking retirement on overflow/write failure plus a bounded finalization wait; verify controllably stalled writers cannot block encoded publication and that create/write/short-write/close/overflow/timeout faults are reported once, never silently resume recording, and test workers are released afterward.
- [ ] 3.3 Add recording enabled/state/path/bytes/error data to metrics and terminal banner/summary, with persistent failure visibility and audit-compatible logging; verify metrics/UI tests cover disabled, unavailable, healthy, failed, and incomplete recording and that permanent errors do not age out.
- [ ] 3.4 Add a recording tap to the shared MP3 fan-out rather than using a normal HTTP subscription; extend `broadcast_test.go` to verify recorder and healthy listeners get identical bytes, absent/slow listeners do not affect recording, and recorder failure does not block other outputs.
- [ ] 3.5 Document recorder guarantees, bounded buffering/failure behavior, best-effort usability after power loss, and lossy source-audio versus recognition-PCM differences in `docs/usage.md` and relevant package comments; explain that buffered/not-yet-durable data and an incomplete trailing frame may be lost, and verify the documentation does not promise power-loss durability, loss limited to the recorder queue, or lossless or sample-exact evidence.

## 4. Shared encoder and session lifecycle

- [ ] 4.1 Refactor `commands.go`/`run.go` encoder demand to be HTTP streaming OR effective recording, attach `audio.mp3` before capture starts, and gate `web.Config.Audio` strictly on HTTP streaming; verify the streaming/recording/no-transcript option matrix, missing encoder fallback, and PCM-only arguments when neither MP3 output is needed.
- [ ] 4.2 Give each encoded pipe one drain owner in `ffmpeg.go`, `file.go`, and `device.go`, join it at EOF/process end, and retain the recorder across capture restarts; verify fake-process/reader tests for complete tails, append-not-truncate restarts, no competing auxiliary reader, and prompt cancellation.
- [ ] 4.3 Reorder session teardown to stop the source while encoded consumption remains available, then finalize fan-out/recording before audit and terminal summaries; verify normal EOF, cancellation, early setup/listen failure, repeated shutdown, and blocked storage cases without resource leaks or unbounded waits.
- [ ] 4.4 Extend audio source integration tests with generated short media and ffmpeg/ffprobe when available; verify a recording-only replay produces decodable 128 kbps MP3 from beginning to tail, normal streaming quality is unchanged, and concatenated restart output remains decodable without launching the application binary. Verify encoded bytes are written incrementally without requiring shutdown-only metadata and that an abruptly truncated recording, including a partial trailing frame, retains decodable complete-frame audio without clean finalization; this tests format resilience, not filesystem/hardware power-loss durability.
- [ ] 4.5 Extend CLI/session tests to verify text and audit compatibility, source recording through recognition filtering/pauses, logged recording faults with continuing captions, and final summary ordering; verify the existing audit secret-exclusion and lifecycle tests still pass.
- [ ] 4.6 Add HTTP-level tests in `internal/web/server_test.go` or existing in-process session tests proving `/audio.mp3` stays disabled for recording-only sessions and remains available for streaming-only sessions; verify tests start and close servers within the test process only.
- [ ] 4.7 Update any affected internal architecture comments and audio-lifecycle sections of `DESIGN.md`; verify they describe the single encoded reader, independent output demand, and source-before-recorder shutdown ordering implemented by the tests.

## 5. Deployment guidance and release notes

- [ ] 5.1 Update `README.md`, `deploy/default`, and `deploy/README.md` with default audio recording, `LIVECAPTION_AUDIO_RECORDING=false`, the 50 GB budget override, pre-upgrade backup/deletion warning, approximate 57.6 MB/hour audio use, privacy implications, and lack of a runtime hard cap; verify settings agree with CLI tests and no kiosk deployment files are changed.
- [ ] 5.2 Add an application `CHANGELOG.md` entry covering recording, opt-outs, and automatic deletion of older bundles including legacy transcripts; verify the entry explicitly notes cleanup occurs only before persisted sessions and deleted history requires backups to recover.

## 6. Integrated verification

- [ ] 6.1 Run `go build ./...`, `go test ./...`, and `golangci-lint run ./...`; fix findings in touched Go files and record any pre-existing unrelated failures. Do not run the application binary, `go run`, or `just run`.
- [ ] 6.2 Run race-enabled tests for touched audio, caption, CLI, and metrics packages, checking bounded queue retirement, source restart/EOF, concurrent status reads, and shutdown; verify no races or leaked test workers remain.
- [ ] 6.3 Review the complete option matrix and both delta specs against passing tests and documentation, run `openspec validate add-session-audio-recording --strict`, and inspect the diff for unintended changes; report any unverified browser behavior rather than starting a server.
