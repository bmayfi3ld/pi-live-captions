## 1. Pin Transcript Compaction

- [x] 1.1 Add focused writer coverage for adjacent duplicate music and silence markers, first-timestamp retention, marker reset by speech or a different marker, preserved identical speech, and line/byte metrics that exclude omitted markers; verify the new assertions fail before implementation with `go test ./internal/caption`.
- [x] 1.2 Add the minimum marker-state handling to `internal/caption/writer.go` under the existing writer lock, recognizing only the established music and silence marker strings; verify `go test ./internal/caption` passes without changing hub or web event code.

## 2. Document and Verify

- [x] 2.1 Add an Unreleased `CHANGELOG.md` entry describing adjacent non-speech marker compaction in newly recorded transcripts; verify it does not claim historical transcript rewriting or live-viewer changes and leaves kiosk/deployment changelogs untouched.
- [x] 2.2 Run `go test ./...`, `go build ./...`, and `golangci-lint run ./...`; fix findings in touched code without starting the application or a built binary.
