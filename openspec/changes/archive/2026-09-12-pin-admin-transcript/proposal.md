## Why

The admin Live Transcript card sits below the console's diagnostics, so operators lose sight of captions while scrolling, especially on phones. Keeping it docked to the viewport bottom lets operators monitor captions and inspect the console at the same time.

## What Changes

- Add an accessible “Pin transcript” toggle to the existing Live Transcript card.
- When pinned, keep that same card visible at the viewport bottom; when unpinned, retain its current inline location below the diagnostic grid.
- Start pinned on phone-sized viewports and inline on larger screens. Use an initial viewport width of 600 CSS pixels or less as the phone proxy; keep manual choices for the current page lifetime, without persistent storage.
- Keep console content reachable, accommodate mobile safe areas and narrow widths, and preserve the existing three-row live caption display and connection indicator.

## Capabilities

### New Capabilities

- `admin-transcript-pinning`: Local display toggle, phone default, bottom docking, and continuity of the existing live transcript.

### Modified Capabilities

None. Existing input-availability and buffer-health requirements remain unchanged.

## Impact

- Primarily `internal/web/static/admin.html`: layout, toggle markup, and page-local state.
- Extend the existing dependency-free checks in `internal/web/admin_viewer_test.js`; update `CHANGELOG.md` under Unreleased when implemented.
- Reuse `CaptionStack` and the existing `/events` connection and resize observer; no API, authentication, backend configuration, viewer, kiosk, or dependency changes.

## Non-goals

Draggable or resizable panels, transcript history, persistent preferences, a general mobile dashboard redesign, and changes to caption rendering or delivery.
