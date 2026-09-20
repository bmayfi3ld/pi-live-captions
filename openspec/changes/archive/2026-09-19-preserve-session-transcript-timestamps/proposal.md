## Why

Saved transcript timestamps currently use each recognizer WebSocket's connection-local media clock. Automatic silence pauses and network reconnects restart that clock at zero, so one application session can contain repeated and backward timestamps that no longer identify positions in the source audio.

## What Changes

- Preserve each audio frame's source-session offset through buffering and recognizer delivery.
- Normalize settled transcript words and Speechmatics music edges from provider-local time onto the monotonic source-session clock before they reach the caption hub.
- Keep saved speech, silence, and music timestamps source-relative across automatic pauses, reconnects, pre-roll, and buffered-audio discontinuities.
- Retain existing transcript text format, provider timing for latency metrics, and live caption behavior.

## Capabilities

### New Capabilities
- `transcript-timestamps`: Defines source-relative, monotonic timestamp behavior for saved transcripts across recognizer connection boundaries.

### Modified Capabilities

None.

## Impact

- Shared STT buffering and per-connection anchor mapping in `internal/stt/`.
- Speechmatics music-event delivery and transcript timing normalization.
- Caption hub reset assumptions and saved transcript timestamp coverage.
- Focused STT, hub, and writer tests plus transcript documentation and the application changelog.
- No wire-format, command-line, configuration, dependency, or historical-transcript migration changes.
