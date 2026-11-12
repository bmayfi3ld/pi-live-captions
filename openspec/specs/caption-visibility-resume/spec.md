# caption-visibility-resume Specification

## Purpose
Ensure the public captions page and admin caption mirror return directly to live captions after screen-off or backgrounding, without replaying missed speech.

## Requirements

### Requirement: Hidden caption displays suspend streaming and discard stale content
Both the public captions page and admin caption mirror SHALL close their caption stream and cancel pending caption reconnect work when the document becomes hidden. They SHALL discard queued captions and already-displayed text, and SHALL NOT accept caption stream updates or initiate caption connections while hidden.

#### Scenario: Screen switched off with captions pending
- **WHEN** the document becomes hidden after the user switches off the phone screen while caption words are queued
- **THEN** the caption connection is closed and pending reconnect work is cancelled
- **AND** queued words and displayed text are discarded rather than retained for later playback

#### Scenario: Page initially loads in the background
- **WHEN** either caption page initializes while its document is hidden
- **THEN** it does not open a caption connection until the document becomes visible

#### Scenario: Delayed work runs while hidden
- **WHEN** a stream callback or previously scheduled reconnect callback runs while the document is hidden
- **THEN** it does not display captions, change live stream indicators, or open a caption connection

### Requirement: Returning to a caption display starts a fresh live session
Both caption displays SHALL establish a fresh caption connection when their document becomes visible again, regardless of the prior connection state. They SHALL remain free of pre-hide and missed captions, display only future captions from the fresh subscription, and remain blank until such captions arrive. The connection indicator SHALL indicate connecting until the replacement connection opens; current status and music state SHALL synchronize from the fresh subscription without adding historical markers.

#### Scenario: Old connection appeared healthy at screen-off
- **WHEN** the user restores visibility after a connection was open before hiding
- **THEN** the display uses a fresh connection rather than continuing the prior one
- **AND** missed speech is not played back or fast-forwarded

#### Scenario: Return during silence or music
- **WHEN** the document becomes visible while upstream recognition is paused or music is active
- **THEN** the fresh subscription restores the appropriate current indicator
- **AND** the caption area remains blank without inventing a silence or music marker from the snapshot

#### Scenario: Resize after returning
- **WHEN** the user resizes or rotates the display after returning to visibility
- **THEN** layout reflow does not restore text discarded at the visibility boundary

### Requirement: Obsolete connections cannot affect the resumed session
Callbacks from replaced or closed caption connections SHALL NOT display content, change current indicators, or schedule reconnects. Repeated hide/show cycles SHALL NOT accumulate active caption connections or reconnect timers.

#### Scenario: Buffered old callback arrives after resume
- **WHEN** an old connection delivers a message, open, or error callback after a replacement connection has been established
- **THEN** the callback has no effect on the resumed display or its connection lifecycle

#### Scenario: Repeated visibility changes with retry pending
- **WHEN** the user repeatedly hides and shows either caption page while reconnect work is pending
- **THEN** the visible page owns at most one current caption connection and one reconnect timer
- **AND** the hidden page owns neither

### Requirement: Foreground behavior remains compatible
While continuously visible, both caption displays SHALL preserve existing word pacing, caption ordering, and connection recovery behavior. The public viewer SHALL preserve its existing wake-lock behavior independently of caption stream suspension. Visibility changes SHALL NOT clear or alter server-side saved transcripts.

#### Scenario: Continuous foreground viewing
- **WHEN** captions arrive while the page remains visible
- **THEN** they follow existing paced rendering and normal connection recovery rather than being discarded by a new catch-up policy

#### Scenario: Hide and resume with wake support enabled
- **WHEN** the public viewer is hidden and then shown with wake support enabled
- **THEN** its existing wake release and reacquisition behavior remains in effect independently of the caption connection reset
- **AND** saved transcripts remain unaffected by the local display reset
