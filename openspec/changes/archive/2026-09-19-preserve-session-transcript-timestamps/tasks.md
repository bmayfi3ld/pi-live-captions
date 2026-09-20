## 1. Preserve and map source timing

- [x] 1.1 Carry each `audio.Frame` source end offset through the STT ring chunk, correct the frame offset documentation, and verify ring tests preserve PCM, capture time, gate state, and source offset.
- [x] 1.2 Extend the per-connection anchor index to resolve provider media positions to source offsets alongside capture/send times, including chunk boundaries, pre-roll, clamping, eviction, and discontinuous source ranges; verify with focused `internal/stt/anchor_test.go` cases.

## 2. Normalize shared STT output

- [x] 2.1 Normalize every timed word onto source time in the shared read path, re-derive transcript bounds, handle untimed transcripts without dropping text, and preserve latency anchoring from the original provider end; verify focused shared-session tests cover reconnects, pre-roll, and a source gap within one result.
- [x] 2.2 Return typed Speechmatics music edges from decoding to the shared read path and normalize them through the same anchor before callback delivery; verify Speechmatics tests cover start/end decoding and ordered source-relative callback values while Deepgram behavior remains unchanged.
- [x] 2.3 Replace timestamp-encoded music reset behavior with an explicit connection-lifecycle reset after graceful drain, preserving held-word filtering and avoiding synthetic transcript markers; verify caption/STT tests cover pause, reconnect, music end, and first returning speech.

## 3. Verify transcript behavior

- [x] 3.1 Add an integration-level test that drives at least two recognizer connections in one session and verifies saved speech, silence, and music lines retain the existing text format with nondecreasing source-relative timestamps rather than restarting at zero.
- [x] 3.2 Update transcript documentation to define timestamps as source-session positions and add an application `CHANGELOG.md` entry; verify neither claims historical transcript rewriting, new configuration, or changed caption text.

## 4. Validate the change

- [x] 4.1 Run `go test ./...`, `go build ./...`, and `golangci-lint run ./...`; fix findings in touched code and confirm no application binary was started.
