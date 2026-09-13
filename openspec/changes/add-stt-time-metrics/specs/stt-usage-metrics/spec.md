## Purpose

Provide duration-based speech-to-text usage totals that operators can compare directly with provider billing while retaining byte-level diagnostics.

## ADDED Requirements

### Requirement: STT usage includes minute and hour totals
The system SHALL report cumulative STT audio sent during the current session as minutes and hours in the STT stats, derived from the duration represented by the successfully sent pipeline PCM bytes. The existing cumulative bytes-sent total MUST remain available and unchanged.

#### Scenario: Audio has been sent
- **WHEN** the stats are requested after pipeline PCM audio has been sent to the STT provider
- **THEN** the STT stats include numeric minute and hour totals representing that audio duration
- **AND** both duration totals correspond to the same bytes counted by the existing bytes-sent total

#### Scenario: No audio has been sent
- **WHEN** the stats are requested before any pipeline PCM audio has been sent
- **THEN** the STT minute and hour totals are both zero
- **AND** the existing bytes-sent total is zero

### Requirement: Admin STT card displays duration-based usage
The admin dashboard SHALL display cumulative audio-sent minutes and hours in the STT card alongside the existing audio-sent byte value. The displayed values SHALL refresh with the existing dashboard stats updates and remain cumulative when STT is paused.

#### Scenario: Dashboard receives updated STT usage
- **WHEN** an admin stats refresh contains updated STT minute and hour totals
- **THEN** the STT card displays the updated totals with their respective units
- **AND** the existing audio-sent byte value remains visible

#### Scenario: STT is paused
- **WHEN** the STT connection is intentionally paused and no audio is being sent
- **THEN** the displayed minute and hour totals retain their most recent cumulative values
