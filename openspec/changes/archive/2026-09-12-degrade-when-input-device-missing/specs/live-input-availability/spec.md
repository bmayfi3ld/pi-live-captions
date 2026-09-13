## Purpose

Keep a caption appliance reachable and explain how to restore its configured live input when that input is missing or unavailable.

## ADDED Requirements

### Requirement: Missing input does not terminate the web session
The system SHALL continue serving `/admin`, `/api/stats`, and the viewer when the configured live input is rejected by device enumeration or cannot be opened for capture, provided unrelated startup prerequisites succeed. Existing admin authentication and `/healthz` liveness behavior SHALL remain unchanged. The system MUST NOT intentionally substitute a different input for a device rejected by validation.

#### Scenario: Configured input absent from an available device listing
- **WHEN** the configured device is not found in a nonempty enumeration for its backend
- **THEN** the web session remains reachable and reports that configured input as missing
- **AND** capture is blocked rather than attempting an input that could fall back to the default device

#### Scenario: Device passes validation but capture cannot open it
- **WHEN** a configured live input passes or skips enumeration validation but fails to open, including a missing ALSA card
- **THEN** the web session remains reachable and reports the input as unavailable
- **AND** capture retries the same configured backend and device using the existing retry behavior

#### Scenario: Unrelated startup error
- **WHEN** startup fails because of invalid STT credentials, invalid configuration, missing FFmpeg executable, or another prerequisite unrelated to input availability
- **THEN** that failure retains its existing error behavior rather than being relabeled as a missing input

### Requirement: Input availability is persistent source health
The stats response SHALL retain the configured source identity and expose current source availability, its failure reason, and whether recovery requires restart. While a live input fault is active, overall health SHALL be `degraded` regardless of STT being idle, connecting, reconnecting, connected, or paused. A closed session SHALL retain `closed` health. An input fault MUST NOT expire merely because time has passed, and audio silence alone MUST NOT be classified as a missing device.

#### Scenario: Fault persists through the degradation window and auto-pause
- **WHEN** the input remains unavailable for longer than 60 seconds and STT becomes paused
- **THEN** `/api/stats` continues reporting the active input fault and `health: degraded`

#### Scenario: Input becomes usable on an existing retry
- **WHEN** capture of the same configured device begins delivering PCM frames after an opening failure
- **THEN** the current input fault clears and source availability reports capture
- **AND** overall health returns to the existing health rules, including any remaining recent restart or unrelated degradation

#### Scenario: Working input supplies silence
- **WHEN** capture continues delivering silent PCM and STT pauses normally
- **THEN** the source is not reported missing and existing paused-health behavior is retained

### Requirement: Admin input fault is explicit and actionable
The existing admin health indicator SHALL display its degraded state for an active input fault. The Source card SHALL keep the selected device visible and show `Missing` for a validation rejection or `Unavailable` for an opening/capture failure, with its diagnostic reason. During the fault, the card SHALL provide visible troubleshooting guidance without a hover tooltip. It SHALL advise checking the cable/connection if the device is USB; otherwise, or if reconnecting does not resolve the problem, SSH into the appliance and follow `deploy/README.md` steps 5 and 6 to discover and configure the device.

#### Scenario: Operator inspects a missing-input card
- **WHEN** an operator views the Source card during an input fault
- **THEN** the configured device, missing/unavailable label, and troubleshooting guidance are visible without relying on color alone
- **AND** the card does not show a hover tooltip

#### Scenario: Fault clears
- **WHEN** capture resumes and the admin page receives an updated stats response
- **THEN** the current missing/unavailable label and failure-only guidance are removed
- **AND** the configured device remains visible; historical FFmpeg diagnostics do not imply an active missing-input fault

### Requirement: Recovery guidance matches supported recovery
The system SHALL reuse existing capture retries without adding configuration reload, automatic input switching, device rediscovery polling, or another recovery loop. Enumeration-rejected inputs SHALL remain blocked for the session and require server restart after correction. Retrying capture failures SHALL indicate that reconnecting the same configured input can recover automatically. All input configuration changes SHALL require server restart. Operator-facing restart guidance SHALL say "restart the server" without prescribing a command or restart mechanism.

#### Scenario: Enumeration rejection requires restart
- **WHEN** validation rejected the configured device at startup
- **THEN** the guidance says to correct the connection or device setup and "restart the server", without a shell command
- **AND** it does not promise automatic recovery merely from reconnecting the device

#### Scenario: Existing capture retries can recover
- **WHEN** input capture is retrying after an opening failure or runtime disconnect
- **THEN** the guidance says the same configured device is retried automatically
- **AND** it says configuration changes require a server restart

### Requirement: Input failures are logged for appliance diagnostics
The system SHALL emit warning-level logs for validation and capture availability failures containing the configured backend/device, a useful failure reason, and the recovery action (retry or restart required). Repeated retries SHALL use the existing bounded retry cadence rather than a tight logging loop.

#### Scenario: Headless appliance starts with an unavailable input
- **WHEN** device validation or capture opening fails
- **THEN** the service log identifies the selected backend/device, explains the failure, and states the recovery action without requiring debug logging
