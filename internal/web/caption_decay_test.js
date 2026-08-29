// caption_decay_test.js — the one runnable check behind idle decay.
//
//   node internal/web/caption_decay_test.js
//
// Decay is the only part of caption.js that paints without anything arriving
// on the wire, so it is the only part that can empty the screen by mistake —
// or fail to empty it at all. Both are worth an assertion.
//
// Two stubs and no framework: a DOM small enough for the typesetter to lay
// words out against (fixed 100px-wide rows, 20px tall), and a virtual clock
// replacing setTimeout so the 10s idle wait costs no wall time. Everything
// else is the real caption.js.

var assert = require("assert");

// ---- virtual clock ----
var now = 0;
var timers = [];
var nextId = 1;
global.setTimeout = function (fn, ms) {
  var id = nextId++;
  timers.push({ id: id, at: now + (ms || 0), fn: fn });
  return id;
};
global.clearTimeout = function (id) {
  timers = timers.filter(function (t) { return t.id !== id; });
};
// advance runs every timer due within ms, in time order, including ones the
// callbacks themselves schedule — which is the whole decay chain.
function advance(ms) {
  var until = now + ms;
  for (;;) {
    timers.sort(function (a, b) { return a.at - b.at; });
    if (!timers.length || timers[0].at > until) break;
    var t = timers.shift();
    now = t.at;
    t.fn();
  }
  now = until;
}
Date.now = function () { return now; };

// ---- DOM stub ----
var CHAR_W = 10; // every glyph is 10px wide, so "usableWidth 100" is 10 chars

function makeEl() {
  var el = {
    children: [],
    style: { setProperty: function () {} },
    dataset: {},
    className: "",
    textContent: "",
    parentNode: null,
    offsetHeight: 0,
    appendChild: function (c) { c.parentNode = el; el.children.push(c); return c; },
    removeChild: function (c) {
      el.children = el.children.filter(function (x) { return x !== c; });
      c.parentNode = null;
    },
    replaceChildren: function () { el.children = []; },
    addEventListener: function () {},
    getBoundingClientRect: function () {
      return { height: 20, width: (el.textContent || "").length * CHAR_W };
    },
    clientHeight: 200,
    clientWidth: 101 // 100 usable after computeMetrics' -1, gutter stubbed to 0
  };
  return el;
}

global.document = {
  documentElement: makeEl(),
  createTextNode: function () { return makeEl(); },
  createElement: function (tag) {
    if (tag === "canvas") {
      return {
        getContext: function () {
          return {
            font: "",
            measureText: function (s) { return { width: String(s).length * CHAR_W }; }
          };
        }
      };
    }
    return makeEl();
  }
};
global.getComputedStyle = function () {
  return { fontWeight: "400", fontSize: "20px", fontFamily: "serif", paddingLeft: "0px" };
};

global.window = {};
require("./static/caption.js");

var stack = makeEl();
var page = makeEl();
var probe = makeEl();
probe.textContent = "0123456789"; // 10 chars: canvas and layout agree, fontScale 1

var cs = window.CaptionStack({
  stack: stack, viewport: makeEl(), page: page, probe: probe, requestedLines: 3
});

function painted() {
  return stack.children.map(function (row) {
    return row.children.map(function (c) { return c.textContent; }).join("");
  }).filter(function (s) { return s.length; });
}

// Three short segments, each its own row (they are wider together than 10
// chars). Drained with the queue's own pacing, which the virtual clock skips.
cs.appendSegment([{ t: "alpha", o: 0, d: 100 }], 1);
cs.appendSegment([{ t: "bravo", o: 0, d: 100 }], 1);
cs.appendSegment([{ t: "charlie", o: 0, d: 100 }], 1);
advance(2000);
assert.deepStrictEqual(painted(), ["alpha", "bravo", "charlie"]);
console.log("ok  text paints before the idle window");

// Nine seconds of silence is not yet ten: nothing has moved.
advance(9000);
assert.deepStrictEqual(painted(), ["alpha", "bravo", "charlie"]);
console.log("ok  decay does not start early");

// The tenth second pushes the first blank row. trimRows keeps maxRows + 1, so
// that first tick costs no text yet — it slides everything one step down the
// opacity ladder. The second tick, ten seconds later, is what drops "alpha".
advance(1500);
assert.deepStrictEqual(painted(), ["alpha", "bravo", "charlie"]);
assert.strictEqual(stack.children.length, 4); // three rows of text + one blank
console.log("ok  decay pushes a row at 10s idle");

// Seven more seconds: still inside the same idle window, still one blank row.
// One row per window, not a cascade.
advance(7000);
assert.strictEqual(stack.children.length, 4);
console.log("ok  decay pushes one row per idle window, not a cascade");

// t≈21s: second blank row. t≈31s: the third pushes the stack past trimRows'
// maxRows + 1 and "alpha" finally leaves the DOM.
advance(10000);
assert.strictEqual(stack.children.length, 5);
advance(10000);
assert.deepStrictEqual(painted(), ["bravo", "charlie"]);
console.log("ok  the oldest row leaves once the blanks push it past the trim");

advance(40000);
assert.deepStrictEqual(painted(), []);
console.log("ok  decay empties the display");

// The ledger goes with it, or a resize would replay text the reader watched
// leave. retypeset() is rebuild()'s only public door, so this is the check.
cs.retypeset();
assert.deepStrictEqual(painted(), []);
console.log("ok  a resize after decay does not resurrect the text");

// Decay stops once empty — it must not sit re-arming a timer forever.
advance(60000);
assert.strictEqual(timers.length, 0);
console.log("ok  decay stops when there is nothing left");

// New speech after a full decay paints normally.
cs.appendSegment([{ t: "delta", o: 0, d: 100 }], 1);
advance(2000);
assert.deepStrictEqual(painted(), ["delta"]);
console.log("ok  speech after decay paints again");

// Speech arriving mid-decay cancels it: the row that is still on screen must
// survive rather than keep marching off under the new text.
advance(11000); // one decay tick has fired, "delta" is now one row up
cs.appendSegment([{ t: "echo", o: 0, d: 100 }], 1);
advance(9000);  // past when the next decay tick was due, short of a fresh one
assert.deepStrictEqual(painted(), ["delta", "echo"]);
console.log("ok  arriving speech cancels a decay in progress");

// The countdown belongs to the queue going quiet, not to the segment
// arriving — a slow segment still typesetting must not decay out from under
// itself. Not asserted here: drain()'s backlog speedup makes a drain longer
// than IDLE_MS need about a minute of queued speech, which is not a shape
// this harness can build honestly. The arming site in drain()'s tail is what
// covers it.

console.log("\nall decay checks passed");
