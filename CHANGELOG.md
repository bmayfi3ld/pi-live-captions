# Changelog

The topmost version heading is the source of truth for the release version:
CI reads it, and publishes only when it moves forward. Cutting a release is
adding a new `## X.Y.Z` heading here.

## 0.7.0

### Added

- Every recorded session now writes `audit.jsonl` beside `transcript.txt`: an append-only,
  one-JSON-object-per-line evidence file carrying the same finalized captions and markers as
  the transcript (with millisecond source timing), plus noise-gate open/close transitions
  with their source positions, runtime gate setting changes, recognizer connection states,
  reconnects, pauses, drops, source restarts, session warnings and errors, startup metadata
  (version, source, recognition and noise-gate configuration — no credentials) and a final
  metrics summary on clean shutdown. Best-effort by design: an audit write failure disables
  the audit stream, is reported once in the log and as degraded health on `/admin`, and
  never interrupts live captions or the transcript. See `docs/usage.md`, "Audit log".

### Changed

- Newly recorded transcripts compact adjacent identical music and silence markers.
- set default of the web page to show one less line while scrolling

### Fixed

- Transcript timestamps are now positions in the source audio for the whole session: silence
  pauses and reconnects no longer restart the clock at zero, and speech, silence and music
  markers share one nondecreasing timeline in newly recorded transcript files. Existing
  transcript files are unchanged.

## 0.6.0

### Fixed

- Speechmatics punctuation from removed profanity or disfluencies no longer creates duplicate or conflicting sentence marks in new transcripts.

## 0.5.0

### Added

- Duration-based STT usage metrics: `/api/stats` now reports cumulative audio sent in minutes and hours, and the admin dashboard displays both totals alongside bytes sent.

### Changed

- The admin live transcript starts pinned on pages initially 600 pixels wide or narrower; its inline “Pin transcript” toggle remains local to the current page.

- Caption audio gate defaults are now -50 dBFS with a 5-second release.

### Fixed

- Paused reconnect-buffer rotation no longer falsely reports dropped audio or degraded health when speech resumes.

## 0.4.0

### Changed

- Unavailable configured live inputs keep the web console reachable with persistent degraded health; `/admin` shows whether the source is missing and restart-required or unavailable and retrying, with visible guidance instead of a hover tooltip.

## 0.3.0

### Added

- Viewer `?bottom=N` setting positions the last caption row N% above the viewport bottom
  (0–90%, respecting device safe areas).
- Music and silence markers in the admin caption scroll and timestamped transcripts.
- a music delay to avoid back and forth jitter with music detection
- Caption audio gate with live admin threshold and release controls, input RMS and peak
  meters, and threshold markers in a dedicated card. Defaults to -35 dBFS and a 3-second
  release; startup values can be configured with `--noise-threshold-dbfs` and
  `--noise-release`. Live adjustments are session-only; listener audio is unchanged.

### Changed

- Viewer defaults to four caption rows instead of five; `?lines=N` still overrides it.
- Admin latency charts use a zero baseline and session-wide peak scales that never shrink,
  including after refresh; rolling latency statistics remain five-minute measurements.
- default accuracy/latency for speechmatics to 1.2 seconds
- enabled filler word filtering

### Fixed

- Reconnecting viewers and admin pages receive music-off state, clearing stuck music indicators.
- Reconnect state snapshots do not duplicate music or silence scroll markers.
- Admin chart axis labels have room to display larger, updating latency values.

## 0.2.3

- Restart a running livecaption service after a package upgrade.

## 0.2.2

- Label the apt repository as `stable` rather than `unknown` in apt output.

## 0.2.1

- Fix recurring non-monotonic DTS errors from ALSA capture by deriving live PCM and MP3 output
  timestamps from processed sample counts.

## 0.2.0

- Debian package, private apt repo, and a manual setup runbook (`deploy/`).
- Every flag can now be set as `LIVECAPTION_<FLAG>`, so the service is
  configured entirely from a systemd `EnvironmentFile`. A variable that is set
  but empty is treated as unset, so a half-edited config line no longer fails
  startup with a confusing "exists but is a directory".
- `--version` reports the real build version instead of always `0.1.0`.

## 0.1.0

- Initial working build: live and replay captioning, viewer and admin pages,
  mDNS advertisement, transcript recording.
