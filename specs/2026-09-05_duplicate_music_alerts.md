# bug duplicate music alerts and words slipping through

## issue
There is an issue where when music is playing that if there is ever a small quiet section or break in the music the status will go back to standard and then if the music starts up it goes back to music. The bigger issue with that is if during the silence any mumblings or audio slips through it might get intepreted as a random word, as a user I want some level of stability with the status, not flashing back and forth from music to green back to music.  But with this I don't want to lose the first few non-music words that come back after the music actually stops.

I suspect we need to keep the word caching that happens while music is playing, but instead of keeping everything only keep a few seconds, and send those words once we are confident there is no music.

To be confident there is no music, maybe we hold the music status for some hard coded (but easily adjustable in code) duration, (maybe 5 seconds) and don't switch to live until that is done.  Then if its done, we play only the last 5 seconds of words (similar to the existing system).

## Implementation plan

### Root cause and scope

- Speechmatics forwards every music start/end through `Config.OnMusic` to
  `Hub.SetMusic` (`internal/stt/speechmatics/speechmatics.go` and
  `internal/cli/run.go`).
- `internal/caption/hub.go` immediately sets music off on an end event and
  replays held words whose media timestamps follow that end. A short musical
  pause therefore opens the caption gate and releases possible garble.
- The viewer in `internal/web/static/index.html` already adds a music marker
  only on an off-to-on transition. Repeated markers are a consequence of real
  server transitions, not missing browser deduplication. The admin indicator
  consumes the same events.
- Fix the shared hub, not either browser: suppression must also protect the
  terminal and transcript writer. Keep the existing SSE format and provider
  callback signature; no new dependencies or CLI options.

### 1. Delay music-off, not music-on

Add two named, easily adjustable constants in `internal/caption/hub.go`, both
initially `5 * time.Second`: `musicEndHold` and `musicReplayWindow`. Keep them
separate because detector stability and retained speech are different knobs.

Keep `music` as the **effective suppression state**, including the hold, and
store a pending end's media offset plus its timer/token. Three logical states
are enough; no separate state-machine framework is needed:

| Current state | Input | Action |
| --- | --- | --- |
| Live | Music start | Suppress immediately, close the open transcript line, emit one music-on event. |
| Music | Positive music end | Start the hold; continue buffering and showing music. |
| Holding | Music start | Cancel/invalidate the pending end; stay suppressed without another marker. Discard the abandoned gap's held words. |
| Holding | Duplicate music end | Do not restart the timer or emit anything. |
| Holding | Hold expires | Emit one music-off event, then release only eligible buffered speech. |
| Live | Redundant positive music end | Ignore it. |
| Any | `SetMusic(false, at <= 0)` | Immediate connection reset: cancel pending work, drop old-clock words and offsets, and emit off only if previously suppressed. |

Repeated starts while already in Music are also no-ops. After an interrupted
hold, a subsequent end starts a full new hold from that end's arrival.

Use a one-shot standard-library timer, not a sleep inside `SetMusic`, polling,
or a check that only runs when another transcript arrives. The indicator must
leave music even if the room becomes completely silent. Measure the hold in
wall-clock time from receipt of the end event; use media timestamps only for
word filtering. An old media end timestamp must not shorten the hold.

### 2. Keep a short replay tail without losing boundary words

- `Publish` must continue holding settled segments throughout Music and
  Holding; nothing from this buffer reaches SSE, `OnFinal`, or caption counters
  before confirmation.
- Replace count-only retention with a rolling media-time window. Track the
  newest observed media position as the maximum of held transcript ends and
  music-edge offsets on the current connection. On append/end, remove words
  before `newestMedia - musicReplayWindow`, trimming a straddling segment
  rather than discarding its useful tail. Remove `maxHeldSegments` and its
  count-based trimming: the media-time window is the single retention rule.
- At confirmation, the replay cutoff is
  `max(pendingEndMedia, newestMedia - musicReplayWindow)`. Reuse `afterMusic`
  to retain words starting at or after that cutoff, preserving speaker IDs and
  recomputing segment start/duration and relative word offsets as it already
  does. Keep the existing all-or-nothing fallback for untimed segments.
- Release eligible segments once, in their original order, through the same
  caption/line-assembly logic used for normal publishing. Clear the held buffer.
  Retain the confirmed cutoff for late-arriving segments from this connection,
  so delayed pre-boundary finals cannot bypass filtering after the gate opens.
  Reset that cutoff on a new connection.
- A music restart during the hold invalidates that gap: discard its buffered
  words rather than replaying them after a later song ending. New words can
  then form the next rolling tail.

**Window semantics and limitation:** five seconds means recognized media time,
not five seconds since a packet arrived. With no new media timestamps, silence
alone does not age the buffer; the wall-clock timer still expires normally.
Words after the actual music boundary that remain within the tail are preserved,
including words received before the delayed end event. A strict five-second
window cannot guarantee retaining every first word if detector lag plus the
hold exceeds that window in media time. Start with the requested five seconds
and increase `musicReplayWindow` independently if real recordings show this
loss. Retention assumes normally advancing provider timestamps; a time window
is not a hard memory bound if timestamps stall or segment volume is excessive.
Document that ceiling with a `ponytail:` comment in the implementation; add a
defensive cap only if malformed-stream behavior becomes a real concern. This debounce also cannot identify garble
inside a gap that lasts longer than the hold; it is stability, not a second
speech/music classifier.

### 3. Make timer release safe across existing callers

Music callbacks run in the recognizer reader, whereas `Publish` runs in a
separate consumer of a buffered transcript channel in `internal/cli/run.go`.
Adding a timer introduces another publisher; the current unlock-then-replay
loop is not enough to guarantee ordering.

- Serialize the complete off-event/replay operation against `Publish`, music
  edges, `Clear`, and shutdown. A small hub-local operation mutex plus a private
  publish helper is sufficient; avoid recursively calling the public locking
  method. Keep `OnFinal` outside the state mutex.
- Stop the timer **and** invalidate its token on cancellation/reset. A callback
  already scheduled must check that it still owns the pending end before
  changing state. It must not reopen a restarted song or replay an old buffer.
- `Clear` drops held text without ending the music hold. Serializing it with
  release ensures a pending callback cannot restore text from before the wipe.
- `Subscribe` must snapshot effective music state under the lock and enqueue
  its initial state before later broadcasts can overtake it. Do not read
  `lastMusic` after unlocking as the current code does. Timer broadcasts must
  also be safe against unsubscribe closing a subscriber channel; keep the
  nonblocking sends protected against concurrent channel closure.
- Extend the shutdown-only `Hub.Flush` path to invalidate pending release and
  wait for any active release operation before closing the final line. Drop
  unconfirmed held words rather than treating shutdown as proof that music
  ended. `internal/cli/run.go` already calls this after the producer/consumer
  finish and before the writer closes; update lifecycle comments accordingly.

### 4. Regression coverage

Update `internal/caption/hub_test.go`, using standard-library `testing/synctest`
(the project targets Go 1.26) to advance time without real five-second sleeps.
Adapt existing immediate-off tests rather than deleting their assertions.

Cover these observable behaviors:

1. Start → end → restart before the deadline produces one on event, no off,
   no captions or transcript garble, and no stale callback release. A later
   end requires a complete new hold; redundant edges do not extend it.
2. A genuine end remains suppressed just before the deadline, then emits off
   before replay exactly once. Expiry also works with no incoming transcripts.
3. Preserve the existing delayed-end/straddling-segment regression: retain
   `please`, discard song words, and preserve speaker and zero-relative offset.
   Include speech received during the hold and a late final after release.
4. Retention follows timestamps rather than segment count: irregular durations,
   a segment crossing the cutoff, words exactly at the cutoff, untimed fallback,
   and long music with advancing timestamps retaining only the five-second
   tail. Replace the existing count-cap test; verify that more than 32 segments
   within the window are retained. Explicitly test the strict-window loss
   when the earliest returning speech falls outside the retained tail.
5. Reset during a pending hold drops old-clock words and cancels its callback;
   clear during a hold cannot restore cleared text; shutdown leaves no delayed
   caption or writer callback. Preserve the music-start open-line flush test.
6. A subscriber joining during the hold receives music-on state; competing
   publish/start/reset/clear/unsubscribe operations remain race-free and cannot
   interleave fresh captions ahead of the replay or send on a closed channel.

### 5. Verification and acceptance

After implementation, run:

```sh
go test ./internal/caption ./internal/stt/speechmatics
go test -race ./internal/caption
go build ./...
go test ./...
golangci-lint run ./...
```

User browser check, using an already-running instance with the fix: play music
with several gaps shorter than five seconds, then transition to speech. Both
viewer and admin should stay in music throughout the short gaps; the viewer
should add no duplicate music markers and show no gap garble. After the final
end plus the hold, replay the eligible speech tail once and resume normal
captions. Check the transcript as well, and repeat with a silent ending and
clear/reconnect during the hold.

Do not start the app or a built binary for verification. Browser behavior must
be checked by the user; Go tests can verify the server-side event sequence.

**Status:** plan only; no implementation changes yet.
