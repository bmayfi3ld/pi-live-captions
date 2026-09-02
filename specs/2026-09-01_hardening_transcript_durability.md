# Transcript hardening — durability on power loss, and behaviour when the disk fills

**Date:** 2026-09-01
**Severity:** Medium — silent partial loss of the product's only durable artifact
**Status:** OPEN, no code changed
**Code analyzed:** working tree at `40e5ecb` — `internal/caption/writer.go`,
`internal/metrics/metrics.go`, `internal/web/static/admin.html`

**Context:** the deployment target is a venue appliance that is moved between sites and has
its power pulled at the end of each event rather than shut down. Transcripts are the only
thing a session leaves behind; captions themselves are ephemeral. Two gaps make that
artifact less durable than it reads.

---

## Summary

| # | Finding | Impact |
|---|---|---|
| H1 | Writes are flushed but never `fsync`ed | Power loss costs ~30s of transcript, not the ~2s the code intends |
| H2 | Session directory is not synced after creation | Power loss soon after start can lose the file, not just its tail |
| H3 | `ENOSPC` degrades silently after 60s | A full disk is a persistent fault displayed as a transient one |
| H4 | No retention policy | Transcripts accumulate forever; no bound, no pruning |

H1 and H2 are the real defects. H3 is the one most likely to bite in the field. H4 is cheap
insurance — see the sizing note, which argues it is *not* the likely cause of a full disk.

---

## H1 — writes are never fsynced

`Writer` flushes its `bufio.Writer` every 2s but never calls `File.Sync()`. Flushing moves
bytes into the kernel page cache, not onto the disk. A process crash loses ~2s as intended;
an abrupt power loss loses whatever the kernel has not written back — typically up to ~30s
(`dirty_expire_centisecs`).

Evidence in `internal/caption/writer.go`:

- `:49` — `os.OpenFile(..., O_CREATE|O_WRONLY|O_APPEND, 0o644)`, no `O_SYNC`.
- `:57` — writes buffered through `bufio.NewWriter(txt)`.
- `:62-70` — 2s ticker calls `w.flush()`. The comment reads *"Periodic flush bounds how much
  a crash can cost to a couple of seconds."* True for a process crash; misleading for power
  loss, which is the failure mode this deployment actually has.
- `:110-118` — `flush()` calls `txtBuf.Flush()` and nothing else.
- `:121-136` — `Close()` calls `Flush()` then `txt.Close()`. `close(2)` does not imply
  durability.

**Fix:** add `Sync()` after `Flush()` in both `flush()` and `Close()`.

```go
if err := w.txtBuf.Flush(); err != nil {
	w.fail(err)
	return
}
// Venue power gets pulled without a shutdown; a page-cache write is not a
// transcript.
if err := w.txt.Sync(); err != nil {
	w.fail(err)
}
```

One fsync per 2s on a small append-only text file, against a workload already doing
continuous audio I/O. Cadence does not need to change.

## H2 — parent directory is not synced

`NewWriter` does `os.MkdirAll` (`:44`) and creates `transcript.txt` (`:49`) without syncing
the parent directory. On POSIX, the file's *directory entry* is separately durable from its
contents; power loss shortly after session start can lose the entry, costing the whole file
rather than its tail.

**Fix:** after creating the file, open the session directory and `Sync()` it once. One call,
at construction only, not on the hot path.

## H3 — a full disk degrades silently

Current behaviour on any write error is `w.fail(err)` (`writer.go:108`) →
`metrics.SetTranscriptError` (`metrics.go:402`), which stores the message and calls
`markDegradedLocked`. This is better than nothing — `/admin` has a `tr-error` element
(`admin.html:468`) and the health badge flips to "degraded" — but two problems:

1. **The warning expires while the fault persists.** `degradedWindow` is 60s
   (`metrics.go:199`), sized for transient events like a ring eviction. A full partition
   does not resolve itself, so after a minute the badge returns to healthy while every
   subsequent line is still being dropped. The operator's signal disappears exactly when it
   is most needed.
2. **Captions keep flowing.** Correct — the live service must not stop because recording
   failed — but it means the failure is invisible to anyone not watching `/admin`.

**Fix:**

- Make a transcript write failure **sticky**: hold degraded state until a write succeeds
  again, rather than for a fixed window. Either a separate persistent flag or a
  "degraded-until-cleared" variant of the existing mechanism.
- Distinguish `ENOSPC` from other write errors in the surfaced message — "transcript disk
  full" is actionable at a venue; "write error" is not.
- On `ENOSPC`, attempt the retention prune (H4) once, then retry the write. If it still
  fails, stay degraded and stop retrying every line to avoid hammering a full disk.
- Consider a preflight free-space check at session start, surfaced in the startup banner
  alongside the other `ui.BannerField` rows, so a doomed session is visible before the event
  begins rather than during it.

## H4 — no retention policy

Nothing ever removes old session directories under `--transcript-dir`. Growth is unbounded.

**Sizing note, which should temper the design.** A line is roughly 80 bytes
(`writer.go:100`, `"[%s] %s%s\n"`). At ~20 lines/minute that is ~100 KB/hour, so a 4-hour
event costs well under half a megabyte.

The target box (Chromebox CN60, 16 GB M.2) has **~10 GB free after a Debian + Xfce
install**. At the rate above that is on the order of 100,000 hours of captioned audio —
tens of thousands of events. **Transcripts cannot fill this disk.** The realistic causes of
a full partition are journald, the apt package cache, or an OS upgrade — none of which
retention policy in this codebase can do anything about.

Two consequences:

- Retention is worth having as a tidiness bound, not as the fix for H3. **H3's graceful
  degradation is the requirement; H4 is the nice-to-have.** Do not let rotation logic become
  the reason the disk-full path is considered handled.
- Because the appliance's transcripts are small and valuable, retention should be generous
  and count-based, not aggressive.

**Proposed design — deliberately minimal:**

- Prune once at session start, in `NewWriter`, before the new directory is created. No
  background goroutine, no concurrency, and it guarantees headroom for the session about to
  begin.
- Keep the newest N session directories, N from a `--transcript-keep` flag, default
  generous (100 sessions ≈ years of events, still tens of MB).
- Sort by directory name — the `2006-01-02T15-04-05` format sorts lexicographically in
  timestamp order, so no `stat` calls are needed.
- **Never delete the current session's directory.**
- Log each removal at info level. Deleting a customer's records silently is worse than
  keeping them.

Explicitly out of scope unless someone asks: byte-budget quotas, compression of old
transcripts, upload/archival to a remote, or a background reaper. All are speculative until
the count-based bound proves insufficient.

## Test

- H1/H2: extend `internal/caption` tests — write a line, run one flush cycle, assert the
  bytes are readable via a separate `os.Open`. That covers buffering; true durability cannot
  be unit-tested without power control. Verify by hand on the venue box: speak a line, pull
  the plug, confirm the tail survived.
- H3: inject a write error (a `Writer` over a full `tmpfs`, or a fake `*os.File` wrapper)
  and assert the degraded state is still set well after `degradedWindow` has elapsed.
- H4: create N+3 dated directories in a temp dir, construct a `Writer`, assert the oldest
  are gone, the newest N remain, and the new session's own directory is untouched.

## Notes

- None of this is distro- or filesystem-specific.
- A UPS addresses the root cause of H1/H2; these fixes bound the damage when there isn't one
  or it fails. Both are worth having.
- If the appliance later moves to a read-only root with an overlay, the transcript directory
  needs its own writable partition — at which point H3's free-space handling stops being
  theoretical, since that partition will be small.
- **Out of scope for this codebase, but the actual mitigation for a 16 GB disk:** cap
  journald (`SystemMaxUse=200M` in `/etc/systemd/journald.conf`) and keep the apt cache
  clear. Those are the things that will fill this partition, and they are host configuration,
  not application changes. Record them in the box's build runbook, not here.
