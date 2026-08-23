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
  //   requestedLines  how many rows to show: a plain number, or a zero-arg
  //                   function returning one. A function lets a page raise
  //                   this after mounting — index.html's /api/config fetch
  //                   can arrive after the stack already exists — without
  //                   caption.js having to expose a setter as a fifth public
  //                   method.
  //
  // Returns { appendSegment, breakRow, resetAll, retypeset } — see below.
  window.CaptionStack = function (opts) {
    var stack = opts.stack;
    var viewport = opts.viewport;
    var page = opts.page;
    var probe = opts.probe;

    function requestedLines() {
      var v = typeof opts.requestedLines === "function" ? opts.requestedLines() : opts.requestedLines;
      return (typeof v === "number" && v > 0) ? v : 3;
    }

    // ledger is the append-only record of every word ever painted. Nothing
    // in it is ever mutated once pushed (contrast the old, larger entry
    // shape needed to support diffing incoming revisions against what was
    // already on screen and replaying utterance boundaries after a resize)
    // — entries only ever leave from the front, via trimLedger.
    var ledger = [];          // [{text, frozen, span}]
    var rows = [];            // [{el, words:[entry,...], frozen}], only the last is unfrozen
    var LEDGER_CAP = 4000;    // ~200 rows of headroom, mirrors the old 200-line cap

    var ctx = document.createElement("canvas").getContext("2d");
    var rowH = 0;
    var usableWidth = 0;
    var fontScale = 1;
    var maxRows = requestedLines();

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

    function placeInRow(row, entry) {
      if (row.words.length) row.el.appendChild(document.createTextNode(" "));
      var span = document.createElement("span");
      span.className = "w";
      span.textContent = entry.text;
      row.el.appendChild(span);
      entry.span = span;
      row.words.push(entry);
    }

    // Row opacity by age is the only recency cue left once word color stops
    // carrying state (there is no more state to carry).
    function applyRowOpacities() {
      var n = rows.length;
      var levels = [1, 0.85, 0.7];
      for (var i = 0; i < n; i++) {
        var age = n - 1 - i;
        var op = age < levels.length ? levels[age] : 0.55;
        rows[i].el.style.opacity = String(op);
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
        stack.style.transition = "transform 200ms cubic-bezier(.22,.61,.36,1)";
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
      var cand = row.words.length ? rowText(row) + " " + text : text;
      if (measureWidth(cand) <= usableWidth) {
        var entry = { text: text, frozen: false, span: null };
        ledger.push(entry);
        placeInRow(row, entry);
      } else {
        freezeAndGlide(animate); // doesn't-fit trigger
        row = activeRow();
        if (measureWidth(text) > usableWidth) {
          hardSplit(text, animate);
        } else {
          var e2 = { text: text, frozen: false, span: null };
          ledger.push(e2);
          placeInRow(row, e2);
        }
      }
      trimLedger();
    }

    // Rare case: a single "word" (URL, long compound) wider than the row.
    // Split it into chunks that fit; each full chunk freezes its own row.
    function hardSplit(word, animate) {
      var i = 0;
      while (i < word.length) {
        var row = activeRow();
        var end = i + 1;
        while (end <= word.length && measureWidth(word.slice(i, end)) <= usableWidth) end++;
        end = end - 1;
        if (end <= i) end = i + 1; // guarantee progress even for an oversize glyph
        var chunk = word.slice(i, end);
        var entry = { text: chunk, frozen: false, span: null };
        ledger.push(entry);
        placeInRow(row, entry);
        i = end;
        if (i < word.length) freezeAndGlide(animate);
      }
    }

    // ---- measurement & layout ----

    function adjustMaxRows() {
      var rh = rowH || 1;
      var fitRows = Math.floor(page.clientHeight / rh) || 1;
      maxRows = Math.max(1, Math.min(requestedLines(), fitRows));
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
      usableWidth = Math.max(1, page.clientWidth - 1);
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
        var cand = row.words.length ? rowText(row) + " " + entry.text : entry.text;
        var fits = row.words.length === 0 || measureWidth(cand) <= usableWidth;
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
    function appendSegment(text, animate) {
      var words = tokenize(text);
      for (var i = 0; i < words.length; i++) pushWord(words[i], animate);
    }

    // breakRow is freezeAndGlide exposed under a name that says why it's
    // called: the server measured a real pause (hub.go's breakGap) and
    // wants the current row frozen where it is, even half-full, before the
    // next segment starts a clean row. freezeAndGlide's own empty-row guard
    // means a break landing on a fresh row — e.g. the very first segment of
    // a session — is silently ignored, which is exactly right.
    function breakRow(animate) {
      freezeAndGlide(animate);
    }

    // Clears the ledger *and* the rows built from it. Both must go
    // together: EventSource reconnects on its own and every reconnect
    // delivers a fresh snapshot, so keeping the old rows would replay
    // history on top of itself.
    function resetAll() {
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
