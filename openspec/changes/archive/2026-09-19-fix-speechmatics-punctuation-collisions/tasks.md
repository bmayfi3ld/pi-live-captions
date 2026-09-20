## 1. Speechmatics punctuation reconciliation

- [x] 1.1 Extend the existing result fixtures and add table-driven regression cases in `internal/stt/speechmatics/speechmatics_test.go` for both removal tags: duplicate commas, comma-to-terminal and terminal-to-comma collisions, two terminal marks, consecutive removals, and lone punctuation retention. Verify new collision cases fail against the old assembly behavior with `go test ./internal/stt/speechmatics`.
- [x] 1.2 Implement removal-aware, separate-result punctuation reconciliation in `internal/stt/speechmatics/speechmatics.go`, preserving word and segment timing and resetting local state at surviving-word/run boundaries. Update adjacent comments, including the stale tag comment claiming only profanity is acted on. Verify collision tests and existing provider tests pass with `go test ./internal/stt/speechmatics`.
- [x] 1.3 Cover preservation and state boundaries: embedded `a.m.` plus comma with/without removal, ellipses and `?!` not separated by removal, leading/all-filtered input, subsequent kept words, speaker changes, unchanged word timing, punctuation-derived segment ends, and independent messages/flat-text fallback. Verify these cases pass with `go test ./internal/stt/speechmatics`.

## 2. Integration and release verification

- [x] 2.1 Add a focused check using the existing hub test infrastructure or an adapter-to-hub test that a corrected terminal transcript produces matching live word text and finalized line text without losing sentence closure. Verify with `go test ./internal/stt/speechmatics ./internal/caption`.
- [x] 2.2 Add an application `CHANGELOG.md` entry describing the Speechmatics removal-related punctuation fix; verify it does not claim global grammar cleanup or historical transcript repair and leaves kiosk/deployment files unchanged.
- [x] 2.3 Run `go build ./...`, `go test ./...`, and `golangci-lint run ./...`; fix findings in touched Go files and record any unrelated pre-existing findings. Do not start the application or modify the live box. Report browser behavior as requiring the user's manual check.
