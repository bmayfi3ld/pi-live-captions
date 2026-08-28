// caption_pace_test.js — the one runnable check behind caption.js's gap math.
//
//   node internal/web/caption_pace_test.js
//
// No framework and no package.json: caption.js is an IIFE that only touches
// `window` at load time (document is reached inside the CaptionStack factory,
// which this never calls), so a bare object is the whole harness.
//
// It lives beside static/ rather than in it because //go:embed static ships
// every file in that directory into the binary, and a test is not an asset.
//
// Scope is paceWords only — turning a caption event's Words into per-word
// holds. The clamps and the catch-up threshold live in drain(), which needs a
// DOM; they are asserted here only as the raw gaps they are applied to.

var assert = require("assert");

global.window = {};
require("./static/caption.js");
var paceWords = window.CaptionStack.paceWords;

function holds(items) {
  return items.map(function (i) { return i.ms; });
}
function durs(items) {
  return items.map(function (i) { return i.dur; });
}
function texts(items) {
  return items.map(function (i) { return i.text; });
}

// A caption event's words: each hold is the SILENCE after the word — the next
// word's onset minus this word's end — so the pause the speaker actually took
// between "two" and "three" survives while the time they spent saying "two"
// does not masquerade as one.
var timed = paceWords([
  { t: "one", o: 0, d: 200 },
  { t: "two", o: 300, d: 400 },
  { t: "three", o: 800, d: 300 }
]);
assert.deepStrictEqual(texts(timed), ["one", "two", "three"]);
assert.deepStrictEqual(durs(timed), [200, 400, 300]);
// one ends at 200, two starts at 300 -> 100ms of silence.
// two ends at 700, three starts at 800 -> 100ms, NOT the 500ms onset gap.
// The last word has no successor onset, so null.
assert.deepStrictEqual(holds(timed), [100, 100, null]);
console.log("ok  silence from onsets and durations");

// The regression this split exists for: a long word followed immediately by
// the next one. Onset-to-onset would report a 480ms hold and paint it as a
// pause after "consecrate"; the silence is actually zero.
var fluent = paceWords([
  { t: "consecrate", o: 0, d: 480 },
  { t: "themselves", o: 480, d: 560 }
]);
assert.deepStrictEqual(holds(fluent), [0, null]);
console.log("ok  a long word is not a pause");

// Rounding can put a word's end a hair past the next word's onset. That is an
// artifact of two independently rounded boundaries, not negative silence.
var overlap = paceWords([{ t: "a", o: 0, d: 260 }, { t: "b", o: 240, d: 100 }]);
assert.deepStrictEqual(holds(overlap), [0, null]);
console.log("ok  overlapping boundaries clamp to zero");

// No d on the wire (provider reported no end): dur is null and drain() falls
// back to its character estimate, with the whole onset gap left as silence.
var noDur = paceWords([{ t: "one", o: 0 }, { t: "two", o: 300 }]);
assert.deepStrictEqual(durs(noDur), [null, null]);
assert.deepStrictEqual(holds(noDur), [300, null]);
console.log("ok  missing duration falls back to the onset gap");

// An event with no words at all. There is no flat-string wire shape left to
// handle — the snapshot replay that was its only source is gone.
assert.deepStrictEqual(paceWords([]), []);
assert.deepStrictEqual(paceWords(undefined), []);
console.log("ok  empty segment yields nothing");

// stt.Untimed: the provider had no per-word detail and sent the whole segment
// as one wire word, so `t` holds spaces. It must still tokenize into separate
// display words, and none of them may claim a made-up onset.
var untimed = paceWords([{ t: "one two three", o: 0 }]);
assert.deepStrictEqual(texts(untimed), ["one", "two", "three"]);
assert.deepStrictEqual(holds(untimed), [null, null, null]);
console.log("ok  Untimed segment is char-paced");

// A timed word followed by an Untimed one: the timed word's gap is unknowable
// (the next onset is missing), and pacing it off the following segment's clock
// would be wrong, so it stays null rather than guessing.
var mixed = paceWords([{ t: "hello", o: 0 }, { t: "there world", o: 400 }]);
assert.deepStrictEqual(texts(mixed), ["hello", "there", "world"]);
assert.deepStrictEqual(holds(mixed), [400, null, null]);
console.log("ok  timed word before an untimed run");

// Onsets are never invented: a word with no numeric o contributes no gap, and
// nothing downstream sees an offset running backwards.
var partial = paceWords([{ t: "a" }, { t: "b", o: 100 }, { t: "c", o: 250 }]);
assert.deepStrictEqual(holds(partial), [null, 150, null]);
console.log("ok  missing onset yields no gap");

console.log("\nall paceWords checks passed");
