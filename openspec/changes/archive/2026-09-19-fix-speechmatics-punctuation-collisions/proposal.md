## Why

Saved Speechmatics transcripts contain collisions such as `would,,`, `And,,`, and `Winston.,`. The adapter drops profanity/disfluency words but blindly attaches their punctuation to the previous surviving word, allowing these artifacts; saved text alone cannot prove the removed tokens behind each observed occurrence.

## What Changes

- Reconcile comma and sentence-ending punctuation brought together by tagged-word removal during Speechmatics result assembly.
- Preserve legitimate punctuation unrelated to removal, including `a.m.,`, ellipses, and question/exclamation combinations.
- Keep surviving word timing, speaker grouping, and sentence-ending behavior intact so live captions and saved transcripts use the same corrected words.
- Add focused regression coverage and an application changelog entry during implementation.
- Do not add a global text filter, change Deepgram, rewrite historical transcripts, or modify deployment/kiosk behavior.

## Capabilities

### New Capabilities

- `speechmatics-punctuation-cleanup`: Removal-aware punctuation reconciliation in Speechmatics transcripts without changing unrelated text or word timing.

### Modified Capabilities

None.

## Impact

Implementation is localized to `internal/stt/speechmatics/speechmatics.go` and its tests, plus `CHANGELOG.md`. Caption hub tests may verify sentence closure and wire-word consistency. No new dependencies, configuration, API, wire format, or live-box changes are required.
