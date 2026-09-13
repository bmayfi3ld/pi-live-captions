## Why

Speech-to-text providers commonly bill by audio duration, while the admin STT card currently reports only bytes sent. Operators need minute and hour totals they can compare directly with provider usage and pricing.

## What Changes

- Add cumulative STT audio-sent totals expressed in minutes and hours alongside the existing byte total.
- Show both duration totals on the admin STT card and keep them updating with the existing stats refresh.
- Preserve the existing bytes-sent metric for diagnostics and compatibility.

## Capabilities

### New Capabilities
- `stt-usage-metrics`: Defines duration-based STT usage reporting in the stats API and admin dashboard.

### Modified Capabilities

None.

## Impact

- Affects the STT portion of the metrics snapshot and `/api/stats` response.
- Affects the STT card in `internal/web/static/admin.html`.
- Adds focused metrics/API coverage and a changelog entry.
- Does not change STT provider connections, audio encoding, or dependencies.
