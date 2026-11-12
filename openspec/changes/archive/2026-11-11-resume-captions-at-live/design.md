## Context

See proposal.md for motivation. Both `index.html` and the admin caption mirror keep an EventSource alive while hidden and reconnect on visibility restoration only when it is CLOSED. The shared `caption.js` drains pending words through timers, which browsers can throttle or suspend. Either queued words or buffered stream events can therefore become catch-up content. The exact phone event ordering has not been reproduced here.

`Hub.Subscribe()` sends status/music snapshots, not caption history. `CaptionStack.clear()` already empties pending words, cancels the drain timer, clears the layout ledger, and rebuilds empty rows. This makes a fresh subscription plus a display reset sufficient without a server change.

This design records lifecycle and callback-ordering decisions across both browser entry points.

## Goals / Non-Goals

**Goals:** Make visibility a boundary between live viewing sessions; prevent old stream callbacks or retry timers from crossing that boundary; reuse existing reset and subscription behavior.

**Non-Goals:** General foreground network-lag detection, age-based caption filtering, replay/history APIs, changes to pacing or transcription, admin polling changes, and wake-lock redesign.

## Decisions

### Suspend on hidden, start fresh on visible

In both pages, make connection startup visibility-aware, including initial loading in a hidden tab. On hiding, invalidate and close the current source, cancel pending reconnect timers, and clear the caption stack. On becoming visible, reset stale display/connection state and establish one fresh source instead of inspecting the old source's readyState. Continue invoking the viewer's existing wake-lock synchronization independently.

Use the existing connecting indicator state while opening the replacement connection; the fresh status/music snapshots restore current upstream state. Do not synthesize silence or music markers from subscription snapshots.

Keeping the old connection and merely accelerating its queue was rejected: it retains missed content and cannot discard transport-buffered events. Clearing only the display was rejected because a surviving stream can refill it with stale events.

### Guard all asynchronous stream entry points

Accept open, error, and message callbacks only for the currently owned source while the document is visible. Ensure retry callbacks and connection startup also check visibility. Invalidate source ownership before closing it. This prevents a delayed callback from an obsolete source from painting text, mutating indicators, or scheduling another connection after resume. Keep no more than one owned source and one retry timer.

Use small local lifecycle changes in the two existing page scripts; do not introduce a new streaming framework or dependency solely for this fix.

### Reuse the full display reset

Call `CaptionStack.clear()` rather than adding a queue-only reset. The user approved a blank display until the next live segment. Clearing the ledger also prevents resize/orientation reflow from resurrecting old text. Do not alter foreground word pacing or the server's live-only subscription contract.

### Test the lifecycle with existing lightweight JavaScript patterns

Use Node assertions and mocked document visibility, EventSource, timers, and caption stack to exercise both actual page scripts. Cover callbacks from a replaced source explicitly, rather than relying on a mock close method to suppress them. Add or extend a typesetter check proving that clear cancels queued output and old text stays absent after reflow. Existing HTTP tests cover subscription snapshots; no running app is needed for automated checks.

## Risks / Trade-offs

- Brief blank display on return, including during silence -> intentional; current status/music indicators explain the live state while waiting for future captions.
- Mobile browsers differ in lifecycle delivery and suspension -> manually verify screen-off/unlock and tab switching on the user's phone; mocked tests verify application logic but cannot establish browser behavior.
- Two page scripts can drift -> run the same lifecycle cases against both entry points.
- A reconnect racing with stale callbacks can corrupt the new session -> validate ownership and visibility before any callback side effect.
- Background caption previews no longer collect text -> intentional live-only semantics; saved transcripts and unrelated admin functionality remain unchanged.

## Migration Plan

Ship updated embedded assets with the normal application release and changelog entry. Existing browser pages need a reload to load the fix. No configuration or data migration is required. Rollback restores the previous assets; stored transcripts are unaffected.
