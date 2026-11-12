## Why

After a phone's screen is manually switched off, returning to the captions page can replay queued captions instead of showing live speech. A live caption display must discard missed content rather than make the reader wait for catch-up.

## What Changes

- Suspend caption streaming when the page becomes hidden, closing the connection and cancelling reconnect work.
- Clear queued captions and already-displayed text so stale content cannot reappear on return or resize.
- Open a fresh live-only stream when visible again, regardless of the previous connection state; reject hidden-page and obsolete-stream callbacks.
- Apply the same behavior to the public captions page and the admin caption mirror.
- Preserve normal foreground pacing, status/music synchronization, and existing wake-lock behavior.

## Capabilities

### New Capabilities

- `caption-visibility-resume`: Live-only caption recovery after a page is hidden, including screen-off and tab switching.

### Modified Capabilities

None.

## Impact

- Browser lifecycle handlers in `internal/web/static/index.html` and `internal/web/static/admin.html`.
- Reuse the existing `CaptionStack.clear()` API in `internal/web/static/caption.js` to discard queued and displayed content.
- Browser-script regression coverage in `internal/web/` and a primary application changelog entry during implementation.
- No new dependencies, server protocol changes, transcript changes, or kiosk deployment changes. The existing SSE subscription already sends status/music snapshots followed only by future captions.
