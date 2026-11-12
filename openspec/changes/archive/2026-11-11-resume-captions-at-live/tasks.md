## 1. Visibility-scoped caption streaming

- [x] 1.1 Update `internal/web/static/index.html` to close/invalidate the caption stream, cancel retries, and reuse the full caption reset while hidden; reconnect fresh when visible and guard asynchronous callbacks by visibility and source ownership. Verify with a Node regression check covering queued captions, initially hidden load, delayed obsolete callbacks, and repeated hide/show cycles.
- [x] 1.2 Apply the same lifecycle behavior to the caption mirror in `internal/web/static/admin.html`, without changing unrelated admin polling. Verify the same lifecycle cases against the admin script and confirm current status/music snapshots do not create historical markers.

## 2. Regression coverage and release documentation

- [x] 2.1 Add or extend a runnable typesetter regression check proving that clear cancels pending word output and resize/reflow cannot resurrect discarded text. Verify it passes with Node alongside the existing pacing and decay checks.
- [x] 2.2 Add a primary application changelog entry describing live-only return after screen-off/backgrounding for both caption displays. Verify the entry states that old displayed text is cleared and no history is replayed.
- [x] 2.3 Run all `internal/web/*test.js` checks with Node, `go build ./...`, and `go test ./...`; record results. If any Go files were edited, also run `golangci-lint run ./...` and resolve findings in touched code. Do not start the application binary.

Verification: all four `internal/web/*test.js` scripts, `go build ./...`, `go test ./...`, `git diff --check`, and strict OpenSpec validation passed. No Go files were edited. The stale four-row default assertion was corrected to the current three-row default with user approval.

## 3. Real-device acceptance

User reported the fix worked and accepted both caption displays as ready. Browser/device details and individual secondary scenarios were not reported; acceptance is user-confirmed, not independently verified. The agent did not start the application.

- [x] 3.1 Have the user verify both caption displays using their running instance: switch the phone screen off during speech, wait, unlock, and confirm a blank reset followed by only new captions; repeat with tab switching, silence/music, and rotation. Confirm the viewer's wake behavior remains unchanged. Record the browser/device and results, or explicitly leave this task pending if real-device verification is unavailable.
