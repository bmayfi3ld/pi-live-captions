## Context

See `proposal.md` for motivation. `serverMessage.transcripts()` in `internal/stt/speechmatics/speechmatics.go` skips words tagged `profanity` or `disfluency` before speaker/run bookkeeping. Separate punctuation results append unconditionally to the last surviving `stt.Word`. Existing tests deliberately preserve a single comma after removal and assert word timing, empty-result behavior, and speaker grouping, but do not exercise collisions.

`Transcript.Text()` joins words for the hub and transcript writer, whereas browser events use `wireWords()`. The hub closes lines using terminal `.`, `?`, or `!`. Text-only cleanup after this split would leave browser words inconsistent or affect sentence closure.

Design is included because removal provenance and abbreviation punctuation require an explicit distinction before coding. All production examples inspected were Speechmatics, per the user. Saved output confirms malformed punctuation but lacks raw result tags; it does not prove every example's cause.

## Goals / Non-Goals

**Goals:** Reconcile punctuation at its attachment point, retaining provider timing and existing transcript structure. Keep state local to assembly of one result message and avoid buffering or latency changes.

**Non-Goals:** General grammar correction, abbreviation detection from flat strings, punctuation repair across already-published messages, new public transcript fields, configurable filter infrastructure, or changing speaker attribution of punctuation after removed interjections.

## Decisions

### Fix the Speechmatics adapter, not downstream text

Track whether tagged words were removed since the last kept word and which trailing punctuation came from separate punctuation results. Use a small amount of local state in `transcripts()`; a tiny private helper is acceptable if it makes the collision rule clearer, but no shared filter interface is needed.

Reset removal and punctuation provenance when accepting a new surviving word and at run resets. Keep removal context through consecutive removed words and their punctuation. Do not persist it across messages. Unknown/non-word result handling remains unchanged.

A global regex was rejected: it cannot distinguish an artifact from valid `a.m.,`, ellipses, or `?!`, and updating only `Transcript.Text()` would miss word-based browser events.

### Reconcile only the boundary exposed by removal

Recognize commas and terminal punctuation (`.`, `?`, `!`) supplied as separate results. For a collision across removed words:

| Existing separate punctuation | Incoming separate punctuation | Result |
| --- | --- | --- |
| comma | comma | one comma |
| comma | sentence ending | incoming sentence ending |
| sentence ending | comma | existing sentence ending |
| sentence ending | sentence ending | existing sentence ending |

Keeping the existing ending when two endings meet is the conservative planning default: it preserves the surviving text's established question/exclamation rather than inventing a new combination. Preserve punctuation clusters that were contiguous without removal; reconciliation must not flatten existing ellipses or question/exclamation combinations. Other punctuation remains unchanged.

Track the suffix contributed by punctuation results rather than classifying the final character of the whole word. An embedded period in `a.m.` is not a separate punctuation result and must survive with an attached comma, even if that comma follows a removed word. This avoids needing an abbreviation dictionary.

If the retained word has no conflicting separate punctuation, preserve the existing attachment behavior. Leading punctuation still drops, and all-filtered messages still emit nothing.

### Change text only, not timing or grouping

Mutate the assembled word text before the transcript is returned. Keep `Word.Start` and `Word.End` untouched. Continue the existing run-end update from an attachable punctuation result even when its textual mark is suppressed; changing that would also change media gaps used by the hub. Keep existing speaker-run rules, including ignored all-filtered interjections.

Do not alter the wire shape, hub assembly, transcript writer, provider configuration, or flat-text fallback. Corrected `Words` automatically feed both downstream representations.

## Risks / Trade-offs

- Unknown raw input for historical examples -> Use deterministic synthetic result fixtures reproducing the known assembly mechanism; do not claim all provider-originated punctuation errors are fixed.
- Over-normalizing legitimate punctuation -> Require removal provenance and separate-result provenance; test embedded abbreviation periods, ellipses, and `?!` explicitly.
- State leaking to subsequent words or speaker runs -> Reset state on kept-word/run boundaries and cover consecutive removals, leading removals, and speaker changes.
- Losing a terminal mark delays saved-line closure -> Pin comma/terminal precedence and verify a corrected terminal transcript through the existing hub tests or an equivalent adapter-to-hub test.
- Cross-message collisions remain outside scope -> No retracting already-published words or adding a look-behind buffer. Revisit only with raw-message evidence of a separate issue.

## Migration Plan

No data migration or configuration changes. After review and explicit implementation authorization, ship through the normal application release process with a changelog entry. Historical transcript files remain untouched. Rollback is the prior application version. Verification uses Go tests/build/lint without starting the application or touching the live service; browser presentation remains a user check.
