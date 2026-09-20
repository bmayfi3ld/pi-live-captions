# transcript-marker-compaction Specification

## Purpose
Keep saved timestamped transcripts readable by collapsing redundant adjacent non-speech markers without removing meaningful transitions or spoken content.

## Requirements

### Requirement: Compact adjacent identical non-speech markers
The system SHALL write only the first marker in an uninterrupted sequence of identical recognized non-speech markers in a newly recorded transcript. The retained line SHALL keep the first marker's timestamp, and suppressed marker attempts MUST NOT increment persisted transcript line or byte metrics.

#### Scenario: Repeated music markers without intervening content
- **WHEN** two or more music markers are offered to the transcript consecutively
- **THEN** the transcript contains only the first music marker with its original timestamp

#### Scenario: Repeated silence markers without intervening content
- **WHEN** two or more silence markers are offered to the transcript consecutively
- **THEN** the transcript contains only the first silence marker with its original timestamp

### Requirement: Preserve meaningful marker boundaries
Any different line successfully written after a marker SHALL end that marker's compaction sequence. The system MUST preserve repeated speech lines and MUST NOT apply marker compaction to arbitrary identical text.

#### Scenario: Speech separates matching markers
- **WHEN** a music marker, a speech line, and another music marker are offered in that order
- **THEN** all three lines are written in order

#### Scenario: A different marker separates matching markers
- **WHEN** a music marker, a silence marker, and another music marker are offered in that order
- **THEN** all three markers are written in order

#### Scenario: Identical speech remains verbatim
- **WHEN** two identical speech lines are offered consecutively
- **THEN** both speech lines are written

### Requirement: Keep live event behavior independent
Transcript marker compaction SHALL NOT suppress or alter music, silence, status, caption, or snapshot events delivered to live viewer and admin clients.

#### Scenario: A redundant saved marker has a live transition
- **WHEN** a recognized non-speech transition produces a marker identical to the previous saved transcript line
- **THEN** the redundant transcript marker is omitted while live event delivery remains governed by the existing event-state rules
