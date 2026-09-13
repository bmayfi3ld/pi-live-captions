## 1. Add reversible transcript docking

- [x] 1.1 In `internal/web/static/admin.html`, add an accessible native “Pin transcript” checkbox alongside the Live Transcript heading/status; initialize it once from `(max-width: 600px)` before mounting captions and toggle a dedicated class on the existing card without persistent storage or authentication gating. Verify 390/600/601-pixel defaults and matching checkbox/class state using actual page-script checks in `internal/web/admin_viewer_test.js`.
- [x] 1.2 Add fixed-bottom, themed, safe-area-aware docking CSS, a wrapping header, a half-viewport height cap with overflow access, and measured document bottom clearance. Reuse the existing resize observer for clearance and caption reflow, with immediate updates on toggle and a resize fallback. Verify measured-height changes, removal of extra clearance on unpin, preservation of unrelated card classes, and retypeset scheduling in the Node checks.
- [x] 1.3 Clamp the existing diagnostic grid minimum widths to the available space and allow overflowing diagnostic values to wrap without hiding content. Verify the CSS retains existing desktop track sizes and include 320-pixel layout and last-card reachability in the browser handoff below.

## 2. Regression checks and release note

- [x] 2.1 Extend `internal/web/admin_viewer_test.js` with repeated-toggle, resize-without-mode-reset, fresh-load-default, disabled-server-controls, and single-caption-stack/stream continuity checks against actual page code. Verify `node internal/web/admin_viewer_test.js`, `node internal/web/caption_pace_test.js`, and `node internal/web/caption_decay_test.js` pass without a server.
- [x] 2.2 Add an Unreleased entry in `CHANGELOG.md` describing phone-default transcript pinning and the inline toggle; verify the entry matches the implemented initial-width and page-local behavior without a release-version change.
- [x] 2.3 Run `go build ./...` and `go test ./...` to verify embedded assets and existing server behavior still pass; if any Go file was changed, also run `golangci-lint run ./...` and fix findings in touched code. Do not run the application binary or start a server outside tests.

## 3. Browser verification handoff

- [x] 3.1 Deliver a manual checklist for the user to run against their existing instance: initial defaults at 320/390/600/601 pixels and desktop width; pin/unpin and reload; portrait-to-landscape rotation; safe areas and browser chrome; enlarged text; light/dark themes; keyboard focus and touch toggle use; last diagnostic card and noise-gate controls reachable above the dock; live captions, speaker badges, music/silence markers, and disconnect/reconnect indication preserved. Verify the handoff explicitly distinguishes passed automated checks from browser behavior not verified by the agent; do not claim a browser pass without user confirmation.
