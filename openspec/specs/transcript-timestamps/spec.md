# Transcript Timestamps Specification

## Purpose

Define stable source-relative timestamps so every saved transcript line can be located within one continuous application session despite recognizer pauses or reconnects.

## Requirements

### Requirement: Saved timestamps use the source-session clock
The system SHALL timestamp each saved speech line at the source-media position where that finalized line begins. It SHALL timestamp each saved silence or music marker at the source-media position of the represented transition. Timestamps within one transcript SHALL be nondecreasing and SHALL NOT restart when a recognizer connection restarts.

#### Scenario: Automatic pause and resume
- **WHEN** automatic silence handling closes a recognizer connection and later opens a new connection in the same application session
- **THEN** lines produced after resume have source-relative timestamps later than or equal to preceding lines rather than timestamps restarted near zero

#### Scenario: Network reconnect
- **WHEN** recognition reconnects after a transient connection failure while the audio source continues
- **THEN** settled lines from the replacement connection retain their positions on the same source-session timeline

#### Scenario: Multiple lines begin together
- **WHEN** diarization or segmentation produces multiple finalized lines beginning at the same source-media position
- **THEN** the transcript MAY contain equal adjacent timestamps without treating them as a clock reset

### Requirement: Timing normalization follows delivered source audio
The system SHALL derive source-relative timing from the source positions of audio actually delivered to the recognizer. If buffering, pre-roll, or dropped audio creates a discontinuity between provider-local time and source time, the system SHALL preserve the source position of each resolvable word or event rather than assuming one constant offset for the whole application session.

#### Scenario: Resume includes pre-roll
- **WHEN** a replacement recognizer connection starts by sending buffered audio captured before speech reactivated the connection
- **THEN** resulting words are timestamped at those buffered frames' original source positions

#### Scenario: Buffered audio has a gap
- **WHEN** discarded buffered audio creates a gap in source positions represented by one recognizer connection
- **THEN** words after the gap are timestamped after that source gap rather than compressed onto a contiguous provider-local timeline

### Requirement: Speech and non-speech events share one timeline
The system SHALL normalize settled speech and recognizer-reported music transitions onto the same source-session clock before caption assembly compares or records their times. Silence markers derived by caption assembly SHALL use that same timeline.

#### Scenario: Music spans a recognizer connection boundary
- **WHEN** music detection and automatic pause or reconnect activity occur during one application session
- **THEN** music markers, silence markers, and subsequent speech remain correctly ordered on one source-relative timeline

#### Scenario: Music filtering compares event and word times
- **WHEN** the recognizer reports speech near a music-ending boundary
- **THEN** filtering compares the music edge and word positions after both have been normalized to source time

### Requirement: Transcript compatibility is preserved
The system SHALL retain the existing human-readable transcript line structure and SHALL require no new command-line option or configuration to obtain source-relative timestamps. Existing transcript files SHALL remain unchanged.

#### Scenario: New transcript output
- **WHEN** a session records a transcript after this change
- **THEN** each line continues to use the existing `[clock]`, optional `[speaker]`, and text structure with corrected clock values

#### Scenario: Historical transcript
- **WHEN** the application is upgraded with older transcript files already present
- **THEN** those files are neither rewritten nor migrated
