# Spec Delta

## Purpose

Preserve source audio alongside each session's transcript and audit log so later offline tools can analyze recognition accuracy without adding another capture or encode pipeline.

## ADDED Requirements

### Requirement: Session audio is recorded by default
For live and replay sessions with transcript persistence enabled, the system SHALL save `audio.mp3` in the same session directory as `transcript.txt` and `audit.jsonl`, unless audio recording is explicitly disabled. Existing transcript and audit formats MUST remain compatible.

#### Scenario: Default live or replay session
- **WHEN** a session starts with default output options and MP3 encoding and storage are available
- **THEN** source MP3 audio is saved as `audio.mp3` beside the text transcript and audit stream

#### Scenario: Audio recording alone is disabled
- **WHEN** the session runs with `--no-audio-recording`
- **THEN** no audio recording is created
- **AND** text and audit persistence and HTTP audio streaming retain their independently configured behavior

#### Scenario: All transcript persistence is disabled
- **WHEN** the session runs with `--no-transcript`
- **THEN** no session directory, text transcript, audit stream, or audio recording is created
- **AND** HTTP audio streaming remains independently configurable

### Requirement: Recording shares the source MP3 encode
The system SHALL persist the existing 128 kbps source MP3 encode without a second capture or audio encode. Recording SHALL work without HTTP listeners and when HTTP audio streaming is disabled. It MUST NOT change the quality or availability of an enabled HTTP stream.

#### Scenario: Recording and streaming are enabled together
- **WHEN** a session produces MP3 audio for recording and HTTP listeners
- **THEN** the recorder and listeners receive the same encoded source bytes rather than independently encoded audio

#### Scenario: HTTP streaming is disabled
- **WHEN** recording is enabled and `--no-audio-stream` is set
- **THEN** the source MP3 encode is still produced for the recording
- **AND** the HTTP audio route remains disabled

#### Scenario: No listeners connect
- **WHEN** no HTTP listener connects during an otherwise healthy session
- **THEN** the audio recording still contains the available source audio from session capture start to end

### Requirement: Recognition filtering does not remove recorded audio
Recording SHALL preserve available source audio independently of noise-gate state, silence auto-pause, music suppression, and recognition reconnects. After a capture restart, recording SHALL append subsequent source audio to the same file; it MUST NOT insert fabricated audio for unavailable source periods.

#### Scenario: Recognition pauses or suppresses input
- **WHEN** silence auto-pause, a closed noise gate, music suppression, or a recognition reconnect prevents audio from reaching recognition
- **THEN** the available source MP3 audio continues to be recorded

#### Scenario: Capture restarts
- **WHEN** the source capture process ends and subsequently recovers
- **THEN** the new encoded audio is appended to the same `audio.mp3`
- **AND** source outages remain visible through the existing source diagnostics rather than invented silence samples

### Requirement: Recording faults cannot stall live captioning
Recording SHALL use bounded buffering and MUST NOT apply disk backpressure to capture, recognition, or HTTP audio. A create, write, queue-overflow, or close failure SHALL visibly mark recording as failed without interrupting captions or other persistence. On overflow or write failure, the system SHALL stop accepting recording data rather than silently resume a file with unreported missing bytes.

#### Scenario: Disk writing is slower than capture
- **WHEN** the recording buffer fills
- **THEN** recording is retired with a persistent failure diagnostic
- **AND** capture, caption delivery, and HTTP streaming continue without waiting for the disk writer

#### Scenario: Recording cannot be created or written
- **WHEN** the recording file cannot be opened or a write fails
- **THEN** the failure is logged and exposed in recording status
- **AND** text/audit persistence and captions continue where their own sinks are healthy

#### Scenario: Encoder is unavailable
- **WHEN** the installed ffmpeg lacks the required MP3 encoder
- **THEN** recording is reported unavailable and no alternative encoder is silently substituted
- **AND** captions and transcript persistence continue

### Requirement: Recording lifecycle is finalized explicitly
The system SHALL attach recording before source audio starts. On normal EOF or clean shutdown it SHALL drain queued recording data and close the file before reporting final recording status. Shutdown SHALL remain bounded if recording storage is blocked, and a recording that cannot be fully finalized SHALL be reported incomplete.

#### Scenario: Replay ends normally
- **WHEN** a replay reaches EOF with healthy recording storage
- **THEN** queued encoded audio, including the available encoder tail, is written before the recorder is closed
- **AND** the resulting file is decodable as MP3

#### Scenario: Session shuts down with healthy storage
- **WHEN** a live or replay session shuts down cleanly
- **THEN** the source stops producing encoded data before the recorder is drained and closed
- **AND** final diagnostics reflect any recording failure

#### Scenario: Power is lost during recording
- **WHEN** power is lost before recording can be finalized
- **THEN** the recording preserves already-persisted complete MP3 frames as usable audio on a best-effort basis, without requiring clean shutdown
- **AND** buffered or not-yet-durable audio and an incomplete trailing frame may be lost
- **AND** recovery of audio not preserved by the filesystem or storage hardware is not guaranteed

#### Scenario: Storage does not finish pending writes
- **WHEN** storage blocks recording finalization beyond the shutdown deadline
- **THEN** application shutdown does not wait indefinitely for the recorder
- **AND** diagnostics identify the recording as incomplete
