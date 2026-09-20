## Context

See `proposal.md` for motivation and `specs/transcript-timestamps/spec.md` for required behavior.

Audio sources already maintain one offset across their lifetime, including device-capture process restarts. In production source implementations, `audio.Frame.Offset` identifies the end of the frame even though its current field comment says first sample. `startDrain` copies frame PCM and capture time into an STT ring chunk but drops that source offset. Each recognizer WebSocket then starts a byte-based `anchorIndex` at zero; the index currently maps provider-local media positions only to capture and send wall times.

Settled transcripts pass through the shared session read loop, but Speechmatics music events invoke the caption hub directly from provider decoding. The hub therefore receives speech and music on the same connection-local clock and deliberately treats negative gaps or a music-off edge at zero as connection resets. The writer prints the resulting line offset without further interpretation.

## Goals / Non-Goals

**Goals:**
- Establish the audio source clock as the timing contract leaving the shared STT driver.
- Reuse the existing byte anchor index to map provider-local positions accurately across pre-roll and source gaps.
- Normalize both individual word positions and music-event boundaries before the hub compares them.
- Keep latency capture/send anchoring and provider-specific decoding responsibilities intact.
- Make reconnect and automatic-pause behavior testable without starting the application.

**Non-Goals:**
- Reconstruct or rewrite historical transcript files.
- Change caption text, speaker attribution, endpointing, auto-pause thresholds, or music classification.
- Add wall-clock timestamps, connection identifiers, configuration, or a second transcript format.
- Guarantee meaningful timing for malformed provider positions outside all retained audio anchors.

## Decisions

### 1. Map provider positions to source offsets in the existing anchor index

Carry each frame's source end offset into the STT ring chunk and record it beside the chunk's provider byte range in `anchorIndex`. Resolve source time with the same covering-entry lookup and within-chunk interpolation already used for capture time.

The mapping is piecewise rather than a single per-connection delta. If the ring discards audio or a replacement connection begins with pre-roll, adjacent provider bytes can correspond to non-adjacent source positions; the chunk anchor preserves that discontinuity.

Alternatives considered:
- **Add a cumulative duration at every reconnect:** fails because recognizer time counts submitted audio while source time also includes omitted silence and dropped audio.
- **Timestamp writes from wall time:** records recognition/publication delay rather than where speech occurred and weakens replay semantics.
- **Create a separate source-time index:** duplicates the existing byte-range search and retention rules.

### 2. Normalize every timed word, then re-derive its transcript bounds

For each settled transcript, map each timed word's start and end independently. Recompute `Transcript.Start` and `Duration` from the mapped surviving word bounds. This preserves gaps inside one provider result instead of applying the end word's translation delta to the entire result.

For the untimed fallback, map its transcript start/end positions as a unit because no finer provider timing exists. Capture/send latency remains resolved from the original provider-local end before timing fields are replaced.

If an individual lookup cannot be resolved from retained anchors, keep the result publishable rather than dropping caption text. Tests and normal provider limits must ensure expected finalized results remain inside the anchor retention window; unresolved timing continues to be treated as unavailable rather than fabricated.

Alternative considered:
- **Shift the entire transcript by one delta:** smaller, but wrong when one result crosses a buffered-audio discontinuity.

### 3. Return music events to the shared read loop for normalization

Provider decoding will return an optional typed music edge together with settled transcripts instead of calling the hub callback directly. The shared read loop will resolve that edge through the same connection anchor before invoking the configured music callback.

This keeps the provider responsible for interpreting Speechmatics message shapes while making the shared connection driver the sole boundary where provider-local timing becomes source timing. Deepgram returns no music edge, preserving its behavior.

Alternative considered:
- **Inject a mapper callback into the Speechmatics session:** couples the provider adapter to mutable connection-driver state and leaves ordering between transcript and event normalization implicit.

### 4. Remove connection-reset meaning from normalized hub timestamps

Once all timed STT output is source-relative, reconnects no longer produce negative media gaps or a music-off edge at zero. Keep negative-gap handling as a defensive row break for contradictory input, but stop using zero as a recognizer-reset signal in the normal session path. Reset any connection-scoped music hold explicitly when a connection ends or a replacement begins, rather than encoding lifecycle in a timestamp value.

The explicit reset must discard held old-connection material and clear pending music timers before new normalized events arrive. It must not write an extra marker solely because a WebSocket changed.

Alternative considered:
- **Leave the zero-edge reset path in place:** source time can legitimately be zero only at session start, and relying on a fabricated event would preserve two competing meanings for timestamps.

### 5. Keep transcript serialization unchanged

`caption.Writer` continues formatting `Line.OffsetMS`; correctness moves upstream so speech, silence, and music all share one clock before line assembly. No output versioning or migration is needed.

## Risks / Trade-offs

- **Provider results outlive anchor retention** -> Keep existing bounded retention sized well beyond configured provider finalization delay and add focused boundary tests; do not invent source positions when lookup is impossible.
- **A word boundary lands exactly between chunks** -> Define and test boundary lookup against the following byte range while preserving nondecreasing mapped word times.
- **Changing music-event delivery alters ordering** -> Decode and dispatch events synchronously in the read loop in message order, with adapter and hub integration tests around music end and returning speech.
- **Frame offset semantics are currently misdocumented** -> Correct the comment and pin source-end interpolation with tests before relying on it.
- **Explicit connection reset could lose valid trailing results** -> Perform reset only after the existing graceful finish/drain completes; reconnect failures already abandon unreadable connection output.

## Migration Plan

Ship through the normal application release process. New sessions immediately write source-relative timestamps; existing files remain untouched. Verify with focused STT anchor/session tests, Speechmatics music tests, caption hub/writer tests, the full Go test suite, build, and lint.

Rollback is the prior application version. No persisted state or configuration requires cleanup.
