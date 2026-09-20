## Context

See `proposal.md` for motivation. `caption.Hub` already suppresses repeated music-on edges while music remains active and emits transcript markers only through `OnMarker`; however, a confirmed music end followed by another start is a real detector transition even when no speech line reaches the transcript between them. The live SSE stream and saved transcript therefore have different needs: clients need current transitions, while the append-only file needs a concise readable history.

`caption.Writer.Write` is the common serialized boundary for finalized speech and non-speech lines. It already owns file formatting, append behavior, locking, and persisted line/byte metrics. Marker text is currently the established `♪ music ♪` and `— silence —` vocabulary.

## Goals / Non-Goals

**Goals:**
- Compact only adjacent identical recognized transcript markers.
- Keep the first marker and timestamp in each compacted run.
- Let any successfully written different line end the run.
- Keep persisted metrics aligned with bytes and lines actually written.

**Non-Goals:**
- Changing provider event interpretation, music-end hold timing, caption suppression, or SSE behavior.
- Deduplicating speech or arbitrary repeated text.
- Rewriting existing transcript files or adding marker types/configuration.
- Correcting connection-relative transcript timestamps.

## Decisions

### Compact at the transcript writer

Track the last successfully written recognized marker in `Writer`, under its existing mutex. Before formatting a line, omit it only when its text is a recognized marker equal to that remembered marker. After a successful different write, remember its marker text or clear marker state for ordinary speech.

This keeps persistence policy at the persistence boundary and naturally excludes skipped lines from transcript metrics. It also avoids changing the hub's live state machine.

Alternative: suppress repeated markers in `Hub.SetMusic` or the `OnMarker` callback. Rejected because the hub must continue representing real live transitions, and it would need extra state describing whether transcript content—not live events—occurred between transitions.

### Use the existing closed marker vocabulary

Recognize the exact established music and silence marker strings. This applies one rule to both current marker types, including adjacent silence markers if producer behavior later makes them possible, while ensuring identical speech remains verbatim.

Alternative: deduplicate every adjacent identical `Line.Text`. Rejected because repeated speech such as acknowledgements, readings, or refrains is valid transcript content.

Alternative: add a new marker kind field to `Line`. Rejected because only two internal marker strings exist and no other behavior needs a broader wire or data-model change.

### Retain the first marker

Because transcript files are append-only and periodically flushed, the writer will keep the first marker and drop later adjacent copies. Its timestamp marks the beginning of the uninterrupted transcript-level region and requires no buffered replacement or file rewriting.

## Risks / Trade-offs

- [A new marker string is introduced but not recognized] → Keep the recognized marker vocabulary explicit and extend the focused test when a new marker type is added.
- [String identity couples compaction to display text] → Accept the small closed vocabulary rather than expanding `Line`; introduce typed markers only if marker behavior grows beyond this rule.
- [Compaction accidentally removes legitimate repeated speech] → Test identical ordinary lines alongside music and silence cases.
- [Metrics count omitted markers] → Return before formatting and metric updates, and assert persisted counters reflect only actual writes.

## Migration Plan

Ship as an application-only behavior change with a changelog entry and focused writer/hub tests. New sessions compact markers immediately; historical transcripts remain unchanged. Rolling back restores repeated marker writes without data migration.
