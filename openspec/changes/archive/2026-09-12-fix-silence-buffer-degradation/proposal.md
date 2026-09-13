## Why

Normal STT auto-pause recovery can falsely report dropped audio and mark the admin health badge Degraded: the reconnect ring classifies an evicted chunk using the gate's current state, so speech resumption turns discarded paused-period silence into an apparent live-audio loss. Correcting that accounting keeps normal pre-roll rotation from generating fault warnings while retaining warnings for genuine active-period buffer loss.

## What Changes

- Classify buffered chunks by whether the STT auto-pause gate was active when each chunk entered the ring; use the discarded chunk's classification when counting evictions.
- Exclude paused-period chunk evictions from buffer-drop totals and degradation, including during resume/redial.
- Preserve drop reporting for active-period chunks, including when they are evicted after the gate becomes inactive or when auto-pause is disabled.
- Preserve buffer capacity, ordering, pre-roll, resume timing, and existing health precedence and degradation window.
- Add deterministic regression coverage and an application changelog entry during implementation.
- Exclude the separate report of a badge clearing after browser refresh/server restart; this change does not claim to diagnose or resolve that behavior.

## Capabilities

### New Capabilities

- `stt-buffer-health`: Eviction accounting distinguishes paused-period pre-roll from active-period audio without suppressing real buffer-loss warnings.

### Modified Capabilities

None. `live-input-availability` remains unchanged; source faults continue to take precedence over intentional STT pauses.

## Impact

- Primary implementation: `internal/stt/ring.go`, shared by provider engines through `internal/stt/session.go`.
- Regression coverage: `internal/stt/ring_test.go`, using existing gate and metrics APIs. Existing provider resume tests cover reconnect accounting but not paused-buffer evictions.
- Operator-visible effect: fewer false increments of `stt.buffer_drops_total` and false `health: degraded` verdicts in `/api/stats` and `/admin`.
- No API shape changes, new dependencies, configuration, browser changes, or deployment/kiosk changes.
