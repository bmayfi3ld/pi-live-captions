// caption.js — the shared rolling-display typesetter.
//
// index.html (the viewer) and admin.html (the live-transcript mirror on the
// admin page) both mount this file rather than each carrying their own copy.
// That's not just DRY: the user requirement is that /admin renders
// identically to the viewer, and two copies of a typesetter drift the
// instant one of them gets a bugfix the other doesn't. One file, two mounts.
//
// One job: lay words out left-to-right into fixed-height rows, and glide the
// stack up by one row when the current row either runs out of width or the
// server reports the speaker actually paused. A word, once painted, is never
// touched again — the server only ever publishes committed, never-revised
// text (see internal/caption/hub.go), and there is no catch-up replay on the
// wire, so every word reaching this file arrives exactly once, live, and is
// painted through exactly one code path.
(function () {
  "use strict";

  function tokenize(text) {
    var t = String(text).replace(/^\s+|\s+$/g, "");
    return t ? t.split(/\s+/) : [];
  }

  // paceWords normalizes a caption event's Words into the queue's item shape:
  // [{text, dur, ms}], where dur is how long the speaker spent saying the word
  // and ms is the silence that followed it before the next one began. null on
  // either means "nobody measured it — pace by character".
  //
  // The wire shape is [{t, o, d}, ...]. o is the speaker's onset in ms from the
  // segment's own start and d is the word's spoken duration, so the silence
  // after a word is the next word's onset minus this word's own END, not its
  // onset. Onset-to-onset conflates the two, and drain() would then paint the
  // word instantly and sit out the entire time it took to say — a phantom
  // pause landing one word before the real one. Splitting them is what lets a
  // phrase spoken without a break come out without a break.
  //
  // Sources of null, all honest rather than defensive:
  //   - The LAST word of a segment has no ms: the next segment's onsets
  //     restart at 0 on their own clock and cannot supply the gap.
  //   - A word with no d has no dur: the provider reported no end for it.
  //   - An stt.Untimed segment, where the provider had no per-word detail and
  //     sent the entire segment as one wire word. Its `t` therefore contains
  //     spaces: it tokenizes into several display words, of which only the
  //     first can claim the onset. The rest are null, and the whole segment
  //     falls back to character pacing — which is exactly the truth.
  function paceWords(seg) {
    if (!seg) return [];
    var items = [];
    for (var i = 0; i < seg.length; i++) {
      var parts = tokenize(seg[i].t);
      for (var j = 0; j < parts.length; j++) {
        // Only a wire word's first token can carry its onset; a multi-token
        // one is the Untimed case, and inventing offsets inside it would be
        // fabricating prosody nobody measured.
        var timed = (j === 0 && typeof seg[i].o === "number");
        items.push({
          text: parts[j],
          dur: (timed && typeof seg[i].d === "number") ? seg[i].d : null,
          ms: null,
          at: timed ? seg[i].o : null
        });
      }
    }
    for (var k = 0; k < items.length; k++) {
      var next = items[k + 1];
      if (items[k].at !== null && next && next.at !== null) {
        // Never negative: providers do round word boundaries independently,
        // so an end can land a few ms past the next onset. That is a rounding
        // artifact, not the speaker talking backwards.
        items[k].ms = Math.max(0, next.at - items[k].at - (items[k].dur || 0));
      }
      delete items[k].at; // onsets were only ever a means to the gaps
    }
    return items;
  }

  // CaptionStack(opts) builds one independent rolling caption stack.
  //
  // opts:
  //   stack           the element rows are appended to (translated for the glide)
  //   viewport        the fixed-height clipping element around stack
  //   page            the element whose clientHeight/clientWidth bound layout
  //   probe           an offscreen element carrying the row's real font, used
  //                   to calibrate canvas text measurement against actual layout
  //   requestedLines  how many rows to show: a positive number, known at mount
  //                   time by both callers. It is a ceiling, not a promise —
  //                   adjustMaxRows lowers it to whatever actually fits the
  //                   page height.
  //   onPainted       optional function(publishedAt), called the moment a
  //                   segment's first word actually reaches the screen — see
  //                   appendSegment's third argument. Omitted by /admin, which
  //                   measures nothing.
  //
  // Returns { appendSegment, breakRow, pushEvent, retypeset } — see below.
  window.CaptionStack = function (opts) {
    var stack = opts.stack;
    var viewport = opts.viewport;
    var page = opts.page;
    var probe = opts.probe;
    var onPainted = opts.onPainted || null;

    var requestedLines = (typeof opts.requestedLines === "number" && opts.requestedLines > 0)
      ? opts.requestedLines : 3;

    // ledger is the append-only record of every word painted, and the only
    // input rebuild() has. Nothing in it is ever mutated once pushed — entries
    // only ever leave from the front, via trimLedger.
    var ledger = [];          // [{text, speaker}]
    var rows = [];            // [{el, words:[entry,...]}], only the last is open
    var LEDGER_CAP = 400;     // rebuild() only ever renders maxRows+1 rows; this is
                              // ample headroom for the widest plausible reflow

    // ---- paced emission queue ----
    //
    // appendSegment/breakRow must not call pushWord/freezeAndGlide directly
    // for every token in a segment: that gives bursty, all-at-once text
    // instead of a rolling feel, and — worse — freezeAndGlide sets a
    // fixed-duration CSS transition on #stack, so a second glide fired in the
    // same tick before the first painted a frame would just overwrite it, and
    // several rows would jump at once instead of gliding independently.
    //
    // pending decouples "when text arrives" from "when it's typeset": every
    // incoming word (and every break) is queued here and drained one item
    // per tick by drain(), below. Three item shapes share the queue —
    // {text, dur, ms} words from paceWords, {speaker} markers, and the BREAK
    // sentinel — and drain() tells them apart by their fields rather than by
    // type, which is why a word is an object and not a bare string. BREAK
    // must stay IN the queue (never jump ahead of already-queued words) or a
    // break could freeze a row before the words meant to land in it have.
    //
    // How long each tick waits is the pacing decision, and it is made from a
    // word's own dur and ms — how long the speaker spent saying it and the
    // silence that followed — so the display follows the speaker's real rhythm
    // rather than a constant rate. A word appears whole, at its onset: there is
    // no per-character reveal, because a caption is read, not watched, and
    // animating the glyphs of a word the reader has already recognized buys
    // motion at the cost of legibility. See drain().
    var pending = [];
    var drainTimer = null;
    var lastGlideAt = 0; // when the last glide started, so scheduleDrain never
                         // fires a second one on top of a decay glide
    var CHAR_MS = 1;   // stands in for a word's spoken length when the provider
                       // measured none, so an untimed segment lands in one go
    var MAX_HOLD_MS = 900; // ceiling on the SILENCE after one word, so an outlier
                           // onset or a long pause inside a segment can't stall
                           // the display. Not applied to the word's own spoken
                           // duration, which is bounded by its segment already.
    // RATE_MS is the backlog at which the display plays at 2x, and the reason
    // it can ever catch up at all.
    //
    // Replaying prosody at 1x costs exactly as much wall time as the speech it
    // describes, so a display running at 1x can never recover the recognizer's
    // latency — it can only add to it, since every row glide floors a tick at
    // GLIDE_MS and every setTimeout lands a few ms late. The result is drift
    // that grows without bound, which is what a fixed rate always gives you.
    //
    // So the rate rises with the backlog: rate = 1 + queued_ms / RATE_MS. That
    // is proportional rather than a threshold — a slight accelerando into each
    // segment easing back toward 1x as the queue empties, instead of pacing
    // normally until some cliff and then dumping the backlog flat. It is also
    // self-limiting: falling further behind speeds the display up, which is
    // what stops it falling further behind. Derived from queued MILLISECONDS,
    // not queued items: forty words can be three seconds of fast speech or
    // fifteen of slow, and only the latter should hurry.
    var RATE_MS = 3000;
    var GLIDE_MS = 100; // MUST equal both the transition set in freezeAndGlide and
                        // the `transition: opacity` on .row in index.html —
                        // the serialization guarantee (never two glides in flight)
                        // depends on the scheduler waiting exactly as long as the
                        // glide animation takes.
    var BREAK = {};
    // EVENT_SPEAKER stamps a non-speech marker (*music*, *silence*) with a
    // speaker id no diarizer ever emits, so the existing turn-break rule —
    // in pushWord AND, identically, in rebuild() — isolates the SPEECH around
    // it onto separate rows for free, live and after a resize reflow. That
    // rule alone can't separate two markers from each other (both carry
    // EVENT_SPEAKER), so both break sites carry a second term on .evt: a
    // marker always starts its own row. applyBadge's speaker > 0 guard keeps
    // it unbadged.
    var EVENT_SPEAKER = -1;

    // How much unplayed speech the queue is holding, in ms. Summed on each tick
    // rather than tracked as a running total: the queue is small (the rate
    // factor below is what keeps it that way) and a running total is one more
    // thing for every push and shift site to get wrong.
    //
    // ponytail: O(n) per tick over a queue the rate factor bounds at a few
    // dozen. If the queue ever needs to hold minutes, track the sum instead.
    function backlogMs() {
      var total = 0;
      for (var i = 0; i < pending.length; i++) {
        var it = pending[i];
        // Markers and BREAK carry no speech; a BREAK's own GLIDE_MS is real
        // wall time but is not the speaker's, and hurrying the queue on
        // account of it would be the display racing its own animation.
        if (it === BREAK || it.speaker !== undefined) continue;
        total += (it.dur || 0) + (it.ms || 0);
      }
      return total;
    }

    // currentSpeaker is the 1-based speaker id (0 = unknown) pushWord stamps
    // onto every ledger entry it creates. It's set from the paced queue by a
    // speaker-marker item (see drain()) rather than passed as an argument,
    // because appendSegment's words for a given segment are spread across
    // many future drain() ticks — the marker has to ride the same queue, in
    // the same order, so a word is never stamped with a speaker that hasn't
    // "arrived" yet from the queue's point of view.
    var currentSpeaker = 0;

    var ctx = document.createElement("canvas").getContext("2d");
    var rowH = 0;
    var usableWidth = 0;
    var fontScale = 1;
    var maxRows = requestedLines;

    function measureWidth(s) {
      return ctx.measureText(s).width * fontScale;
    }

    function activeRow() {
      return rows[rows.length - 1];
    }

    function rowText(row) {
      var parts = [];
      for (var i = 0; i < row.words.length; i++) parts.push(row.words[i].text);
      return parts.join(" ");
    }

    function placeInRow(row, entry) {
      var isFirst = row.words.length === 0;
      if (row.words.length) row.el.appendChild(document.createTextNode(" "));
      var span = document.createElement("span");
      span.className = entry.evt ? "w evt" : "w";
      span.textContent = entry.text;
      row.el.appendChild(span);
      row.words.push(entry);
      // Badge decision happens exactly once per row, right here, so the live
      // path and rebuild() (which also funnels every placement through this
      // one function) can never disagree about where a badge lands.
      if (isFirst) applyBadge(row, entry.speaker);
    }

    // A row's first word decides whether its badge shows. Both callers
    // (pushWord and rebuild()'s placement loop) always call placeInRow with
    // the currently-active row, i.e. the last element of `rows`, so the rows
    // before it are unambiguously the earlier ones.
    //
    // Scanned backwards past event rows rather than reading rows[len-2]
    // directly: a marker row (*music*, *silence*) carries EVENT_SPEAKER, and
    // taking it as "the previous speaker" would swallow the badge of a real
    // speaker change that happened across the marker. The previous SPEAKER is
    // the previous person who talked, not the previous line of the display.
    // Crossing a marker also FORCES a badge: the event row breaks the visual
    // thread of who is talking, so the first speech row after it re-asserts
    // the speaker even when it's the same person resuming.
    function applyBadge(row, speaker) {
      // Only marker rows are skipped — an empty or unknown-speaker row still
      // terminates the scan with prevSpeaker 0, exactly as before, so
      // diarization dropping out keeps suppressing the badge.
      var prevSpeaker = 0;
      var sawEvent = false;
      for (var i = rows.length - 2; i >= 0; i--) {
        var w = rows[i].words[0];
        if (w && w.evt) { sawEvent = true; continue; }
        prevSpeaker = w ? w.speaker : 0;
        break;
      }
      // Either an event row was crossed, or the speaker genuinely changed.
      // Absent an event this is deliberately false for a session's first row
      // (no previous row) and for an all-one-speaker session (prevSpeaker
      // never differs) — both cases should carry no badge at all.
      // > 0 rather than truthy: EVENT_SPEAKER is negative and must never
      // paint a speaker glyph or colour.
      if (speaker > 0 && (sawEvent || (prevSpeaker > 0 && speaker !== prevSpeaker))) {
        row.el.dataset.speaker = String(speaker);       // true number: the glyph
        row.el.dataset.spk = String(((speaker - 1) % 6) + 1); // cycled 1-6: the color
      }
    }

    // Row opacity by age is the only recency cue left once word color stops
    // carrying state (there is no more state to carry). This ladder is the
    // single definition — nothing in the CSS sets .row opacity, so there is
    // no second place for it to disagree with. Index is age: 0 is the live
    // row, 5 is the row on its way off the top (trimRows keeps maxRows + 1),
    // which is why the last step is 0 rather than something still visible.
    function applyRowOpacities() {
      var n = rows.length;
      var levels = [1, 0.3, 0.3, 0.3, 0.05, 0];
      for (var i = 0; i < n; i++) {
        var age = n - 1 - i;
        rows[i].el.style.opacity = String(age < levels.length ? levels[age] : 0);
      }
    }

    function makeEmptyRow() {
      var d = document.createElement("div");
      d.className = "row";
      return { el: d, words: [] };
    }

    function trimRows() {
      while (rows.length > maxRows + 1) {
        var old = rows.shift();
        if (old.el.parentNode) old.el.parentNode.removeChild(old.el);
      }
    }

    function trimLedger() {
      var over = ledger.length - LEDGER_CAP;
      if (over > 0) ledger.splice(0, over);
    }

    // A row is closed by exactly one of two triggers: the next word doesn't
    // fit (pushWord below), or the server told us the speaker actually
    // paused (breakRow, this function's public name). Skipping an empty row
    // means a break that lands with nothing typeset since the last glide —
    // two pause events in a row, or the very first segment of a session —
    // can never emit a blank row.
    function freezeAndGlide() {
      var row = activeRow();
      if (!row || row.words.length === 0) return;
      glide();
    }

    // glide is freezeAndGlide without the empty-row guard: one row of upward
    // motion, unconditionally. Only decayTick calls it directly — an idle
    // display has nothing left to freeze, and pushing blank rows is exactly
    // how the old text leaves the screen.
    function glide() {
      // Also trimmed here, not only from the transitionend handler below: a
      // tab that is backgrounded mid-glide may never deliver transitionend,
      // and rows would pile up in the DOM for as long as it stayed hidden.
      trimRows();

      var next = makeEmptyRow();
      stack.appendChild(next.el);
      rows.push(next);
      applyRowOpacities();

      stack.style.transition = "none";
      stack.style.transform = "translateY(" + rowH + "px)";
      void stack.offsetHeight; // force reflow before the transition kicks in
      stack.style.transition = "transform " + GLIDE_MS + "ms cubic-bezier(.22,.61,.36,1)";
      stack.style.transform = "translateY(0)";
      lastGlideAt = Date.now();
    }

    // ---- idle decay ----
    //
    // Left alone, the last thing said sits frozen on screen forever. Every
    // IDLE_MS with nothing arriving, decayTick pushes one blank row, and the
    // existing opacity ladder fades each row out as it ages off the top. One
    // row per idle window, not a rapid clear: the text a reader may still be
    // reading should drift off at reading pace. No new visual mechanism — a
    // decay tick is the same glide a row break is, just with nothing to freeze.
    var IDLE_MS = 10000;
    var idleTimer = null;

    function armIdle() {
      if (idleTimer !== null) clearTimeout(idleTimer);
      idleTimer = setTimeout(decayTick, IDLE_MS);
    }

    function decayTick() {
      idleTimer = null;
      for (var i = 0; i < rows.length; i++) {
        if (rows[i].words.length) {
          glide();
          armIdle();
          return;
        }
      }
      // Fully decayed. The ledger has to go with it: rebuild() replays it on
      // resize, so leaving it would let a rotation resurrect text the reader
      // just watched leave.
      ledger = [];
    }

    stack.addEventListener("transitionend", function (e) {
      if (e.target === stack && e.propertyName === "transform") trimRows();
    });

    // pushWord is the only way a single word reaches the screen. It takes a
    // queue item ({text, ms}) rather than a bare string so the hard-split
    // below can re-queue its chunks in the same shape everything else uses;
    // only .text matters to layout, ms having been consumed by drain() already.
    function pushWord(item) {
      var text = item.text;
      var row = activeRow();
      // Turn break: a new speaker never shares a row with the previous one,
      // even when there's width left for the word. Checked against the
      // row's first word (every word in a row is stamped with the same
      // speaker, by this same rule) rather than its last, and rebuild()'s
      // placement loop applies the identical condition — that's what lets a
      // resize reproduce every turn break without the server ever sending
      // an explicit break flag for a speaker change.
      // item.evt is the second trigger: a marker always starts its own row.
      // The speaker check alone can't do it — two markers in a row both carry
      // EVENT_SPEAKER, so they'd share a line whenever nothing between them
      // moved currentSpeaker (a speech segment that yielded no words).
      if (row.words.length && (item.evt || row.words[0].speaker !== currentSpeaker)) {
        freezeAndGlide();
        row = activeRow();
      }
      var cand = row.words.length ? rowText(row) + " " + text : text;
      if (measureWidth(cand) <= usableWidth) {
        var entry = { text: text, speaker: currentSpeaker, evt: item.evt };
        ledger.push(entry);
        placeInRow(row, entry);
      } else {
        freezeAndGlide(); // doesn't-fit trigger
        row = activeRow();
        if (measureWidth(text) > usableWidth) {
          // Rare case: a single "word" (URL, long compound) wider than the
          // row. Don't glide repeatedly in this same tick — split into chunks
          // and let each one take its own turn through the queue, exactly
          // like a word would. unshift so the chunks are the very next things
          // drained (in original order), ahead of whatever was already queued
          // behind this word.
          // dur and ms null on every chunk — a word's measured timing
          // describes the word, and subdividing it across glyph runs would be
          // inventing timing for something the speaker never said as separate
          // words. Both fields, not just ms: drain() reads dur too, and an
          // item missing it would make its hold NaN and stall the queue.
          var chunks = splitToChunks(text).map(function (c) {
            return { text: c, dur: null, ms: null, evt: item.evt };
          });
          // A single glyph wider than the whole row splits to itself
          // (splitToChunks' end<=i progress guard). Re-queueing that would
          // land back here next tick and loop forever, gliding each time —
          // so place it and let overflow:hidden clip it.
          if (chunks.length === 1) {
            var wide = { text: chunks[0].text, speaker: currentSpeaker, evt: item.evt };
            ledger.push(wide);
            placeInRow(activeRow(), wide);
            trimLedger();
            return;
          }
          // This branch paints nothing — it re-queues. Move any publish stamp
          // onto the first chunk so drain() reports when text actually lands,
          // and clear it here so drain() doesn't report for this pass.
          chunks[0].pub = item.pub;
          item.pub = null;
          Array.prototype.unshift.apply(pending, chunks);
          trimLedger();
          return;
        }
        var e2 = { text: text, speaker: currentSpeaker, evt: item.evt };
        ledger.push(e2);
        placeInRow(row, e2);
      }
      trimLedger();
    }

    // Pure computation, no DOM: returns the chunk strings word splits into so
    // each one fits usableWidth.
    function splitToChunks(word) {
      var chunks = [];
      var i = 0;
      while (i < word.length) {
        var end = i + 1;
        while (end <= word.length && measureWidth(word.slice(i, end)) <= usableWidth) end++;
        end = end - 1;
        if (end <= i) end = i + 1; // guarantee progress even for an oversize glyph
        chunks.push(word.slice(i, end));
        i = end;
      }
      return chunks;
    }

    // ---- measurement & layout ----

    function adjustMaxRows() {
      var rh = rowH || 1;
      var fitRows = Math.floor(page.clientHeight / rh) || 1;
      maxRows = Math.max(1, Math.min(requestedLines, fitRows));
      viewport.style.height = (maxRows * rowH) + "px";
    }

    function computeMetrics() {
      var cs = getComputedStyle(probe);
      ctx.font = cs.fontWeight + " " + cs.fontSize + " " + cs.fontFamily;
      var box = probe.getBoundingClientRect();
      var measuredH = Math.ceil(box.height);
      if (measuredH > 0) {
        rowH = measuredH;
        // Set on the document root, not a page-local element: every row's
        // CSS height/line-height reads this custom property, and there is
        // only ever one CaptionStack instance live per page, so there is no
        // cross-instance collision to guard against.
        document.documentElement.style.setProperty("--row-h", rowH + "px");
      }
      // Canvas resolves a font stack independently of layout — generic
      // families like ui-sans-serif in particular often don't match — so
      // calibrate against the real rendered probe. If canvas under-measures,
      // a row that "fits" would be clipped by overflow:hidden instead of
      // wrapping; the no-wrap invariant would survive but the reader would
      // lose a word.
      var canvasW = ctx.measureText(probe.textContent).width;
      fontScale = (box.width > 0 && canvasW > 0) ? box.width / canvasW : 1;
      // The speaker gutter is .row's own padding-left (see CSS), not page
      // padding, so it has to be subtracted here or a row would be measured
      // against the full page width and get clipped by .row's
      // overflow:hidden instead of wrapping one word sooner — the exact
      // failure mode fontScale calibration above exists to prevent.
      //
      // Measured on a throwaway row rather than parsed out of --gutter,
      // whose specified value is a calc() the browser won't resolve for a
      // custom property. It has to be a row of its own, not rows[0]: this
      // function runs once at construction BEFORE rebuild() has made any
      // row, and reading 0 there would leave usableWidth one gutter too
      // wide for the entire session — every row clipping its last word,
      // with no resize to correct it.
      var gutterRow = makeEmptyRow();
      stack.appendChild(gutterRow.el);
      var gutter = parseFloat(getComputedStyle(gutterRow.el).paddingLeft) || 0;
      stack.removeChild(gutterRow.el);
      usableWidth = Math.max(1, page.clientWidth - 1 - gutter);
      adjustMaxRows();
    }

    // Rebuilds rows from the ledger from scratch. Used at
    // construction and by retypeset(). Row structure after a resize is purely
    // a function of width and each entry's stored speaker, which is what makes
    // this correct without replaying any boundary metadata.
    function rebuild() {
      stack.style.transition = "none";
      stack.style.transform = "translateY(0)";
      stack.replaceChildren();
      rows = [];
      var row = makeEmptyRow();
      stack.appendChild(row.el);
      rows.push(row);

      for (var idx = 0; idx < ledger.length; idx++) {
        var entry = ledger[idx];
        row = activeRow();
        // Same turn-break condition as pushWord, replayed from the entry's
        // own stored speaker rather than a per-event break flag — that's
        // what makes a resize able to reproduce every turn break at all.
        var turnBreak = row.words.length > 0 && (entry.evt || row.words[0].speaker !== entry.speaker);
        var cand = row.words.length ? rowText(row) + " " + entry.text : entry.text;
        var fits = row.words.length === 0 || (!turnBreak && measureWidth(cand) <= usableWidth);
        if (!fits) {
          row = makeEmptyRow();
          stack.appendChild(row.el);
          rows.push(row);
        }
        placeInRow(row, entry);
      }
      trimRows();
      applyRowOpacities();
    }

    // ---- public surface ----
    // Three functions, matching the visual model exactly: text only ever
    // arrives (appendSegment) or a row only ever breaks early (breakRow);
    // retypeset is the one way the stack gets rebuilt rather than extended.

    // appendSegment is the ONLY way text enters. `seg` is one already-final
    // segment straight off the wire — a delta, never the accumulated
    // utterance — so there is nothing to diff against what's already painted:
    // normalize and queue.
    //
    // publishedAt is optional: the server's publish time for this segment, in
    // epoch ms. It rides the queue on the segment's FIRST word and comes back
    // out through onPainted when that word is painted, which is the only
    // honest end point for a publish->reader measurement — appendSegment
    // itself paints nothing, and the queue below can hold a word for seconds.
    // First word rather than last: the last would fold in however long the
    // speaker spent saying the segment, which is speech, not lag.
    function appendSegment(seg, speaker, publishedAt) {
      var words = paceWords(seg);
      // `> 0` rather than a typeof check: the caller's Date.parse yields NaN
      // for a missing or unparseable timestamp, and NaN is a number.
      if (publishedAt > 0 && words.length) words[0].pub = publishedAt;
      // A speaker marker rides the same queue as the words it precedes, in
      // order — exactly like BREAK, it must never jump ahead of whatever's
      // already queued, or it would re-attribute words still waiting to be
      // painted from an earlier segment.
      pending.push({ speaker: speaker || 0 }); // absent/0 both mean "unknown"
      for (var j = 0; j < words.length; j++) pending.push(words[j]);
      armIdle(); // anything arriving cancels a decay in progress
      scheduleDrain();
    }

    // scheduleDrain()/drain() pace the queue one item per tick. Because
    // drain() only ever schedules its own next call after the current item has
    // fully finished — and waits GLIDE_MS, not the word's own hold, whenever
    // that item caused a glide — at most one freezeAndGlide can ever be in
    // flight at a time. That invariant is the entire point of this queue.
    function scheduleDrain() {
      if (drainTimer !== null) return;
      if (pending.length === 0) return;
      // Near-immediate: the first word after a silence should not sit in the
      // queue waiting out a pacing tick nothing is pacing against. The one
      // thing it does wait for is a decay glide still in flight — two glides
      // at once is the exact failure this queue exists to prevent.
      drainTimer = setTimeout(drain, Math.max(0, GLIDE_MS - (Date.now() - lastGlideAt)));
    }

    function drain() {
      drainTimer = null;
      if (pending.length === 0) return;
      var item = pending.shift();
      if (item !== BREAK && item.speaker !== undefined) {
        // Speaker marker: unlike BREAK or a word, it paints nothing, so it
        // must not burn a pacing tick — consume it and drain whatever's
        // next immediately. Tested with !== undefined, not for truthiness:
        // speaker 0 ("unknown") is a real marker and must still apply.
        currentSpeaker = item.speaker;
        drain();
        return;
      }
      var delay;
      if (item === BREAK) {
        freezeAndGlide();
        delay = GLIDE_MS;
      } else {
        // A word owns the screen for two distinct spans, and they are kept
        // distinct because only one of them may be capped: `say` is the time
        // the speaker spent on the word, bounded by its own segment already,
        // and `gap` is the silence that followed, which one outlier onset
        // could otherwise stretch until the display stalls. Unmeasured falls
        // back to a character estimate, which lands an untimed segment in one
        // go rather than guessing at prosody nobody reported.
        var say = (item.dur === null) ? (item.text.length + 1) * CHAR_MS : item.dur;
        var gap = (item.ms === null) ? 0 : Math.min(MAX_HOLD_MS, item.ms);
        // Divided, not thresholded: see RATE_MS. The backlog is measured
        // AFTER the shift above, so it is what remains to be played, and this
        // word's own hold is not counted against itself.
        var hold = (say + gap) / (1 + backlogMs() / RATE_MS);
        var before = rows.length;
        pushWord(item);
        // pushWord clears item.pub if it re-queued the text instead of
        // painting it (the oversize split), so this only fires on a real paint.
        if (item.pub && onPainted) onPainted(item.pub);
        delay = (rows.length > before) ? Math.max(hold, GLIDE_MS) : hold;
      }
      if (pending.length > 0) drainTimer = setTimeout(drain, delay);
      else armIdle(); // queue fully drained: the idle countdown starts here,
                      // not when the segment arrived, since a long segment can
                      // take seconds to typeset
    }

    // breakRow is freezeAndGlide exposed under a name that says why it's
    // called: the server measured a real pause (hub.go's breakGap) and
    // wants the current row frozen where it is, even half-full, before the
    // next segment starts a clean row. freezeAndGlide's own empty-row guard
    // means a break landing on a fresh row — e.g. the very first segment of
    // a session — is silently ignored, which is exactly right.
    //
    // Queued, not fired immediately: BREAK must land after every word queued
    // ahead of it, so the row it freezes actually contains them.
    function breakRow() {
      pending.push(BREAK);
      armIdle(); // anything arriving cancels a decay in progress
      scheduleDrain();
    }

    // pushEvent puts a non-speech marker — "♪ music ♪", "— silence —" — into
    // the stream, the way broadcast captions do. It rides the same paced queue
    // as words and speaker markers so it can never jump ahead of text still
    // waiting to be typeset, and the .evt break term is what gives it its own
    // row (see EVENT_SPEAKER). No trailing speaker marker is needed: the next
    // appendSegment pushes its own, and even when that segment yields no words
    // at all, the next marker still breaks on .evt rather than sharing a line.
    //
    // dur/ms 0 rather than null: a marker is not speech, so it should land
    // immediately rather than get the character-count estimate an untimed word
    // falls back to.
    function pushEvent(text) {
      pending.push({ speaker: EVENT_SPEAKER });
      pending.push({ text: text, dur: 0, ms: 0, evt: true });
      armIdle(); // anything arriving cancels a decay in progress
      scheduleDrain();
    }

    // clear() wipes the screen on the operator's say-so: something is up there
    // that shouldn't be. All three stores have to go, and in this order:
    //
    //   - pending, or words already queued would repaint a second later;
    //   - ledger, or the next resize would let rebuild() resurrect the text
    //     the operator just removed;
    //   - the rows themselves, which rebuild() does for free — it already
    //     replaces the DOM, kills any in-flight glide transform and reapplies
    //     the opacity ladder.
    //
    // idleTimer is deliberately left running: a decay tick landing on an empty
    // stack finds no words and only clears an already-empty ledger.
    function clear() {
      pending = [];
      if (drainTimer !== null) { clearTimeout(drainTimer); drainTimer = null; }
      ledger = [];
      rebuild();
    }

    // retypeset() is the ONLY path that reflows already-painted text. It
    // runs on resize and rotation, never mid-stream: nothing in appendSegment
    // or breakRow calls it.
    function retypeset() {
      computeMetrics();
      rebuild();
    }

    computeMetrics();
    rebuild();

    return {
      appendSegment: appendSegment,
      breakRow: breakRow,
      pushEvent: pushEvent,
      clear: clear,
      retypeset: retypeset
    };
  };

  // Exposed for caption_pace_test.js: the gap math is the one piece of this
  // file that is pure and worth asserting, and it needs no DOM to run.
  window.CaptionStack.paceWords = paceWords;
})();
