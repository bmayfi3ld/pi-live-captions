## Context

See `proposal.md` for motivation and `specs/stt-usage-metrics/spec.md` for behavior. STT upload accounting already increments `sttBytesSent` only after a provider audio send succeeds. All provider audio uses `audio.PipelineFormat`, whose existing `Duration` method converts PCM bytes to represented audio time. `Metrics.Snapshot` supplies both `/api/stats` and the admin page.

## Goals / Non-Goals

**Goals:**
- Derive billing-oriented duration totals from the existing successful-send byte counter.
- Keep one authoritative conversion in the server snapshot and expose explicit minute and hour JSON values.
- Add the two values to the existing STT card without changing its refresh flow.

**Non-Goals:**
- Reconcile local totals with provider-specific rounding, minimum charges, or invoices.
- Track usage across process restarts or split usage by provider connection.
- Change the pipeline audio format or STT send accounting.

## Decisions

1. **Derive duration when taking a metrics snapshot.** Convert `sttBytesSent` with `audio.PipelineFormat.Duration`, then publish `minutes_sent_total` and `hours_sent_total` as floating-point fields under `stt`. This reuses the format definition and keeps the duration values consistent with the byte total without adding mutable counters. Computing in browser JavaScript was rejected because it would duplicate the PCM format and make the API less useful to other metrics consumers.

2. **Keep the existing bytes field and add separate dashboard rows.** The STT card will continue to show `bytes_sent_total`, plus clearly labelled minute and hour totals formatted to a compact fixed precision. Replacing bytes was rejected because bytes remain useful for transport diagnostics and existing API consumers may rely on the field.

3. **Test conversion at the snapshot/API boundary.** A focused metrics test will record a known number of pipeline-format bytes and verify both duration fields and the unchanged byte total. Existing HTTP snapshot decoding coverage confirms those fields pass through `/api/stats`; extend it only if explicit JSON field-name coverage is needed.

## Risks / Trade-offs

- **Provider billing can differ due to provider rounding or non-audio charges** → Label these as audio-sent usage and document that they represent local pipeline duration, not an invoice.
- **Small totals round to zero in the dashboard** → Keep full numeric precision in `/api/stats`; presentation rounding affects display only.
- **Future pipeline format changes could invalidate hard-coded conversion** → Reuse `audio.PipelineFormat.Duration` rather than a literal byte rate.
