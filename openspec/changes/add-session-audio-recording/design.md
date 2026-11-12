# Design

## Context

See `proposal.md` for motivation and the two delta specs for behavior contracts.

Observed implementation:
- `internal/audio/file.go` and `device.go` already produce two outputs from one ffmpeg decode: 16 kHz mono PCM on stdout and, when a `Broadcaster` exists, 128 kbps MP3 at 44.1 kHz on fd 3. MP3 channels remain those of the source.
- `Broadcaster.Run` drains the encoded pipe and distributes bytes through bounded, lossy HTTP subscription queues. It discards bytes when there are no subscribers; an ordinary subscription is unsuitable for durable recording because it joins at the live edge and silently drops chunks under pressure.
- `internal/cli/commands.go` currently requests MP3 only when HTTP streaming is enabled. `newSession` creates the caption writer before capture starts. `caption.NewWriter` creates timestamp-named directories with `transcript.txt` and `audit.jsonl`; the old `transcript.jsonl` is retired.
- Source shutdown currently calls `proc.Close`, which starts an auxiliary-pipe discard reader. Session shutdown closes the broadcaster before the source. Both require adjustment to avoid racing away the recording tail.
- Existing tests exercise encoded fan-out, file replay through ffmpeg, CLI parsing, and session audit lifecycle. `README.md`, `docs/usage.md`, and `deploy/default` describe persistence and streaming. The package already depends on ffmpeg.

## Goals / Non-Goals

**Goals:**
- Reuse the existing encode, including its quality/channel settings, without adding another capture process or encoder.
- Keep disk work out of the encoded pipe's critical path and make incomplete recordings explicit.
- Make cleanup a single synchronous startup phase, independent of recording being enabled.
- Preserve existing text/audit contracts and HTTP behavior when recording options change.

**Non-Goals:**
- Bit-identical copies of recognition PCM, lossless archival audio, sample-accurate alignment, or a new configurable codec/bitrate surface.
- Concurrent processes writing to one transcript root, historical-file browsing, background cleanup, filesystem free-space reservations, or batch accuracy analysis.

## Decisions

### 1. Independent output flags with shared encoder demand

Add default-on, negatable `--audio-recording` (`LIVECAPTION_AUDIO_RECORDING`) and `--transcript-max-storage` (`LIVECAPTION_TRANSCRIPT_MAX_STORAGE`, default `50GB`) to `OutputFlags` for live and replay. Effective recording is `audio-recording && !no-transcript`.

Request the shared encoded output if HTTP streaming **or** effective recording needs it, checking encoder availability once. Do not expose `/audio.mp3` merely because a recording-only run has a broadcaster: `web.Config.Audio` and the HTTP banner/status remain gated by `AudioStream`. When neither output is wanted, retain the existing PCM-only source arguments. Missing `libmp3lame` disables the affected MP3 outputs visibly, not capture or text persistence.

Use explicit decimal `KB`, `MB`, `GB`, and `TB` suffixes and plain integer bytes for the budget; reject fractions, nonpositive values, and overflow before side effects. The default is 50,000,000,000 bytes, not 50 GiB. Match current flag/environment precedence.

**Alternatives:** Tying recording to `AudioStream` would make HTTP opt-out unexpectedly disable persistence. A separate recording encode, Opus, or FLAC would add CPU or dependencies/compatibility work without leveraging the existing stream.

### 2. A dedicated recording sink at the encoded fan-out

Keep one owner reading fd 3 and add a recording sink to the shared encoded fan-out, before normal HTTP delivery. Attach it after session directory creation and before `Source.Start`; do not implement it as a late HTTP subscriber. Copy each encoded chunk into immutable storage once and share it where practical.

Add a recorder in `internal/audio` owning `audio.mp3`, a worker goroutine, and a fixed-size queue. Use a roughly four-second queue (eight current 8 KiB encoded chunks, 64 KiB total) rather than an unbounded backlog. Only the worker writes/closes the file. Enqueue is nonblocking. On overflow or write error, atomically retire the recording sink once and report failure; never resume accepting bytes after silently creating a hole. Already queued prefix data can drain when storage is healthy. File-create failure disables recording only, like the audit companion's best-effort creation policy.

Expose recording enabled/state/path/bytes/error in the metrics snapshot and terminal banner/summary. Report faults through the session logger, which already feeds the audit stream; guard against repeated reports. A permanent recording failure must remain observable until session end rather than aging out like a transient warning. No admin UI redesign is needed; logging, metrics JSON, and terminal output provide visibility.

**Alternatives:** Synchronous `io.MultiWriter` would backpressure the shared ffmpeg process. An ordinary broadcaster subscription permits silent loss and lacks a durable lifetime contract. Unlimited buffering risks exhausting RAM when the disk stalls.

### 3. Explicit encoded-stream completion and bounded shutdown

Keep the same recorder attached across `DeviceSource` capture-process restarts; append each new MP3 frame sequence without truncation. Recording taps source audio upstream of gating, recognizer pauses, and music suppression. Capture downtime inserts no samples, matching the existing cumulative captured-media clock rather than wall-clock elapsed time.

Track completion of the encoded reader per ffmpeg lifetime. At normal replay EOF, drain fd 3 through that owner and preserve the available encoder tail before marking the source complete. During shutdown, stop/interrupt the source first while the encoded consumer remains alive, then join encoded consumption, close HTTP fan-out, and finish the recorder before taking the final audit/terminal snapshot. Remove the competing auxiliary discard reader when an encoded consumer owns the pipe; retain safe draining for otherwise unconsumed outputs. Ensure partial construction and repeated closes do not leak files or goroutines.

Recorder finalization gets a bounded wait (five seconds); if disk work has not completed, mark the recording incomplete and return without indefinitely waiting on filesystem I/O. Keep file ownership with the worker to prevent close/write races. Tests must use controllable blocking writers and release them afterward; an arbitrary blocked kernel write cannot be force-cancelled portably.

Write encoded MP3 bytes incrementally throughout the session; usability of already-persisted complete frames must not depend on shutdown-only metadata or finalization. After abrupt power loss, preserve the surviving complete-frame prefix as usable audio on a best-effort basis, allowing loss of application-buffered data, OS/storage-cache data, and an incomplete trailing frame. This is not a guarantee of power-loss durability or of losing only the recorder queue; filesystem and hardware behavior remain outside the recorder's control. Document those limits and test decoding a recording truncated without clean finalization, without treating that test as a simulation of physical power loss.

MP3 encoder delay/padding and restart boundaries mean decoded sample positions are not promised to match audit millisecond offsets exactly. Later comparison tooling must account for those and existing source-drop/restart diagnostics. Document this rather than claiming a lossless recognition-input recording.

**Alternatives:** Closing the broadcaster first or racing `proc.Close`'s discard goroutine can lose the tail. Padding downtime with synthesized silence creates misleading evidence and differs from the source-media clock.

### 4. Startup retention is a separate synchronous storage operation

Add a small retention helper in `internal/caption` (for example, `retention.go`) and invoke it from `newSession` immediately before `caption.NewWriter`, only when transcript persistence is enabled. Validate configuration first; scan and plan cleanup before any deletion. Missing root means zero existing bytes. Create the new session directory only after retention succeeds.

Walk the configured transcript root without following descendant symlinks. Sum logical regular-file sizes with checked integer arithmetic, including nested files and loose root files. Collect each immediate real child directory as an opaque deletion candidate; do not parse names or look for transcript/audio files. Sort by directory modification time, then basename for ties. This is the explicit operational definition of oldest: it works for legacy/unknown bundles without content detection. Changes to a directory's entries can change its ordering, which is documented.

If total usage is at or above the budget, remove whole candidates in order until usage is strictly below it. Use the measured bundle size to update the total after successful deletion. Root-level loose files count but are never individually deleted; symlink targets are neither counted nor followed. If accounting fails, delete nothing. On the first deletion failure or exhaustion of eligible directories while still over budget, return an actionable startup error rather than selecting newer directories or quietly proceeding over budget. Previous successful deletions cannot be rolled back; report the partial outcome. Log successful removed-directory count and logical bytes reclaimed.

No cleanup timer or runtime storage check is added. Even with audio opt-out, transcript sessions use retention. `--no-transcript` bypasses all storage access. No space for the next session is reserved and a running session can exceed the budget or fill the filesystem.

**Alternatives:** Recognizing directories by file contents conflicts with opaque cleanup and would skip old formats. Parsing timestamp names requires special handling for unknown directories. Continuous enforcement, filesystem quotas, and reserving a predicted session size add complexity and contradict startup-only cleanup.

### 5. Preserve existing audit behavior

Neither existing text lines nor existing audit record requirements change. New recording diagnostics use the current warning/error path and additive metrics data in summaries. Do not revive `transcript.jsonl`, add raw provider dumps, or build offline comparison features. Test existing session-start/session-end ordering and secret exclusion after wiring the recorder.

## Risks / Trade-offs

- [Automatic deletion destroys historical evidence, including legacy/unknown directory contents] → Clearly announce the default and budget in usage/package documentation; treat the configured transcript root as dedicated app-owned storage and require backups for data that must be retained.
- [Default recording captures sensitive audio even while recognition is paused] → Document default-on behavior and both opt-outs prominently; retain current local storage/access policy and expose no new historical-audio HTTP route.
- [A single session can fill the disk despite the budget] → State explicitly that this is a historical-storage threshold, not a filesystem quota. Keep disk failures best-effort for recording and avoid runtime cleanup.
- [Lossy MP3 and restart delays complicate future accuracy comparisons] → Preserve the current source encode and existing audit diagnostics; document timing limits and verify decoded files across restart boundaries in tests.
- [Slow or failed storage produces a partial recording] → Bounded queue, permanent failure status, and stop-on-first-loss semantics make incompleteness visible while protecting captions.
- [Cleanup races with another process or manual edits] → Support one writer per dedicated root; do not add content-based active-session detection. Document that concurrent writers or external mutation during cleanup are unsupported.
- [Directory mtime is not immutable creation time] → Define and document the ordering, add deterministic tests with explicit timestamps, and avoid pretending arbitrary directory names encode dates.
- [Blocked filesystem writes cannot always be cancelled] → Bound shutdown waiting, report incomplete output, and keep worker ownership explicit instead of blocking capture or shutdown on a file mutex.

## Migration Plan

1. Ship the new defaults and configuration examples in `README.md`, `docs/usage.md`, `deploy/default`, and `deploy/README.md`, plus an application `CHANGELOG.md` entry. No kiosk package changes are needed.
2. Explain before upgrade that the next persisted session can delete pre-existing directories, without conversion or per-format filtering. Operators should back up required history and set the budget/recording environment values before restarting the service.
3. New sessions add `audio.mp3`; existing transcript/audit files need no migration. At 128 kbps, audio is about 57.6 MB/hour, so 50 GB holds roughly 868 hours before text/audit overhead. Actual retained duration varies with session sizes.
4. To opt out of new audio persistence, set `LIVECAPTION_AUDIO_RECORDING=false`. To disable all storage activity, use the existing no-transcript setting. Raising the budget reduces cleanup pressure but does not recover deleted files.
5. Binary rollback leaves MP3 files harmlessly alongside existing transcripts; remove unsupported new settings from service configuration as necessary. Previously deleted history can only be restored from backup.
