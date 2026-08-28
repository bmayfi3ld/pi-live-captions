// caption.js — the shared rolling-display typesetter.
//
// index.html (the viewer) and admin.html (the live-transcript mirror on the
// admin page) both mount this file rather than each carrying their own copy.
// That's not just DRY: the user requirement is that /admin renders
// identically to the viewer, and two copies of a typesetter drift the
// instant one of them gets a bugfix the other doesn't. One file, two mounts.
//
// This used to also be where Deepgram's revisable hypotheses were diffed
// against what was already on screen — a word could change color, get
// yanked back out of a frozen row, or get retroactively re-tagged as
// contradicted if a later revision disagreed with it. All of that is gone.
// The server now only ever publishes committed, never-revised text (see
// internal/caption/hub.go), so this typesetter has exactly one job: lay
// words out left-to-right into fixed-height rows, and glide the stack up by
// one row when the current row either runs out of width or the server
// reports the speaker actually paused. A word, once painted, is never
// touched again.
(function () {
  "use strict";

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
  // Returns { appendSegment, breakRow, resetAll, retypeset } — see below.
  window.CaptionStack = function (opts) {
    var stack = opts.stack;
    var viewport = opts.viewport;
    var page = opts.page;
    var probe = opts.probe;

    var requestedLines = (typeof opts.requestedLines === "number" && opts.requestedLines > 0)
      ? opts.requestedLines : 3;

    // ledger is the append-only record of every word ever painted. Nothing
    // in it is ever mutated once pushed (contrast the old, larger entry
    // shape needed to support diffing incoming revisions against what was
    // already on screen and replaying utterance boundaries after a resize)
    // — entries only ever leave from the front, via trimLedger.
    var ledger = [];          // [{text, frozen, span, speaker}]
    var rows = [];            // [{el, words:[entry,...], frozen}], only the last is unfrozen
    var LEDGER_CAP = 4000;    // ~200 rows of headroom, mirrors the old 200-line cap

    // ---- paced emission queue ----
    //
    // appendSegment/breakRow used to call pushWord/freezeAndGlide directly,
    // synchronously, for every token in a settled segment. That caused two
    // problems: bursty, all-at-once text instead of a rolling feel, and —
    // worse — freezeAndGlide sets a fixed-duration CSS transition on #stack,
    // so a second glide fired in the same tick before the first painted a
    // frame would just overwrite it, and several rows would jump at once
    // instead of gliding independently.
    //
    // pending decouples "when text arrives" from "when it's typeset": every
    // incoming word (and every break) is queued here and drained one item
    // per tick by drain(), below. BREAK is a sentinel rather than e.g. a
    // string, so it can share the queue with plain word strings without
    // risk of colliding with real caption text; it must stay IN the queue
    // (never jump ahead of already-queued words) or a break could freeze a
    // row before the words meant to land in it actually have.
    var pending = [];
    var drainTimer = null;
    var CHAR_MS = 35;   // per-character reveal delay (the roll's base speed)
    var MIN_WORD_MS = 20; // floor so one-letter words don't flicker past; kept
                          // below one word's roll time so it never adds a stall
    var CHAR_MS_FLOOR = 5; // fastest the backlog scaling is allowed to push it
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
    // speed; letting them each sample the backlog independently would let the
    // reveal outrun or lag the word that follows it.
    var curCharMs = CHAR_MS;
    var revealTimer = null;
    var revealSpan = null;
    var revealText = "";
    var revealAt = 0;

    // Backlog-adaptive speed: this is the real anti-chop knob. A steady stream
    // rolls at CHAR_MS; a burst of segments types faster instead of drifting
    // further and further behind the audio.
    // ponytail: linear scaling, tuned by eye against live audio — swap for a
    // real latency target (queue length in seconds, not items) if it drifts.
    function charMs() {
      return Math.max(CHAR_MS_FLOOR, CHAR_MS / (1 + pending.length / 40));
    }

    function reducedMotion() {
      return !!(window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches);
    }

    // Finish whatever is mid-roll immediately. Called before starting the next
    // reveal (the queue's pacing should already guarantee the previous one
    // finished, but a glide's GLIDE_MS floor is the only thing enforcing it)
    // and on every path that tears the stack down.
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
      if (reducedMotion() || text.length < 2) return; // nothing to roll
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

    function tokenize(text) {
      var t = text.replace(/^\s+|\s+$/g, "");
      return t ? t.split(/\s+/) : [];
    }

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

    // animate === true rolls the word out one character at a time; every other
    // caller (rebuild, snapshot replay, hardSplit) paints it whole and
    // instantly — replay must never trickle.
    function placeInRow(row, entry, animate) {
      var isFirst = row.words.length === 0;
      if (row.words.length) row.el.appendChild(document.createTextNode(" "));
      var span = document.createElement("span");
      span.className = "w";
      span.textContent = entry.text;
      row.el.appendChild(span);
      if (animate) startReveal(span, entry.text);
      entry.span = span;
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
      return { el: d, words: [], frozen: false };
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
    function freezeAndGlide(animate) {
      var row = activeRow();
      if (!row || row.words.length === 0) return;
      row.frozen = true;
      for (var i = 0; i < row.words.length; i++) row.words[i].frozen = true;

      var next = makeEmptyRow();
      stack.appendChild(next.el);
      rows.push(next);
      applyRowOpacities();

      var reduced = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;
      if (animate && !reduced) {
        stack.style.transition = "none";
        stack.style.transform = "translateY(" + rowH + "px)";
        void stack.offsetHeight; // force reflow before the transition kicks in
        stack.style.transition = "transform " + GLIDE_MS + "ms cubic-bezier(.22,.61,.36,1)";
        stack.style.transform = "translateY(0)";
      } else {
        stack.style.transition = "none";
        stack.style.transform = "translateY(0)";
        trimRows();
      }
    }

    stack.addEventListener("transitionend", function (e) {
      if (e.target === stack && e.propertyName === "transform") trimRows();
    });

    // pushWord is the only way a single word reaches the screen.
    // appendSegment below calls it once per token.
    function pushWord(text, animate) {
      var row = activeRow();
      // Turn break: a new speaker never shares a row with the previous one,
      // even when there's width left for the word. Checked against the
      // row's first word (every word in a row is stamped with the same
      // speaker, by this same rule) rather than its last, and rebuild()'s
      // placement loop applies the identical condition — that's what lets a
      // resize reproduce every turn break without the server ever sending
      // an explicit break flag for a speaker change.
      if (row.words.length && row.words[0].speaker !== currentSpeaker) {
        freezeAndGlide(animate);
        row = activeRow();
      }
      var cand = row.words.length ? rowText(row) + " " + text : text;
      if (measureWidth(cand) <= usableWidth) {
        var entry = { text: text, frozen: false, span: null, speaker: currentSpeaker };
        ledger.push(entry);
        placeInRow(row, entry, animate);
      } else {
        freezeAndGlide(animate); // doesn't-fit trigger
        row = activeRow();
        if (measureWidth(text) > usableWidth) {
          if (animate) {
            // Paced path: don't glide repeatedly in this same tick — split
            // into chunks and let each one take its own turn through the
            // queue, exactly like a word would. unshift so the chunks are
            // the very next things drained (in original order), ahead of
            // whatever was already queued behind this word.
            var chunks = splitToChunks(text);
            // A single glyph wider than the whole row splits to itself
            // (splitToChunks' end<=i progress guard). Re-queueing that would
            // land back here next tick and loop forever, gliding each time —
            // so place it and let overflow:hidden clip it, which is what the
            // synchronous path effectively did too.
            if (chunks.length === 1) {
              var wide = { text: chunks[0], frozen: false, span: null, speaker: currentSpeaker };
              ledger.push(wide);
              placeInRow(activeRow(), wide, animate);
              trimLedger();
              return;
            }
            Array.prototype.unshift.apply(pending, chunks);
            trimLedger();
            return;
          }
          hardSplit(text, animate);
        } else {
          var e2 = { text: text, frozen: false, span: null, speaker: currentSpeaker };
          ledger.push(e2);
          placeInRow(row, e2, animate);
        }
      }
      trimLedger();
    }

    // Rare case: a single "word" (URL, long compound) wider than the row.
    // Pure computation, no DOM: returns the chunk strings word splits into
    // so each one fits usableWidth. Shared by the synchronous hardSplit
    // (below) and the paced path in pushWord, which paces the chunks
    // through the same queue as ordinary words instead of gliding them
    // all in one tick.
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

    // Synchronous-path split: place each chunk immediately, gliding between
    // them exactly as before. Only reached when animate === false.
    function hardSplit(word, animate) {
      var chunks = splitToChunks(word);
      for (var c = 0; c < chunks.length; c++) {
        var row = activeRow();
        var entry = { text: chunks[c], frozen: false, span: null, speaker: currentSpeaker };
        ledger.push(entry);
        placeInRow(row, entry);
        if (c < chunks.length - 1) freezeAndGlide(animate);
      }
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

    // Rebuilds rows from the ledger from scratch, animation suppressed. Used
    // at construction, after a snapshot, and by retypeset(). This is the
    // only place row DOM is created outside of pushWord/freezeAndGlide's
    // normal live-streaming path.
    //
    // There used to be a second trigger here — replaying, per word, where an
    // utterance had ended, so a resize wouldn't lose a pause break that
    // had already landed. That's gone: with words immutable and pause
    // breaks no longer recorded per-word, row structure after a resize is
    // purely a function of width, which is what makes this function correct
    // without replaying any boundary metadata.
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
        entry.span = null;
        entry.frozen = false;
        row = activeRow();
        // Same turn-break condition as pushWord, replayed from the entry's
        // own stored speaker rather than a per-event break flag — that's
        // what makes a resize able to reproduce every turn break at all.
        var turnBreak = row.words.length > 0 && row.words[0].speaker !== entry.speaker;
        var cand = row.words.length ? rowText(row) + " " + entry.text : entry.text;
        var fits = row.words.length === 0 || (!turnBreak && measureWidth(cand) <= usableWidth);
        if (!fits) {
          row.frozen = true;
          for (var k = 0; k < row.words.length; k++) row.words[k].frozen = true;
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
    // Four functions, matching the visual model exactly: text only ever
    // arrives (appendSegment) or a row only ever breaks early (breakRow);
    // resetAll and retypeset are the two ways the whole stack gets rebuilt
    // rather than extended.

    // appendSegment is the ONLY way text enters. `text` is one already-final
    // segment straight off the wire (caption.Event.Text on a "caption"
    // event) — a delta, never the accumulated utterance — so there is
    // nothing to diff against what's already painted: tokenize and push.
    function appendSegment(text, animate, speaker) {
      var spk = speaker || 0; // absent/0 both mean "unknown"
      var words = tokenize(text);
      if (animate === false) {
        // Snapshot/catch-up replay must never trickle: drop anything still
        // queued from before (it's about to be superseded by this replay
        // anyway) and paint synchronously, exactly as before this change.
        pending = [];
        clearTimeout(drainTimer);
        drainTimer = null;
        flushReveal();
        currentSpeaker = spk;
        for (var i = 0; i < words.length; i++) pushWord(words[i], false);
        return;
      }
      // A speaker marker rides the same queue as the words it precedes, in
      // order — exactly like BREAK, it must never jump ahead of whatever's
      // already queued, or it would re-attribute words still waiting to be
      // painted from an earlier segment.
      pending.push({ speaker: spk });
      for (var j = 0; j < words.length; j++) pending.push(words[j]);
      scheduleDrain();
    }

    // scheduleDrain()/drain() pace the queue one item per tick. Because
    // drain() only ever schedules its own next call after the current
    // item has fully finished — and waits GLIDE_MS, not the word's roll time, whenever
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
      if (item !== BREAK && typeof item === "object") {
        // Speaker marker: unlike BREAK or a word, it paints nothing, so it
        // must not burn a pacing tick — consume it and drain whatever's
        // next immediately.
        currentSpeaker = item.speaker;
        drain();
        return;
      }
      var delay;
      if (item === BREAK) {
        freezeAndGlide(true);
        delay = GLIDE_MS;
      } else {
        // Sampled once here, before the word is placed, so startReveal and the
        // delay below roll and wait at exactly the same speed.
        curCharMs = charMs();
        var before = rows.length;
        pushWord(item, true);
        // The word's own roll time (+1 for the space that follows it), floored
        // so short words still read as words rather than a flicker.
        var wordMs = Math.max(MIN_WORD_MS, (item.length + 1) * curCharMs);
        delay = (rows.length > before) ? Math.max(wordMs, GLIDE_MS) : wordMs;
      }
      if (pending.length > 0) drainTimer = setTimeout(drain, delay);
    }

    // breakRow is freezeAndGlide exposed under a name that says why it's
    // called: the server measured a real pause (hub.go's breakGap) and
    // wants the current row frozen where it is, even half-full, before the
    // next segment starts a clean row. freezeAndGlide's own empty-row guard
    // means a break landing on a fresh row — e.g. the very first segment of
    // a session — is silently ignored, which is exactly right.
    function breakRow(animate) {
      if (animate === false) {
        freezeAndGlide(false);
        return;
      }
      // Queued, not fired immediately: BREAK must land after every word
      // queued ahead of it, so the row it freezes actually contains them.
      pending.push(BREAK);
      scheduleDrain();
    }

    // Clears the ledger *and* the rows built from it. Both must go
    // together: EventSource reconnects on its own and every reconnect
    // delivers a fresh snapshot, so keeping the old rows would replay
    // history on top of itself.
    function resetAll() {
      // Queued words from before the drop must not paint on top of the
      // replayed history a reconnect is about to deliver.
      pending = [];
      clearTimeout(drainTimer);
      drainTimer = null;
      flushReveal();
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
      resetAll: resetAll,
      retypeset: retypeset
    };
  };
})();
