# Changelog

The topmost version heading is the source of truth for the release version:
CI reads it, and publishes only when it moves forward. Cutting a release is
adding a new `## X.Y.Z` heading here.

## Unreleased

### Added
- Duration-based STT usage metrics: `/api/stats` now reports cumulative audio sent in minutes and hours, and the admin dashboard displays both totals alongside bytes sent.

### Changed
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
