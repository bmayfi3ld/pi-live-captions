# Spec Delta

## Purpose

Bound historical session storage through a predictable startup-only cleanup policy that removes whole session bundles without requiring knowledge of their transcript, audit, or audio contents.

## ADDED Requirements

### Requirement: Session storage has a configurable budget
The system SHALL provide `--transcript-max-storage` and `LIVECAPTION_TRANSCRIPT_MAX_STORAGE` with a default of `50GB`, meaning 50,000,000,000 bytes. The budget SHALL be a positive byte quantity. Invalid, nonpositive, or overflowing values MUST fail configuration validation before any deletion occurs.

#### Scenario: Default budget
- **WHEN** no storage budget override is provided
- **THEN** retention uses a limit of 50,000,000,000 bytes

#### Scenario: Operator overrides the budget
- **WHEN** a valid positive budget is configured by flag or environment variable
- **THEN** startup retention uses that budget with normal flag-over-environment precedence

#### Scenario: Invalid budget
- **WHEN** the budget is zero, negative, malformed, or too large to represent
- **THEN** configuration is rejected before scanning for deletion or creating a new session directory

### Requirement: Retention runs before creating a persisted session
When transcript persistence is enabled, the system SHALL measure stored bytes and complete any required cleanup before creating the new session directory. At or above the budget, it SHALL remove eligible directories oldest first until usage is strictly below the limit. Below the budget, it MUST NOT delete anything.

#### Scenario: Storage exceeds the limit
- **WHEN** existing storage totals 70 bytes, the budget is 50 bytes, and the oldest directories contain 15 and 10 bytes
- **THEN** those directories are removed in that order, leaving 45 bytes before the new session directory is created

#### Scenario: Storage equals the limit
- **WHEN** existing usage equals the configured budget
- **THEN** cleanup removes oldest directories until usage is strictly below the budget

#### Scenario: Storage is already below the limit
- **WHEN** existing usage is below the budget
- **THEN** no existing directories are removed

#### Scenario: First persisted session
- **WHEN** the configured transcript root does not exist
- **THEN** existing usage is treated as zero and the new session directory can be created normally

### Requirement: Cleanup treats directories as opaque bundles
Every immediate real child directory of the transcript root SHALL be eligible without inspecting filenames or parsing contents. Oldest SHALL mean directory modification time ascending, with directory name ascending as a deterministic tie-breaker. Deletion SHALL remove an entire selected directory and all its contents, including legacy or unknown files.

#### Scenario: Old directory has no audio or audit stream
- **WHEN** the oldest eligible directory contains only a legacy transcript
- **THEN** it is removed before newer eligible directories without special legacy detection

#### Scenario: Bundle contains several artifacts
- **WHEN** a selected directory contains text, JSON debugging data, audio, and other files
- **THEN** cleanup removes the entire directory rather than deleting individual artifact types

#### Scenario: Directories have identical modification times
- **WHEN** eligible directories have identical modification times
- **THEN** their names determine deletion order consistently

### Requirement: Accounting and deletion stay within the transcript root
Usage SHALL be the sum of logical regular-file sizes under the transcript root, including nested bundle files and loose root files. Cleanup MUST NOT follow symlinks or select loose root files or symlinked directories for deletion. Loose files count toward usage but only immediate real child directories are eligible bundles.

#### Scenario: Symlink points outside the root
- **WHEN** the transcript root or a bundle contains a symlink to external data
- **THEN** retention does not traverse or count the linked target
- **AND** deleting a bundle containing that symlink does not delete the external target

#### Scenario: Loose files consume the budget
- **WHEN** undeletable loose root files prevent usage from falling below the budget after all eligible directories are removed
- **THEN** retention reports that it cannot satisfy the configured budget rather than deleting loose files or external data

### Requirement: No retention runs during a session
The system SHALL enforce the budget only during persisted-session startup. It MUST NOT periodically scan or delete bundles while that session runs, and it MUST NOT truncate or stop an active recording merely because the budget is exceeded. A session using `--no-transcript` SHALL skip retention entirely.

#### Scenario: Active session grows beyond the budget
- **WHEN** storage grows above the configured limit during a running session
- **THEN** recording continues without retention scans or deletion
- **AND** the next persisted session performs cleanup before creating its directory

#### Scenario: Audio recording is disabled but transcripts remain enabled
- **WHEN** the session runs with `--no-audio-recording` and transcript persistence enabled
- **THEN** the same startup retention policy still applies

#### Scenario: Transcript persistence is disabled
- **WHEN** the session runs with `--no-transcript`
- **THEN** retention performs no storage scan or deletion

### Requirement: Cleanup failures are explicit and bounded
If usage cannot be measured safely, a selected directory cannot be removed, or the budget cannot be satisfied, startup SHALL fail before creating the new session directory or starting capture. The system SHALL report the failing path and reason and MUST NOT skip the failure by deleting newer directories. Successful cleanup SHALL report reclaimed bytes and removed directory count.

#### Scenario: Directory removal fails
- **WHEN** cleanup cannot remove the oldest selected directory
- **THEN** startup reports the failure without attempting newer directories or creating a new session directory

#### Scenario: Storage measurement fails
- **WHEN** a read or permission error prevents reliable accounting
- **THEN** startup stops before deleting any bundles

#### Scenario: Cleanup succeeds
- **WHEN** cleanup removes historical bundles and reaches usage below the limit
- **THEN** diagnostics report the removed directory count and reclaimed logical bytes
- **AND** session creation proceeds
