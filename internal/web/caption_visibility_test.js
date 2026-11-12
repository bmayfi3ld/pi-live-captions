// Run: node internal/web/caption_visibility_test.js (no server needed).
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

for (const name of ['index', 'admin']) {
  const html = fs.readFileSync(`${__dirname}/static/${name}.html`, 'utf8');
  const start = html.indexOf('  var es = null;');
  const end = html.indexOf('\n  connect();', start) + '\n  connect();'.length;
  const listenerStart = html.lastIndexOf('  document.addEventListener("visibilitychange"');
  const listenerEnd = html.indexOf('\n})();', listenerStart);
  assert(start >= 0 && end > start && listenerEnd > listenerStart);

  function page(visibilityState) {
    const streams = [], timers = new Map(), words = [], markers = [];
    let nextTimer = 0, clears = 0, indicators = 0, wakes = 0, visibility;
    const document = {visibilityState, addEventListener(event, fn) {
      assert.equal(event, 'visibilitychange'); visibility = fn;
    }};
    function EventSource() { this.readyState = 1; streams.push(this); }
    EventSource.CLOSED = 2;
    EventSource.prototype.close = function () { this.readyState = 2; this.closed = true; };
    const context = vm.createContext({
      document, EventSource, sseOpen: null, hubState: null, musicOn: false,
      captionStack: {
        clear() { clears++; words.length = 0; markers.length = 0; },
        appendSegment(segment) { words.push(...segment); },
        breakRow() {}, pushEvent(text) { markers.push(text); }
      },
      applyIndicator() { indicators++; }, syncWake() { wakes++; },
      setTimeout(fn) { const id = ++nextTimer; timers.set(id, fn); return id; },
      clearTimeout(id) { timers.delete(id); }
    });
    vm.runInContext(html.slice(start, end) + '\n' + html.slice(listenerStart, listenerEnd), context);
    return {context, streams, timers, words, markers,
      show(state) { document.visibilityState = state; visibility(); },
      clears: () => clears, indicators: () => indicators, wakes: () => wakes};
  }
  function send(source, ev) { source.onmessage({data: JSON.stringify(ev)}); }
  const caption = {kind: 'caption', words: [{t: 'live', o: 0, d: 100}], speaker: 1};
  const p = page('visible');
  const old = p.streams[0];
  old.onopen();
  assert.equal(p.context.sseOpen, true);
  send(old, caption);
  assert.equal(p.words.length, 1, 'foreground captions reach the typesetter');
  old.readyState = 2;
  old.onerror();
  assert.equal(p.timers.size, 1);
  const staleRetry = [...p.timers.values()][0];
  p.show('hidden');
  assert(old.closed);
  assert.equal(p.timers.size, 0);
  assert.equal(p.words.length, 0);
  const hiddenIndicators = p.indicators();
  send(old, caption); old.onopen(); old.onerror(); staleRetry();
  assert.equal(p.indicators(), hiddenIndicators);
  assert.equal(p.words.length, 0);
  assert.equal(p.streams.length, 1);
  p.show('visible');
  assert.equal(p.streams.length, 2);
  assert.equal(p.context.sseOpen, null, 'replacement starts connecting');
  const current = p.streams[1];
  const resumedIndicators = p.indicators();
  send(old, caption); old.onopen(); old.onerror(); staleRetry();
  assert.equal(p.words.length, 0);
  assert.equal(p.indicators(), resumedIndicators);
  assert.equal(p.timers.size, 0);
  assert.equal(p.streams.length, 2);
  current.onopen();
  send(current, {kind: 'status', state: 'paused', snapshot: true});
  send(current, {kind: 'music', state: 'on', snapshot: true});
  assert.equal(p.context.hubState, 'paused');
  assert.equal(p.context.musicOn, true);
  assert.equal(p.markers.length, 0, 'snapshots do not invent markers');
  send(current, caption);
  assert.equal(p.words.length, 1);
  for (let i = 0; i < 3; i++) {
    p.show('hidden'); p.show('visible');
    assert.equal(p.streams.filter(s => !s.closed).length, 1);
    assert.equal(p.timers.size, 0);
    assert.equal(p.words.length, 0);
  }
  if (name === 'index') assert.equal(p.wakes(), 8, 'wake synchronization still runs');
  const hidden = page('hidden');
  assert.equal(hidden.streams.length, 0, 'hidden initialization opens no stream');
  hidden.show('visible');
  assert.equal(hidden.streams.length, 1);

  // Normal foreground hard-failure retries still recover.
  const failed = hidden.streams[0];
  failed.readyState = 2; failed.onerror();
  const [id, retry] = [...hidden.timers.entries()][0];
  hidden.timers.delete(id); retry();
  assert.equal(hidden.streams.length, 2);
  assert(failed.closed);
  console.log(`ok ${name}: visibility reset, stale callbacks, snapshots and retries`);
}
