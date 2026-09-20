## Context

`internal/caption.Writer` currently owns the session directory and periodically flushes only `transcript.txt`. Final lines reach it after `caption.Hub` has merged settled STT segments, while lower-level timing, noise-gate state, application diagnostics, and the final metrics snapshot follow separate paths. Diagnostics use `slog`: terminal runs render them for people, non-TTY runs emit JSON to stderr, and the packaged service relies on journald.

The audit stream must be portable with the transcript, preserve source-media time separately from observation time, and remain subordinate to the live-caption path. See `proposal.md` for motivation and `specs/session-audit-log/spec.md` for observable behavior.

## Goals / Non-Goals

**Goals:**
- Produce one ordered, append-only JSON Lines stream using the existing session lifecycle and transcript directory.
- Preserve enough context to distinguish likely recognition mistakes from gate closures, missing audio, reconnects, or other degradation.
- Reuse existing metrics, clocks, logging, and periodic flushing rather than introduce a second telemetry subsystem.
- Keep records stable and evolvable through an explicit schema version.

**Non-Goals:**
- Persist raw provider messages, interim hypotheses, audio, or a duplicate word-by-word provider trace.
- Replace stderr, journald, the admin dashboard, or `transcript.txt`.
- Guarantee a final summary after process termination or power loss.
- Add remote upload, database storage, compression, indexing, or retention policy.

## Decisions

### Store audit records in the existing caption writer

Extend the session writer to open and serialize `audit.jsonl` beside `transcript.txt`, guarded by the writer's existing mutex and flushed on the same cadence. This gives both artifacts one lifecycle, ordering point, error policy, and shutdown path.

Alternative: create a separate audit service. Rejected because there is one local append-only sink and no independent lifecycle that justifies another abstraction or goroutine.

### Use a small versioned event envelope with typed payload fields

Every record will carry `schema_version`, `type`, and `observed_at`; records after startup will also carry `elapsed_ms`, with `source_ms` only where the source clock is known. Type-specific fields remain ordinary JSON fields rather than a deeply nested generic attribute model. Integer milliseconds avoid floating-point ambiguity and match the existing wire and hub units.

The initial record types are `session_start`, `caption`, `marker`, `noise_gate`, `config`, `state`, `warning`, `error`, `drop`, and `session_end`. The implementation should share one encoder path while using concrete record construction at call sites.

Alternative: enrich the text transcript. Rejected because it would break its existing compatibility contract and remain awkward for machine ingestion. A single closing JSON document was also rejected because a crash would lose or invalidate the whole artifact.

### Audit final user-visible lines, not raw recognition traffic

Caption records correspond to lines delivered to the transcript callback after hub assembly and filtering. The hub will retain enough end timing when closing a line to provide `end_ms` when reliable. This keeps the audit aligned with what users actually saw while noise-gate and operational records explain surrounding conditions.

Alternative: retain provider payloads and interim or settled segments before assembly. Rejected for the first version because it increases volume, couples the format to providers, and may retain text intentionally filtered from output. It can become a separately specified diagnostic mode if final-output evidence proves insufficient.

### Emit noise-gate edges at the point the gate decision becomes effective

The noise-gate path will expose only state changes, not continuous RMS samples. Each edge carries the source frame position, observation time, and a snapshot of the active threshold and release settings. Runtime setting changes produce a `config` record before later edges use them.

Alternative: infer gate activity from STT pause state or periodic metrics. Rejected because release timing and buffering make inference imprecise, and accuracy analysis specifically needs the boundary where recognition eligibility changed.

### Tee warning and error logs into the session audit sink

Compose the active `slog` handler with a session audit handler after the writer is created. The existing terminal/JSON handler remains authoritative for operator output; the audit handler accepts warning and error records only and writes them through the same serialized audit sink. Session construction must ensure the composed logger is both passed to components and installed as the default before run goroutines start.

State changes and counted degradation events are emitted explicitly at their source so they retain structured component names, counts, and source positions where available. Log capture alone is not used to reconstruct state.

The audit handler must disable itself after its first persistence failure and report that failure only through the original non-audit handler, preventing recursive writes.

Alternative: scrape journald after the event. Rejected because journal retention and machine scope do not reliably map diagnostics into the portable session directory.

### Write startup metadata and reuse the final metrics snapshot

After session wiring is complete, write `session_start` from the resolved, non-secret runtime configuration rather than environment variables or raw CLI structures. On clean shutdown, snapshot metrics after producers stop and append `session_end` before closing the writer. Sensitive fields are excluded by construction through an explicit audit metadata type.

### Keep audit failure independent from transcript failure

Track audit errors separately from transcript errors so a failed `audit.jsonl` does not disable `transcript.txt`. After an audit persistence failure, stop further audit writes, surface one warning through the normal logger, and keep captions running. Transcript behavior remains unchanged.

## Risks / Trade-offs

- [A warning occurs before the audit sink is attached] -> Startup failures remain in stderr/journald; the audit contract starts when the session directory is created and its first record identifies that boundary.
- [A process crash leaves a partial final JSON line] -> Readers consume complete lines independently and ignore an incomplete tail; periodic flushing bounds buffered loss consistently with the current transcript policy.
- [Logging while holding the writer lock causes recursion or deadlock] -> Audit write failures are returned to a non-audit reporting path only after releasing the writer lock, and the audit handler never logs through itself.
- [Additional writes affect the real-time path] -> Records are small, buffered, and serialized with existing final-line writes; no per-frame RMS logging is added.
- [Millisecond conversion truncates sub-millisecond positions] -> This matches existing public timing precision and is sufficient for transcript review; duration values are converted consistently.
- [The current flush policy is not power-loss durable] -> This change follows existing writer semantics. Filesystem synchronization and retention remain the separate transcript-hardening concern already documented in the repository.

## Migration Plan

No data migration is required. New sessions gain `audit.jsonl`; historical transcript directories remain untouched. Rollback removes creation of the companion file while leaving existing audit files harmless and independently readable.
