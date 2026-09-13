## Purpose

Let admin operators keep the live transcript visible while scrolling console diagnostics, with bottom docking enabled by default on phone-sized screens.

## ADDED Requirements

### Requirement: Phone-sized pages start with a pinned transcript
On each admin page load, the Live Transcript SHALL start pinned when the initial viewport width is at most 600 CSS pixels and SHALL start inline for larger widths. The selected mode SHALL remain unchanged across resizing and orientation changes for that page lifetime. Reloading SHALL restore the default for the new initial width, without saving the previous choice.

#### Scenario: Initial phone and desktop defaults
- **WHEN** the admin page loads at widths of 390, 600, or 601 CSS pixels
- **THEN** the transcript starts pinned at 390 and 600 pixels and inline at 601 pixels
- **AND** its toggle exposes the matching state

#### Scenario: Rotation does not change the selected mode
- **WHEN** the operator resizes or rotates the viewport across the 600-pixel boundary
- **THEN** the current pinned or inline mode is retained and the card fits the new viewport

#### Scenario: Reload resets a manual choice
- **WHEN** the operator changes the mode and reloads the admin page
- **THEN** the mode is selected from the initial viewport width again rather than the previous choice

### Requirement: Operators can toggle transcript docking
The Live Transcript SHALL provide a visibly labeled “Pin transcript” control in both modes. It SHALL expose its current state to assistive technology and support touch and keyboard operation with a visible focus indication. Enabling it SHALL dock the card to the viewport bottom immediately, regardless of page scroll position. Disabling it SHALL return the card to its existing inline location below the diagnostic grid. This local display control SHALL be usable regardless of whether password-protected operator controls are enabled and SHALL NOT change server state or another client's display.

#### Scenario: Pin and unpin from the card
- **WHEN** the operator enables “Pin transcript” and scrolls the console
- **THEN** the live transcript and its toggle remain docked at the viewport bottom
- **AND** disabling the toggle restores the inline card below the diagnostic grid

#### Scenario: Keyboard use without operator controls
- **WHEN** an operator opens an admin page with server-changing controls disabled and activates the focused pin control with the keyboard
- **THEN** the local docking mode changes and the control exposes its updated state
- **AND** server-changing controls remain disabled

### Requirement: Docking preserves access to the console
The pinned card SHALL fit within the viewport width, account for device safe areas, and retain readable captions, connection status, and its toggle. Console content, including its last diagnostic card and interactive controls, SHALL remain scrollable into view above the dock. At narrow phone widths down to 320 CSS pixels, the layout SHALL NOT require horizontal scrolling to use the dock or console cards. At short viewport heights, the dock SHALL remain bounded so the console is still usable and the toggle remains reachable.

#### Scenario: Scroll to the last console card
- **WHEN** the transcript is pinned and the operator scrolls to the bottom of the console
- **THEN** the last diagnostic card can be read and its controls reached above the dock rather than being permanently covered

#### Scenario: Narrow phone with a safe area
- **WHEN** the admin page is displayed at a 320-pixel-wide phone viewport with a bottom safe-area inset
- **THEN** transcript text, status, and toggle stay within the usable viewport without horizontal scrolling
- **AND** the dock keeps its controls clear of the safe-area inset

#### Scenario: Short landscape viewport
- **WHEN** the pinned page rotates into a short landscape viewport
- **THEN** the console retains a usable scrolling area and the dock's toggle stays reachable
- **AND** any dock content that cannot fit is reachable by scrolling within the bounded dock

### Requirement: Docking preserves live transcript continuity
Changing docking mode SHALL preserve the existing live caption stream, caption history, three-row display under normal viewport conditions, speaker rendering, music and silence markers, and connection-state indication. Layout changes SHALL reflow captions without clearing or duplicating them and SHALL NOT establish an additional caption stream solely for the pinned view.

#### Scenario: Toggle during live captions
- **WHEN** captions are arriving and the operator toggles docking repeatedly
- **THEN** captions continue in the same transcript without a reset, duplicated events, or a new stream connection caused by toggling
- **AND** text reflows to the available card width

#### Scenario: Disconnection while pinned
- **WHEN** the caption stream disconnects while the transcript is pinned
- **THEN** the existing disconnected indication and reconnect behavior continue
- **AND** the transcript remains pinned with its toggle usable
