## Purpose

Provide a durable, machine-readable session record that lets post-event tools correlate delivered captions with source timing, noise-gate decisions, and operational faults.

## ADDED Requirements

### Requirement: Each recorded session has a versioned audit stream
When transcript recording is enabled, the system SHALL create an append-only `audit.jsonl` file beside `transcript.txt`. Each line MUST be an independently parseable JSON object containing a schema version and record type, and `transcript.txt` MUST retain its existing human-readable format.

#### Scenario: Transcript recording is enabled
- **WHEN** a captioning session creates its transcript directory
- **THEN** that directory contains both `transcript.txt` and `audit.jsonl`
- **AND** every completed audit line is valid JSON with a schema version and record type

#### Scenario: Transcript recording is disabled
- **WHEN** a session runs with transcript recording disabled
- **THEN** the system creates neither the transcript nor the audit stream

#### Scenario: Existing transcript consumers read a new session
- **WHEN** a consumer reads `transcript.txt` from a session containing an audit stream
- **THEN** its established clock, optional speaker, and text structure is unchanged

### Requirement: Audit records identify the session without exposing secrets
The audit stream SHALL begin with metadata identifying the session, application version, absolute UTC start time, source kind and format, recognition engine and non-secret recognition settings, and initial noise-gate settings. It MUST NOT record credentials, API keys, admin passwords, or authorization headers.

#### Scenario: Session metadata is inspected
- **WHEN** a post-event tool reads the first audit record
- **THEN** it can identify the schema, session, application version, start time, source, recognition configuration, and noise-gate configuration without consulting another file

#### Scenario: Configuration contains secrets
- **WHEN** the application starts with provider or administration credentials
- **THEN** no secret value appears in the audit stream

### Requirement: Final output is recorded on the source timeline
The audit stream SHALL record each finalized caption line and each saved silence or music marker. Each output record MUST contain its type, text or marker identity, source-relative start time in integer milliseconds, absolute UTC observation time, and speaker when known. Caption records SHALL include source-relative end time in integer milliseconds when timing is available, and timestamps SHALL remain on the continuous source-session clock across recognition pauses and reconnects.

#### Scenario: Final caption is saved
- **WHEN** a finalized caption line is written to `transcript.txt`
- **THEN** a corresponding audit record contains the same visible text, its source-relative millisecond timing, observation time, and known speaker

#### Scenario: Timing is unavailable
- **WHEN** the recognizer cannot provide a reliable caption end time
- **THEN** the audit record omits or explicitly marks the unavailable end rather than inventing one

#### Scenario: Recognition reconnects
- **WHEN** captions are finalized before and after a recognizer reconnect
- **THEN** their audit timestamps remain nondecreasing on one source-session timeline

### Requirement: Noise-gate transitions are auditable
The audit stream SHALL record every effective noise-gate open and close transition, including the new state, source-relative position in integer milliseconds, absolute UTC observation time, and the threshold and release settings governing the transition.

#### Scenario: Noise gate opens for speech
- **WHEN** the noise gate changes from closed to open
- **THEN** one audit record identifies the open transition and the source-media position at which audio became eligible for recognition

#### Scenario: Noise gate closes after release
- **WHEN** the noise gate changes from open to closed
- **THEN** one audit record identifies the close transition and the source-media position at which the transition took effect

#### Scenario: Noise-gate settings change during a session
- **WHEN** an operator changes the threshold or release setting
- **THEN** the audit stream records the new settings before subsequent transitions rely on them

### Requirement: Operational degradation is correlated with the session
The audit stream SHALL record application warnings and errors emitted during the session, relevant source and recognition state transitions, reconnects, intentional recognition pauses, audio or recognition-buffer drops, source restarts, and transcript write failures. Each record MUST include an absolute UTC observation time and session-relative elapsed milliseconds; it SHALL include a source-relative position when one is known.

#### Scenario: Provider connection fails and recovers
- **WHEN** the recognizer reports a warning or error and reconnects
- **THEN** the audit stream contains the diagnostic and state transitions needed to place the interruption within the session

#### Scenario: Audio is dropped
- **WHEN** a source, monitor, stream, or recognition buffer reports dropped data
- **THEN** an audit record identifies the affected component and cumulative or event count

#### Scenario: Diagnostic has no source position
- **WHEN** a warning or error cannot be tied reliably to a media position
- **THEN** its record retains wall-clock and session-elapsed timing and does not invent source timing

### Requirement: Clean shutdown records a final session summary
On clean session shutdown, the system SHALL append a final summary containing the metrics snapshot used by the existing shutdown summary, including health, source drops and restarts, recognition reconnects and buffer drops, pause and usage totals, latency statistics, and transcript errors.

#### Scenario: Session shuts down normally
- **WHEN** the application completes its shutdown sequence
- **THEN** the final complete audit record is a session summary containing cumulative operational metrics

#### Scenario: Process or power terminates unexpectedly
- **WHEN** the process cannot perform clean shutdown
- **THEN** previously flushed complete JSON Lines records remain independently readable even though a final summary may be absent

### Requirement: Audit failures do not interrupt live captioning
Audit persistence SHALL be best-effort. A create, encode, write, flush, or close failure MUST NOT stop live captions, and the failure SHALL be exposed through existing logging or health reporting without recursively attempting to write the same failure to the unavailable audit stream.

#### Scenario: Audit storage fails during an event
- **WHEN** the audit stream cannot accept another record
- **THEN** live caption publication continues
- **AND** the failure is surfaced outside the failed audit stream
