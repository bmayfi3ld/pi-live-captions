## Why

Speechmatics can report distinct music intervals without any recognized speech between them, causing timestamped transcripts to contain adjacent identical music markers that add clutter but no readable information. Saved transcripts should represent one uninterrupted non-speech region with one marker while preserving genuine transitions around spoken content.

## What Changes

- Compact adjacent identical non-speech markers in saved transcripts, retaining the first marker and its timestamp.
- Reset compaction when any different transcript line is written, so music separated by speech or another marker remains visible.
- Apply the rule to recognized transcript markers rather than music alone, so silence markers receive the same behavior whenever adjacent duplicates occur.
- Leave live viewer/admin event delivery, music detection, suppression timing, speech lines, and historical transcript files unchanged.

## Capabilities

### New Capabilities
- `transcript-marker-compaction`: Defines how adjacent non-speech markers are represented in newly recorded timestamped transcripts.

### Modified Capabilities

None.

## Impact

- Affects the transcript recording path in `internal/caption/writer.go` and focused coverage in `internal/caption/writer_test.go` and/or `internal/caption/markers_test.go`.
- Requires an application `CHANGELOG.md` entry.
- Does not alter public APIs, SSE events, provider integrations, deployment configuration, dependencies, or kiosk behavior.
