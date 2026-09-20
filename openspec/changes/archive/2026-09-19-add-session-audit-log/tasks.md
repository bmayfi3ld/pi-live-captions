## 1. Audit Persistence

- [x] 1.1 Extend the caption writer to create, encode, periodically flush, and close an independent append-only `audit.jsonl` stream beside `transcript.txt`; verify writer tests parse every complete line and confirm transcript output is byte-for-byte unchanged.
- [x] 1.2 Define the versioned audit envelope and concrete startup, caption, marker, gate, configuration, operational, and shutdown records with explicit secret-safe metadata; verify JSON tests cover required fields, optional timing, and credential exclusion.
- [x] 1.3 Keep audit errors independent from transcript errors, disable the failed audit sink after its first failure, and report once through the normal logger without recursion; verify an injected audit write failure leaves transcript writes and caption flow working.

## 2. Caption and Gate Evidence

- [x] 2.1 Carry reliable finalized-line end timing through caption assembly and write caption/marker audit records from the existing final callbacks; verify hub and writer tests cover millisecond start/end values, unknown end timing, speakers, markers, and continuity across reconnect-style clock ranges.
- [x] 2.2 Expose effective noise-gate open/close edges with their source-media positions and active threshold/release settings, then route them to the audit sink; verify noise-gate tests produce exactly one record per state change and no continuous RMS records.
- [x] 2.3 Record runtime noise-gate setting changes before subsequent edges use them; verify the existing noise-gate HTTP test path observes ordered configuration and transition records.

## 3. Session Diagnostics and Lifecycle

- [x] 3.1 Compose the session logger with an audit handler for warning/error records while retaining existing terminal and JSON stderr behavior; verify logger tests cover attributes, severity filtering, UTC and elapsed timing, and non-recursive sink failure.
- [x] 3.2 Emit structured audit events for source/STT state changes, reconnects, pauses, drops, and restarts at the existing metrics/event points, including source time only when known; verify focused tests cover each event family without reconstructing state from log text.
- [x] 3.3 Write secret-safe `session_start` metadata after session construction and a final metrics-backed `session_end` after producers stop but before writer close; verify session tests assert ordering, expected counters, and absence of a promised final record on simulated abrupt truncation.

## 4. Documentation and Verification

- [x] 4.1 Document `audit.jsonl`, its clock semantics, record categories, relationship to `transcript.txt`, and the continued availability of journald in `docs/usage.md` and deployment guidance; verify documentation examples are valid JSON Lines.
- [x] 4.2 Add a primary application `CHANGELOG.md` entry describing the new per-session audit artifact and confirm no kiosk changelog is needed because kiosk deployment behavior is unchanged.
- [x] 4.3 Run `gofmt` on changed Go files, `go test ./...`, `go build ./...`, and `golangci-lint run ./...`; fix findings in touched code and record any unrelated pre-existing failures.
