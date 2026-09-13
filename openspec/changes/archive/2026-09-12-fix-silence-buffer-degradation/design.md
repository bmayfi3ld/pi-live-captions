## Context

See `proposal.md` for motivation and `specs/stt-buffer-health/spec.md` for the behavior contract.

`internal/stt/session.go` has one drain goroutine that calls `gate.Observe(f)` and then `buf.push(f)`. The gate is driven by incoming frame media time. The ring retains approximately two seconds of PCM while paused; `waitResume` waits for the gate to become active before redialing. Today `ring.push` consults `gate.Active()` at eviction time, so returning speech causes old paused-period chunks to count as drops.

`Metrics.STTBufferDrop` increments a cumulative counter and renews the 60-second degradation window. Admin polls the server's health verdict; neither that resolver nor the page needs changing for this accounting fix. The existing provider resume test asserts zero reconnects but not zero buffer drops, and does not deliberately fill the paused buffer.

This is a source-traced defect, not a reproduced explanation of the user's browser-refresh symptom. A design is useful here to distinguish gate-period classification from sample-level silence detection and to settle both transition directions.

## Goals / Non-Goals

**Goals:**
- Put the correction in the shared ring, not individual provider drivers or admin rendering.
- Associate eviction accounting with the discarded chunk, consistently across both pause and resume.
- Verify the behavior without a browser, real provider, wall-clock sleeps, or application startup.

**Non-Goals:**
- Detect speech per chunk, retune gates, or exempt all acoustically silent input.
- Flush or enlarge the buffer, introduce a resume grace period, or reset cumulative metrics on recovery.
- Change health precedence, its 60-second event window, or persistent source/transcript fault handling.
- Diagnose the separately reported badge that clears on browser refresh after a possible server restart.

## Decisions

### Store admission-time gate activity on each chunk

Add one boolean to the existing private `chunk` structure. Populate it in `ring.push` from the STT auto-pause gate after the drain has observed that frame. Use the evicted chunk's boolean to decide whether to call `STTBufferDrop`; do not consult the current gate for that decision. The single producer observes and pushes sequentially, so the first resuming frame is active-period audio while older paused-period chunks retain their classification.

Keep existing locking, PCM bytes, capture timestamps, byte accounting, capacity, oldest-first eviction, and notification behavior. There is no need to change the public frame type or driver APIs.

Alternatives rejected:
- Suppressing all drops while reconnecting or for a fixed grace period hides genuine active-period loss on slow redials.
- Clearing metrics or shortening the health window treats the symptom and can erase unrelated faults.
- Recomputing RMS on eviction duplicates the gate's policy and mishandles release/hold periods and disabled auto-pause.
- Flushing paused PCM changes pre-roll and may clip the onset of returning speech.

### Preserve conservative active-period accounting

"Active" means the STT gate's decision, not proof of spoken words. Quiet frames during the gate's hold interval still count as active-period audio. Auto-pause disabled means the gate remains active, preserving existing drop behavior. An active-period chunk evicted after the gate closes still counts as loss; the existing paused-health precedence may mask its badge until resume, but the loss remains recorded.

### Use deterministic ring regression coverage

Extend `internal/stt/ring_test.go` with a small buffer, distinguishable PCM/timestamps, real metrics, and gate transitions driven by synthetic media offsets. Observe each frame before pushing, matching the production drain ordering.

Cover paused rotation, resume evicting only paused chunks, subsequent active-period overflow, active-to-paused eviction, and disabled auto-pause. Assert exact drop counts and health snapshots, including no false degradation after a clean resume and no clearing of unrelated degradation. Pop retained chunks to verify payload order and capture timestamps. Reuse existing test infrastructure; a provider/network test is unnecessary for this deterministic accounting rule.

## Risks / Trade-offs

- Gate activity is not sample-level speech classification -> deliberately preserve the existing gate policy rather than add another detector.
- Incorrect transition ordering could classify the first returning frame as paused -> mirror `Observe` then `push` in regression tests and keep that production ordering unchanged.
- Exempting every eviction during resume could hide real loss -> test the transition from evicting paused chunks to evicting active chunks in the same bounded buffer.
- Fix may be mistaken for resolution of the stale-page report -> changelog and review summary must identify only the false degradation caused by paused-buffer eviction.

## Migration Plan

No data migration, API change, configuration change, or kiosk package update is needed. Build and test normally, then deploy through the existing application process. Rollback is the prior application version; this only restores the previous accounting behavior. Implementation includes an application `CHANGELOG.md` entry and build, test, race-test, and lint checks without starting the application.
