## Context

See `proposal.md` for motivation and scope. `internal/web/static/admin.html` places `#lt-card` after `#grid`; the similarly named `#card-transcript` inside the grid shows file-writing statistics and is not the target. The live card mounts one `CaptionStack` with three rows, one `/events` stream, and a `ResizeObserver` that schedules retypesetting. Connection updates toggle the card's `dim` class.

The page uses inline CSS and plain JavaScript, without a UI framework or a phone breakpoint. Existing grids use minimum track widths of 21rem and 17rem, which can overflow a narrow phone once body padding is included. `internal/web/admin_viewer_test.js` already runs real page-script snippets in Node's `vm` with small DOM stubs. Current main specs cover input availability and buffer health; neither specifies transcript placement, and no requirement conflict was found.

Design is included to settle docking, space reservation, responsive defaults, and caption reflow before implementation.

## Goals / Non-Goals

**Goals:** Keep one caption renderer and connection; use native controls and CSS positioning; make docking reversible without altering event handling or the diagnostic dashboard's data flow.

**Non-Goals:** No new module framework, layout library, backend setting, storage API, user-agent device detection, or changes to shared `caption.js`. No general dashboard redesign beyond small overflow corrections needed for phone use.

## Decisions

### 1. Reposition the existing card with fixed positioning

Add a labeled native checkbox in a wrapping card-header container alongside the existing heading and status. Keep the label outside the heading, preserve keyboard focus styling, and provide a comfortable touch target. Toggle a dedicated class without replacing other classes such as `dim`. Pinned CSS sets viewport-bottom positioning, horizontal safe-area-aware insets, an opaque themed surface, and an appropriate stacking order. Unpinned styling retains the existing inline location.

Use `position: fixed`, not `sticky`: this card is at the end of the document and must be visible even while the operator is at the top. Do not clone or remount it; duplicate views would require stream/render synchronization and risk duplicate events.

### 2. Choose the initial mode once per page load

Initialize the checkbox and pinned class using `matchMedia('(max-width: 600px)')` before constructing the caption stack. This is a documented viewport proxy for phones, not hardware detection. Subsequent checkbox changes update local state only. Do not subscribe to breakpoint changes: rotation and resizing must not undo the current mode. Reload naturally recalculates the default.

Persistent preferences and server settings are unnecessary for this request. Without JavaScript the existing inline layout remains the fallback; the toggle should only become available once initialized.

### 3. Reserve actual dock height, not a guessed constant

While pinned, add bottom padding to the document equal to the card's measured border-box height plus the existing bottom gap. Store the measured height in a CSS custom property. Update it immediately after toggling and from the existing card resize observer, alongside its existing debounced retypeset scheduling. Clear the extra reservation when unpinned. A window-resize fallback can refresh geometry and schedule retypesetting when `ResizeObserver` is unavailable.

The card includes bottom safe-area padding, so its measured height includes that inset exactly once. Keep the offscreen text probe out of its scrollable overflow if needed. Make the pinned card viewport-height-bounded (maximum half the dynamic viewport height, with a `vh` fallback) and internally scrollable for short screens or enlarged text. Keep the toggle at the top of the bounded card. Normal heights retain the existing three-row display; constrained heights allow scrolling instead of consuming the entire console.

Hard-coded document padding would fail when the header wraps, fonts change, or safe-area padding grows. Updating document padding must not drive the observed card's height, avoiding a resize loop.

### 4. Make only necessary narrow-width corrections

Allow the transcript header to wrap. Clamp the grid minimum tracks to the available width (for example, `min(100%, 21rem)` and `min(100%, 17rem)`), and let long diagnostic values wrap where needed rather than hiding page overflow. Preserve existing desktop column behavior and console content. Apply safe-area support in the viewport metadata and dock CSS as appropriate.

Reuse the current `CaptionStack.retypeset()` scheduling when the card width changes; retain the same DOM nodes, history, stream handlers, speaker styling, and status behavior. Explicitly schedule retypesetting after a toggle as well, so correctness is not dependent on observer availability.

### 5. Extend existing checks, then hand off browser verification

Extend `internal/web/admin_viewer_test.js` to execute the actual initialization and toggle logic with DOM/media-query stubs. Check defaults at 390/600/601 pixels, repeated toggles, page-lifetime mode retention and reload defaults, matching accessible control state, dock-space updates and cleanup, and no new caption stack or stream on a mode change. Keep existing event-handler checks passing; do not add a browser framework.

Use user-performed browser checks for actual geometry, focus, scrolling, safe areas, rotation, light/dark themes, and live-caption continuity. Node stubs cannot validate CSS layout. Never launch the application to perform these checks from the agent environment.

## Risks / Trade-offs

- Phone classification by width misses wide landscape first loads → document the initial-width rule and make pinning available everywhere.
- Dock obscures focused or final console controls → reserve measured space, account for keyboard focus scroll clearance, and manually check last-card access and tab navigation.
- Long status labels, zoom, or a short viewport increases dock height → wrap the header and cap dock height with overflow access; test landscape and enlarged text.
- Card dimension changes can briefly precede caption reflow → reuse the existing 120ms debounced retypeset, preserving history rather than resetting it.
- Script checks cannot prove mobile browser layout → leave explicit browser checks for the user against their running instance.

## Migration Plan

No data or configuration migration. Implementation changes the embedded admin asset, its existing JavaScript checks, and the application changelog. Build and deploy through the normal application release process. Rolling back those changes restores the original inline transcript; no persisted preference needs cleanup.
