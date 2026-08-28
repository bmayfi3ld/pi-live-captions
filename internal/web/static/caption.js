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
  // [{text, ms}], where ms is how long to sit on that word before the next one
  // lands, and null means "nobody measured it — pace by character".
  //
  // The wire shape is [{t, o}, ...]. o is the speaker's onset in ms from the
  // segment's own start, so a word's ms is just the next word's onset minus
  // its own. That gap covers the word AND any pause after it, which is the
  // whole point — see drain(), where the leftover after the roll becomes
  // dwell.
  //
  // Two sources of null, both honest rather than defensive:
  //   - The LAST word of a segment. Its gap would need the segment's end, and
  //     the wire carries no duration; the next segment's onsets restart at 0
  //     on their own clock, so they cannot supply it either.
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
        var at = (j === 0 && typeof seg[i].o === "number") ? seg[i].o : null;
        items.push({ text: parts[j], ms: null, at: at });
      }
    }
    for (var k = 0; k < items.length; k++) {
      var next = items[k + 1];
      if (items[k].at !== null && next && next.at !== null) items[k].ms = next.at - items[k].at;
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
  //
  // Returns { appendSegment, breakRow, retypeset } — see below.
  window.CaptionStack = function (opts) {
    var stack = opts.stack;
    var viewport = opts.viewport;
    var page = opts.page;
    var probe = opts.probe;

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
    // {text, ms} words from paceWords, {speaker} markers, and the BREAK
    // sentinel — and drain() tells them apart by their fields rather than by
    // type, which is why a word is an object and not a bare string. BREAK
    // must stay IN the queue (never jump ahead of already-queued words) or a
    // break could freeze a row before the words meant to land in it have.
    //
    // How long each tick waits is the pacing decision, and it is made from a
    // word's own ms — the gap the recognizer measured between this word's
    // onset and the next one's — so the display follows the speaker's real
    // rhythm rather than a constant rate. See drain().
    var pending = [];
    var drainTimer = null;
    var CHAR_MS = 1;   // per-character reveal delay (the roll's base speed)
    var MAX_HOLD_MS = 900; // ceiling on one word's dwell, so an outlier onset or a
                           // long silence inside a segment can't stall the display
    // CATCHUP_LEN is the backlog at which pacing is abandoned wholesale. In
    // steady state the queue holds well under a segment's worth of words,
    // because segments arrive at roughly the rate speech is spoken. It only
    // blows past this after a stall — a reconnect flushing several segments at
    // once, or a backgrounded tab waking up — where honoring the measured gaps
    // would mean replaying minutes-old prosody in front of a live speaker.
    // Past the threshold the queue drains flat-out until it clears.
    var CATCHUP_LEN = 40;
    var GLIDE_MS = 130; // MUST equal both the transition set in freezeAndGlide and
                        // the `transition: opacity` on .row in index.html —
                        // the serialization guarantee (never two glides in flight)
                        // depends on the scheduler waiting exactly as long as the
                        // glide animation takes.
    var BREAK = {};

    // ---- per-character reveal ----
    //
    // A word is measured, placed, and pushed to the ledger whole — layout is
    // decided before a single glyph is visible. The roll is purely cosmetic:
    // the span goes in empty and fills one character at a time. Because a row
    // only ever grows rightward from its last word, a partially filled span
    // shifts nothing that's already on screen.
    //
    // curCharMs is recomputed once per drain tick (not per character) so the
    // reveal and the queue's inter-word delay always agree on the current
    // speed. Per tick, not per word, is also what lets a timed word roll at
    // its own rate: drain() fits the roll inside that word's measured hold
    // before starting it.
    var curCharMs = CHAR_MS;
    var revealTimer = null;
    var revealSpan = null;
    var revealText = "";
    var revealAt = 0;

    // Finish whatever is mid-roll immediately. Called before starting the next
    // reveal — the queue's pacing should already guarantee the previous one
    // finished, but a glide's GLIDE_MS floor is the only thing enforcing it.
    function flushReveal() {
      if (revealTimer !== null) {
        clearTimeout(revealTimer);
        revealTimer = null;
      }
      if (revealSpan) revealSpan.textContent = revealText;
      revealSpan = null;
    }

    function revealStep() {
      revealTimer = null;
      revealAt++;
      revealSpan.textContent = revealText.slice(0, revealAt);
      if (revealAt < revealText.length) revealTimer = setTimeout(revealStep, curCharMs);
      else revealSpan = null;
    }

    function startReveal(span, text) {
      flushReveal();
      if (text.length < 2) return; // nothing to roll
      revealSpan = span;
      revealText = text;
      revealAt = 0;
      span.textContent = "";
      revealTimer = setTimeout(revealStep, curCharMs);
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

    // roll === true reveals the word one character at a time; rebuild() passes
    // false, because a reflow must repaint what's already on screen instantly.
    function placeInRow(row, entry, roll) {
      var isFirst = row.words.length === 0;
      if (row.words.length) row.el.appendChild(document.createTextNode(" "));
      var span = document.createElement("span");
      span.className = "w";
      span.textContent = entry.text;
      row.el.appendChild(span);
      if (roll) startReveal(span, entry.text);
      row.words.push(entry);
      // Badge decision happens exactly once per row, right here, so the live
      // path and rebuild() (which also funnels every placement through this
      // one function) can never disagree about where a badge lands.
      if (isFirst) applyBadge(row, entry.speaker);
    }

    // A row's first word decides whether its badge shows. Both callers
    // (pushWord and rebuild()'s placement loop) always call placeInRow with
    // the currently-active row, i.e. the last element of `rows` — so the
    // element just before it is unambiguously "the previous row".
    function applyBadge(row, speaker) {
      var prev = rows.length > 1 ? rows[rows.length - 2] : null;
      var prevSpeaker = (prev && prev.words.length) ? prev.words[0].speaker : 0;
      // Non-zero speaker, non-zero previous speaker, and they differ: this
      // is deliberately false for a session's first row (no previous row)
      // and for an all-one-speaker session (prevSpeaker never differs) —
      // both cases should carry no badge at all.
      if (speaker && prevSpeaker && speaker !== prevSpeaker) {
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
      if (row.words.length && row.words[0].speaker !== currentSpeaker) {
        freezeAndGlide();
        row = activeRow();
      }
      var cand = row.words.length ? rowText(row) + " " + text : text;
      if (measureWidth(cand) <= usableWidth) {
        var entry = { text: text, speaker: currentSpeaker };
        ledger.push(entry);
        placeInRow(row, entry, true);
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
          // ms: null on every chunk — a word's measured gap describes the
          // word, and subdividing it across glyph runs would be inventing
          // timing for something the speaker never said as separate words.
          var chunks = splitToChunks(text).map(function (c) { return { text: c, ms: null }; });
          // A single glyph wider than the whole row splits to itself
          // (splitToChunks' end<=i progress guard). Re-queueing that would
          // land back here next tick and loop forever, gliding each time —
          // so place it and let overflow:hidden clip it.
          if (chunks.length === 1) {
            var wide = { text: chunks[0].text, speaker: currentSpeaker };
            ledger.push(wide);
            placeInRow(activeRow(), wide, true);
            trimLedger();
            return;
          }
          Array.prototype.unshift.apply(pending, chunks);
          trimLedger();
          return;
        }
        var e2 = { text: text, speaker: currentSpeaker };
        ledger.push(e2);
        placeInRow(row, e2, true);
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

    // Rebuilds rows from the ledger from scratch, reveal suppressed. Used at
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
        var turnBreak = row.words.length > 0 && row.words[0].speaker !== entry.speaker;
        var cand = row.words.length ? rowText(row) + " " + entry.text : entry.text;
        var fits = row.words.length === 0 || (!turnBreak && measureWidth(cand) <= usableWidth);
        if (!fits) {
          row = makeEmptyRow();
          stack.appendChild(row.el);
          rows.push(row);
        }
        placeInRow(row, entry, false);
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
    function appendSegment(seg, speaker) {
      var words = paceWords(seg);
      // A speaker marker rides the same queue as the words it precedes, in
      // order — exactly like BREAK, it must never jump ahead of whatever's
      // already queued, or it would re-attribute words still waiting to be
      // painted from an earlier segment.
      pending.push({ speaker: speaker || 0 }); // absent/0 both mean "unknown"
      for (var j = 0; j < words.length; j++) pending.push(words[j]);
      scheduleDrain();
    }

    // scheduleDrain()/drain() pace the queue one item per tick. Because
    // drain() only ever schedules its own next call after the current item has
    // fully finished — and waits GLIDE_MS, not the word's roll time, whenever
    // that item caused a glide — at most one freezeAndGlide can ever be in
    // flight at a time. That invariant is the entire point of this queue.
    function scheduleDrain() {
      if (drainTimer !== null) return;
      if (pending.length === 0) return;
      // Near-immediate: the first word after a silence should not sit in the
      // queue waiting out a pacing tick nothing is pacing against.
      drainTimer = setTimeout(drain, 0);
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
        var chars = item.text.length + 1; // +1 for the space that follows
        // How long this word owns the screen. When the recognizer measured it
        // (ms), that IS the speaker's pacing; otherwise fall back to the
        // roll-time estimate. Capped so one outlier can't stall the stack.
        // Past CATCHUP_LEN the queue is far enough behind that the measured
        // prosody is stale, so it is abandoned and the backlog drains flat.
        //
        // No lower bound: a short word simply flashes past, which is what the
        // speaker actually did. A non-monotonic pair of onsets can make ms
        // negative — setTimeout clamps that to 0, so it costs a frame, not a
        // guard.
        var hold;
        if (pending.length > CATCHUP_LEN) hold = 0;
        else if (item.ms === null) hold = chars * CHAR_MS;
        else hold = Math.min(MAX_HOLD_MS, item.ms);
        // The roll fits inside the hold but never runs slower than the base
        // speed: a word followed by a long pause types at normal speed and
        // then the screen simply sits there. That leftover dwell is what makes
        // a pause read as a pause instead of as slow-motion typing.
        curCharMs = Math.min(CHAR_MS, hold / chars);
        var before = rows.length;
        pushWord(item);
        delay = (rows.length > before) ? Math.max(hold, GLIDE_MS) : hold;
      }
      if (pending.length > 0) drainTimer = setTimeout(drain, delay);
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
      scheduleDrain();
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
      retypeset: retypeset
    };
  };

  // Exposed for caption_pace_test.js: the gap math is the one piece of this
  // file that is pure and worth asserting, and it needs no DOM to run.
  window.CaptionStack.paceWords = paceWords;
})();
