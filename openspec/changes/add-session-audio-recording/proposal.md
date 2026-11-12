# Proposal

## Why

The saved transcript and debug audit log lack the source audio needed for later high-accuracy batch transcription and accuracy comparisons. Saving the existing MP3 encode adds that evidence with minimal CPU overhead, while startup retention prevents historical session bundles from accumulating indefinitely.

## What Changes

- Save `audio.mp3` beside `transcript.txt` and `audit.jsonl` by default for live and replay sessions, copying the existing 128 kbps source encode rather than encoding twice.
- Add an independent audio-recording opt-out; `--no-transcript` disables all session files and retention, while `--no-audio-stream` disables HTTP playback without disabling recording.
- Keep recording independent of recognition pauses, noise gating, and HTTP listener presence. Surface recording failures without interrupting captions.
- Before creating a transcript session directory, apply a configurable storage budget, defaulting to 50 GB (50,000,000,000 bytes). Remove entire oldest directories until stored usage is below the budget, including legacy directories without audio; do not inspect their transcript contents.
- Check retention only at startup. An active session may exceed the budget, and no files are removed during that run.
- Document default audio capture, automatic deletion, storage sizing, configuration, and failure behavior.
- Exclude batch transcription, accuracy scoring, web playback/download of historical files, and kiosk changes.

## Capabilities

### New Capabilities

- `session-audio-recording`: Best-effort, default-on persistence of the shared source MP3 stream for later offline analysis.
- `session-storage-retention`: Configurable startup-only storage accounting and oldest-directory-first deletion of historical session bundles.

### Modified Capabilities

None. The existing `session-audit-log` requirements remain intact; the debug companion is already `audit.jsonl`, not the retired `transcript.jsonl`.

## Impact

- `internal/audio`: shared encoded-output fan-out, bounded asynchronous disk sink, and clean EOF/restart/shutdown handling.
- `internal/cli`: output flags, encoder demand independent of HTTP serving, startup cleanup, recording lifecycle, and diagnostics.
- `internal/caption`: existing session-directory lifecycle is the integration point for retention and the audio companion; text/audit formats remain compatible.
- Metrics/terminal diagnostics as needed to expose recording status and permanent failures; no new public HTTP endpoints or runtime dependencies.
- `README.md`, `docs/usage.md`, `deploy/default`, `deploy/README.md`, and application `CHANGELOG.md` must explain the recording and destructive retention defaults.
