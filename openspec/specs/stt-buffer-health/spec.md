# stt-buffer-health Specification

## Purpose
Keep STT buffer-loss metrics and operator health reporting accurate across intentional silence pauses and speech resumption, without hiding active-period audio loss.

## Requirements

### Requirement: Paused-period pre-roll eviction is not degradation
The system SHALL exclude audio buffered while the STT auto-pause gate was inactive from `stt.buffer_drops_total` when that audio is evicted. Such evictions MUST NOT create or renew a degradation event, regardless of the gate state at eviction time.

#### Scenario: Buffer rotates during a pause
- **WHEN** the bounded buffer evicts paused-period audio while STT remains intentionally paused
- **THEN** the buffer-drop total remains unchanged
- **AND** the eviction does not create or renew degradation

#### Scenario: Speech resumes into a full silence buffer
- **WHEN** speech resumes and incoming audio evicts only paused-period audio during resume/redial
- **THEN** the buffer-drop total remains unchanged
- **AND** after STT reconnects successfully, health is `ok` if no unrelated degradation or standing fault exists

### Requirement: Active-period eviction remains observable
The system SHALL increment `stt.buffer_drops_total` once for each evicted chunk buffered while the STT auto-pause gate was active, and SHALL record the existing degradation event for that loss. Later gate transitions MUST NOT change whether that chunk's loss is counted. With auto-pause disabled, all buffered audio SHALL retain active-period eviction accounting.

#### Scenario: Resume connection takes longer than buffer capacity
- **WHEN** returning audio fills the buffer until active-period chunks are evicted while the gate remains active
- **THEN** each active-period eviction increments the buffer-drop total
- **AND** health reports `degraded` under the existing recent-event health rules

#### Scenario: Gate closes before active-period audio is evicted
- **WHEN** audio buffered while the gate was active is evicted after the gate becomes inactive
- **THEN** that loss is counted and records a degradation event
- **AND** existing health precedence is preserved, including intentional paused health when no live input fault is active

#### Scenario: Auto-pause disabled
- **WHEN** auto-pause is disabled and the bounded buffer evicts audio, including silent audio
- **THEN** each eviction increments the buffer-drop total and records degradation as before

### Requirement: Accounting correction preserves buffering and unrelated health
The system SHALL preserve existing buffer capacity, oldest-first eviction, retained audio ordering and capture timestamps, and immediate gate-driven resume behavior. Exempting paused-period evictions MUST NOT clear earlier loss counters or unrelated degradation and standing faults.

#### Scenario: Retained pre-roll survives accounting correction
- **WHEN** paused audio is buffered and speech resumes
- **THEN** the buffer retains and delivers the same bounded, ordered pre-roll and returning audio with unchanged capture timestamps
- **AND** no buffer flush or additional resume delay is introduced to suppress warnings

#### Scenario: Unrelated fault exists during resume
- **WHEN** paused-period evictions occur while an unrelated fault already requires degraded health
- **THEN** those evictions leave the fault and its health effect intact
- **AND** existing accumulated drop totals are not reset
