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
function texts(items) {
  return items.map(function (i) { return i.text; });
}

// A caption event's words: each hold is the next word's onset minus its own,
// so the pause the speaker actually took between "two" and "three" survives.
var timed = paceWords([
  { t: "one", o: 0 },
  { t: "two", o: 300 },
  { t: "three", o: 800 }
]);
assert.deepStrictEqual(texts(timed), ["one", "two", "three"]);
// The last word has no successor onset and the wire carries no duration, so
// null — drain() falls back to its character estimate there.
assert.deepStrictEqual(holds(timed), [300, 500, null]);
console.log("ok  gaps from onsets");

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
