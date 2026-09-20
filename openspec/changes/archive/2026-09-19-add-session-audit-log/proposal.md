## Why

The human-readable transcript does not retain enough evidence to diagnose recognition accuracy after an event, and session warnings currently live only in stderr or the host journal. A durable, machine-readable record alongside each transcript would let operators and AI analysis correlate caption output with audio timing, noise-gate decisions, reconnects, drops, and other session faults.

## What Changes

- Add a versioned `audit.jsonl` companion in each enabled transcript session directory while preserving `transcript.txt` unchanged.
- Record finalized captions and non-speech markers with source-relative millisecond timing, wall-clock observation time, speaker, and record type.
- Record noise-gate open and close transitions, including the source-media position at which each transition applies.
- Record warnings, errors, relevant state transitions, reconnects, pauses, audio/buffer drops, and source restarts in the same session timeline.
- Record non-secret session metadata at startup and a final metrics summary at clean shutdown, including recognition and noise-gate configuration needed to interpret the run.
- Keep audit recording best-effort so a storage failure degrades observability without interrupting live captions.

## Capabilities

### New Capabilities
- `session-audit-log`: Durable, structured per-session evidence for post-event caption accuracy analysis and fault correlation.

### Modified Capabilities

None. The existing `transcript-timestamps` contract and human-readable transcript format remain unchanged.

## Impact

- Affects session wiring, caption persistence, noise-gate/STT/source event reporting, metrics snapshots, transcript tests, and operator documentation.
- Adds one append-only JSON Lines file to each session directory when transcript recording is enabled; no new dependency or required CLI option.
- Packaged journald logging remains available and unchanged, but important session diagnostics become portable with the transcript.
