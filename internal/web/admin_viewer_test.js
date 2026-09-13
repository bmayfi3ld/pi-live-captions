// Run: node internal/web/admin_viewer_test.js (no server or browser needed).
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const admin = fs.readFileSync(__dirname + '/static/admin.html', 'utf8');
const index = fs.readFileSync(__dirname + '/static/index.html', 'utf8');
function between(source, start, end) {
  const a = source.indexOf(start);
  const b = source.indexOf(end, a);
  assert(a >= 0 && b > a, 'test source boundaries exist');
  return source.slice(a, b);
}

// Exercise the actual chart code, including SVG labels and plotted values.
const elements = {};
const charts = vm.createContext({document: {getElementById(id) {
  return elements[id] ||= {style: {}, clientWidth: 600, setAttribute() {}};
}}});
vm.runInContext(between(admin, '  function fmtMs(', '  function fmtBytes(') +
  between(admin, '  var TREND_CAP', '  // ---- latency waterfall'), charts);
function plot(value, peak) {
  vm.runInContext(`pushTrend('rec', ${value}); drawTrend('rec', 'svg', 'empty', 'blue', ${peak});`, charts);
  return elements.svg.innerHTML;
}
plot(10, 10);
let svg = plot(20, 20);
assert(svg.includes('20 ms</text>'));
const before = svg;
svg = plot(1000, 1000);
assert(svg.includes('1,000 ms</text>'));
assert.notEqual(svg, before);
for (let i = 0; i < 301; i++) svg = plot(5, 1000);
assert(svg.includes('1,000 ms</text>'), 'axis stays at peak after spike rolls away');
assert(svg.includes('0 ms</text>'));
assert.equal(charts.trendHistory.rec.length, 300);

// Exercise the Source card's actual renderer, including visible troubleshooting
// guidance and text-only diagnostics.
const sourceElements = {};
const sourceContext = vm.createContext({document: {getElementById(id) {
  return sourceElements[id] ||= {style: {}, textContent: '', removeAttribute(name) { delete this[name]; }};
}}});
vm.runInContext(
  between(admin, '  function setText(', '  // ---- segment trend graphs') +
  between(admin, '  var sourceHelp = ', '  function render(s)'), sourceContext);
function sourceSnapshot(state, restart, error) {
  vm.runInContext(`renderSource(${JSON.stringify({state, restart_required: restart, error, spec: 'pulse:mic'})});`, sourceContext);
  return sourceElements;
}
let source = sourceSnapshot('missing', true, '<device unavailable>');
assert.equal(source['src-state'].textContent, 'Missing (restart required)');
assert.equal(source['src-error'].textContent, '<device unavailable>');
assert.equal(source['src-help'].style.display, 'block');
assert(!between(admin, '  function renderSource(src)', '  function render(s)').includes('title'), 'Source card has no hover tooltip');
source = sourceSnapshot('unavailable', false, 'EOF without stderr');
assert.equal(source['src-state'].textContent, 'Unavailable (retrying automatically)');
assert(source['src-help'].textContent.includes('retries this configured input automatically'));
assert.equal(source['src-error'].textContent, 'EOF without stderr');
source = sourceSnapshot('capturing', false, 'stale error');
assert.equal(source['src-help'].style.display, 'none');
assert.equal(source['src-error'].style.display, 'none');
source = sourceSnapshot('', false, '<old snapshot>');
assert.equal(source['src-state'].textContent, '—');
assert.equal(source['src-help'].style.display, 'none');
assert.equal(source['src-error'].style.display, 'none');
assert.equal(source['src-error'].innerHTML, undefined, 'diagnostics must be rendered as text');

// Run both pages' actual event handlers against a small caption-stack stub.
for (const html of [admin, index]) {
  const markers = [];
  const context = vm.createContext({
    hubState: null, musicOn: false,
    captionStack: {pushEvent: text => markers.push(text), appendSegment() {}, breakRow() {}},
    applyIndicator() {}
  });
  // onMessage is the last function before connect() in each page.
  const start = html.indexOf('  function onMessage(e)');
  const end = html.indexOf('\n  }', start) + 4;
  vm.runInContext(html.slice(start, end), context);
  function send(ev) { context.onMessage({data: JSON.stringify(ev)}); }
  send({kind: 'status', state: 'connected', snapshot: true});
  send({kind: 'music', state: 'off', snapshot: true});
  send({kind: 'music', state: 'on'});
  send({kind: 'music', state: 'on'});
  send({kind: 'music', state: 'on', snapshot: true});
  assert.equal(context.musicOn, true);
  send({kind: 'music', state: 'off', snapshot: true});
  assert.equal(context.musicOn, false, 'reconnect clears missed music-off');
  send({kind: 'status', state: 'paused'});
  send({kind: 'status', state: 'paused'});
  send({kind: 'status', state: 'paused', snapshot: true});
  assert.deepEqual(markers, ['♪ music ♪', '— silence —']);
}

// URL defaults, validation and percentage positioning.
for (const [query, expected] of [['', undefined], ['bottom=10', '10'], ['bottom=0', '0'],
  ['bottom=-1', '0'], ['bottom=100', '90'], ['bottom=Infinity', undefined],
  ['bottom=oops', undefined], ['bottom=', undefined]]) {
  const styles = {};
  const context = vm.createContext({params: new URLSearchParams(query),
    document: {documentElement: {style: {setProperty(k, v) { styles[k] = v; }}}}});
  vm.runInContext(between(index, '  // ?bottom=N', '  var wakeDisabled'), context);
  assert.equal(context.requestedLines, 4);
  assert.equal(styles['--caption-bottom'], expected === undefined ? undefined :
    `max(${expected}dvh, env(safe-area-inset-bottom, 0px))`);
}
console.log('admin/viewer checks passed');
