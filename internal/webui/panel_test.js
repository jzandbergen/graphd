/* graphd — detail panel logic checks.
 *
 * Run with:  node internal/webui/panel_test.js
 * (also invoked from internal/webui/panel_test.go so `go test ./...` covers it)
 *
 * panel.js is deliberately DOM-free so it can be exercised here with no npm, no
 * node_modules and no headless browser — the same approach as layout_test.js.
 */
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const ASSETS = path.join(__dirname, 'assets');

const sandbox = { console };
sandbox.window = sandbox;
sandbox.global = sandbox;
vm.createContext(sandbox);
vm.runInContext(fs.readFileSync(path.join(ASSETS, 'panel.js'), 'utf8'), sandbox,
  { filename: 'panel.js' });

const P = sandbox.GraphdPanel;

let failures = 0;
function check(name, cond, detail) {
  if (cond) {
    console.log('  ok   ' + name);
  } else {
    failures++;
    console.log('  FAIL ' + name + (detail ? '\n         ' + detail : ''));
  }
}
function has(s, needle) { return s.indexOf(needle) !== -1; }

console.log('detail panel');

// ---- clamp ----
check('clamp keeps short text', P.clamp('abc', 10) === 'abc');
check('clamp ellipsises', P.clamp('abcdefghij', 5) === 'abcd\u2026');
check('clamp does not split a surrogate pair', P.clamp('a\u{1F600}b', 3) === 'a\u2026',
  JSON.stringify(P.clamp('a\u{1F600}b', 3)));

// ---- nextPane: the ring wraps both ways ----
check('ring forward', P.nextPane('notes', 1) === 'output');
check('ring forward from last wraps', P.nextPane('links', 1) === 'notes');
check('ring backward', P.nextPane('output', -1) === 'notes');
check('ring backward from first wraps', P.nextPane('notes', -1) === 'links');
check('ring full lap is identity', P.nextPane('inputs', 4) === 'inputs');
check('unknown current starts at the beginning', P.nextPane('bogus', 1) === 'output');
check('unknown current with negative delta still lands in range',
  P.PANES.indexOf(P.nextPane('bogus', -1)) !== -1);

// ---- tabCount: dot for prose, number for lists ----
check('notes dot when present', P.tabCount('notes', { notes: 'x' }) === '\u2022');
check('notes blank when absent', P.tabCount('notes', { notes: '  ' }) === '');
check('output dot when present', P.tabCount('output', { output: 'x' }) === '\u2022');
check('output blank when absent', P.tabCount('output', {}) === '');
check('inputs number', P.tabCount('inputs', { inputs: [{}, {}, {}] }) === '3');
check('inputs blank when none', P.tabCount('inputs', { inputs: [] }) === '');
check('null task is safe', P.tabCount('notes', null) === '');

// ---- PANES is the single source of tab order ----
check('four panes', P.PANES.length === 4);
check('pane order is notes first', P.PANES[0] === 'notes');

// ---- the shell and the scripts agree ----
//
// There is no browser here and no jsdom, but the highest-risk failure in a
// panel refactor is a selector that no longer matches an element: a renamed id
// or a typo makes a control silently dead, and nothing else would catch it.
// These checks are static, cheap, and cover exactly that.

const shell = fs.readFileSync(path.join(ASSETS, 'index.html'), 'utf8');
const app = fs.readFileSync(path.join(ASSETS, 'app.js'), 'utf8');

// Every id the shell defines.
const shellIds = new Set();
{
  const re = /\bid="([^"]+)"/g;
  let m;
  while ((m = re.exec(shell)) !== null) shellIds.add(m[1]);
}

// Every `#id` selector app.js looks up. Only ids (not classes or tag paths),
// because those are the ones that break silently. The selector must be a
// complete string literal — `'#nav-' + state.view` is built at runtime and
// cannot be checked statically, so it is skipped rather than reported.
// Any string literal that is an id selector anywhere in app.js — not only the
// ones passed straight to $(), because several live in config objects
// (`notesCfg.rendered = '#detail-output-rendered'`) and those break just as
// silently. Literals that are concatenated at runtime (`'#nav-' + view`) are
// skipped: they cannot be resolved statically.
const usedIds = new Set();
{
  const re = /'#([A-Za-z0-9_-]+)'(\s*\+)?/g;
  let m;
  while ((m = re.exec(app)) !== null) {
    if (m[2]) continue; // part of a concatenation
    usedIds.add(m[1]);
  }
}

const missing = [...usedIds].filter((id) => !shellIds.has(id)).sort();
check('every id app.js queries exists in the shell', missing.length === 0,
  'missing from index.html: ' + missing.join(', '));
check('app.js queries a non-trivial number of ids', usedIds.size > 15,
  'found ' + usedIds.size);

// Every data-pane the shell declares must be a pane panel.js knows about, and
// vice versa: a pane in PANES with no section is unreachable, and a section with
// no entry in PANES cannot be cycled to.
const shellPanes = new Set();
{
  const re = /data-pane="([^"]+)"/g;
  let m;
  while ((m = re.exec(shell)) !== null) shellPanes.add(m[1]);
}
const missingSection = P.PANES.filter((p) => !shellPanes.has(p));
const extraSection = [...shellPanes].filter((p) => P.PANES.indexOf(p) === -1);
check('every PANES entry has a section in the shell', missingSection.length === 0,
  'no section for: ' + missingSection.join(', '));
check('every shell pane is in PANES', extraSection.length === 0,
  'not in PANES: ' + extraSection.join(', '));

// The tab buttons must be exactly the panes, or clicking a tab selects nothing.
const tabPanes = new Set();
{
  const re = /class="tab"[^>]*data-pane="([^"]+)"/g;
  let m;
  while ((m = re.exec(shell)) !== null) tabPanes.add(m[1]);
}
const missingTab = P.PANES.filter((p) => !tabPanes.has(p));
check('every pane has a tab button', missingTab.length === 0,
  'no tab for: ' + missingTab.join(', '));

// panel.js must be loaded, or every GraphdPanel call is a runtime error.
check('panel.js is loaded by the shell', has(shell, '/assets/panel.js'));

// ---- prose panes show their content, with no collapse control ----
//
// The prose panes used to have a second disclosure control nested inside the
// tab: you landed on the notes tab and saw a one-line summary of the notes
// rather than the notes, and had to click again. The tab is already the
// one-thing-at-a-time mechanism, so the nested one was removed. A summary row
// that reappears — or a pane body that starts hidden — is the regression.
{
  const notesPane = /<section class="pane" data-pane="notes">([\s\S]*?)<\/section>/.exec(shell);
  const outputPane = /<section class="pane" data-pane="output">([\s\S]*?)<\/section>/.exec(shell);

  check('notes pane exists', !!notesPane);
  check('output pane exists', !!outputPane);

  for (const [name, m] of [['notes', notesPane], ['output', outputPane]]) {
    if (!m) continue;
    const body = m[1];
    // The body must not start hidden — that is what made it look collapsed.
    const bodyTag = /<div class="pane-body"[^>]*>/.exec(body);
    check(`[${name}] pane body is not hidden in the markup`,
      !!bodyTag && !/\bhidden\b/.test(bodyTag[0]), bodyTag ? bodyTag[0] : 'no pane-body');
    // No collapse affordance of any kind.
    check(`[${name}] has no collapse toggle`,
      !/class="pane-toggle"/.test(body) && !/id="\w+-toggle"/.test(body));
    check(`[${name}] has no summary row`,
      !/id="\w+-summary"/.test(body) && !/class="summary"/.test(body));
    // The label survives so edit/preview keeps its context.
    check(`[${name}] keeps its label`, /class="pane-label"/.test(body));
    // The tools and the textarea are still there.
    check(`[${name}] keeps its textarea`, new RegExp('id="detail-' + name + '"').test(body));
    check(`[${name}] keeps edit and preview`,
      new RegExp('id="' + name + '-edit-btn"').test(body)
      && new RegExp('id="' + name + '-preview-btn"').test(body));
  }

  // The list panes must not have picked up a collapse control either.
  for (const p of ['inputs', 'links']) {
    const m = new RegExp('<section class="pane" data-pane="' + p + '">([\\s\\S]*?)</section>').exec(shell);
    check(`[${p}] has no collapse toggle`, !!m && !/pane-toggle/.test(m[1]));
  }

  // And nothing in the scripts may still drive one.
  check('no collapse machinery left in app.js',
    !/setPaneOpen|paneOpen|pane-toggle|notes-summary|output-summary/.test(app));
  check('no summary row is ever written',
    !/updateSummaries|paneSummaryLine/.test(app));
}

// ---- the panel must actually hide when hidden ----
//
// Regression guard for a real bug: `.detail { display: flex }` beat the
// user-agent's `[hidden] { display: none }` (equal specificity, later origin
// wins), so setting `panel.hidden = true` left the panel fully rendered. The
// close button appeared to do nothing, and the panel was visible-but-empty at
// boot. A stylesheet rule must restate the hidden state for any element that
// sets its own display.
{
  const css = fs.readFileSync(path.join(ASSETS, 'style.css'), 'utf8');

  // Every selector in the sheet that sets display, mapped to whether it carries
  // the hidden attribute in the markup.
  const hiddenIds = [];
  {
    const re = /<(\w+)[^>]*\bid="([^"]+)"[^>]*\bhidden\b[^>]*>/g;
    let m;
    while ((m = re.exec(shell)) !== null) hiddenIds.push(m[2]);
  }
  check('the shell starts with at least one hidden element', hiddenIds.length > 0,
    'found ' + hiddenIds.length);

  // For each hidden element, if its class sets display, the sheet must also
  // have a `[hidden]` rule for it.
  const offenders = [];
  for (const id of hiddenIds) {
    const tag = new RegExp('<\\w+[^>]*\\bid="' + id + '"[^>]*>').exec(shell);
    if (!tag) continue;
    const cls = /class="([^"]+)"/.exec(tag[0]);
    if (!cls) continue;
    for (const c of cls[1].split(/\s+/)) {
      // Does .c set display anywhere?
      const setsDisplay = new RegExp('\\.' + c + '(?![A-Za-z0-9_-])[^{}]*\\{[^}]*display\\s*:', 'm').test(css);
      if (!setsDisplay) continue;
      // Is there a .c[hidden] rule (or a bare [hidden] rule)?
      const restated = new RegExp('\\.' + c + '\\[hidden\\][^{}]*\\{[^}]*display\\s*:\\s*none', 'm').test(css)
        || /(^|\n)\[hidden\][^{}]*\{[^}]*display\s*:\s*none/m.test(css);
      if (!restated) offenders.push('.' + c + ' (id=' + id + ')');
    }
  }
  check('every element that sets display restates its [hidden] state',
    offenders.length === 0,
    'display:flex beats [hidden] for: ' + offenders.join(', '));
}

// ---- every GraphdPanel member app.js calls is actually exported ----
//
// app.js and panel.js are separate files with no compiler between them; a
// renamed helper would be a runtime TypeError the first time that code path ran.
{
  const used = new Set();
  const re = /GraphdPanel\.([A-Za-z_][A-Za-z0-9_]*)/g;
  let m;
  while ((m = re.exec(app)) !== null) used.add(m[1]);
  const missing = [...used].filter((k) => !(k in P)).sort();
  check('every GraphdPanel member app.js uses is exported', missing.length === 0,
    'not exported by panel.js: ' + missing.join(', '));
  check('app.js uses GraphdPanel', used.size > 0, 'found ' + used.size + ' call sites');
}

// ---- every CSS class app.js toggles exists in the stylesheet ----
//
// The panel's states (open, on, wide, zero) are all class-driven, so a class
// name that no longer matches a rule is a control that silently does nothing.
//
// Limitation, stated so this is not mistaken for more than it is: the check
// proves the class is styled *somewhere*, not that the rule is scoped to the
// right element. `on` is shared by tabs, chips, radios and buttons, so renaming
// one of those rules still passes. It catches the realistic failure — a class
// removed or renamed outright — and cannot catch a mis-scoped one.
{
  const css = fs.readFileSync(path.join(ASSETS, 'style.css'), 'utf8');
  const toggled = new Set();
  const re = /classList\.(?:toggle|add|remove)\(\s*'([A-Za-z0-9_-]+)'/g;
  let m;
  while ((m = re.exec(app)) !== null) toggled.add(m[1]);
  // Word-boundary match, so `.on` does not count `.once` as a hit.
  const styledSomewhere = (c) => new RegExp('\\.' + c + '(?![A-Za-z0-9_-])').test(css);
  const missing = [...toggled].filter((c) => !styledSomewhere(c)).sort();
  check('every class app.js toggles is styled', missing.length === 0,
    'no rule for: ' + missing.join(', '));
  check('app.js toggles a non-trivial number of classes', toggled.size > 4,
    'found ' + toggled.size);
}

// ---- the canvas node carries no glyph overlay ----
//
// A canvas node says what it is with its outline and its fill, not with an icon
// parked on top of the label. Three markers were removed, each because it was
// noise or unreadable at the zoom you actually read a graph at: a ▶ "has
// output" glyph on tasks that were finished anyway, a 🔒 lock, and a bare
// `unblocks` number in the corner whose meaning was not discoverable. A second
// text element per node also needs a div layer repositioned on every render,
// which is exactly the kind of thing that drifts out of sync with the canvas.
// A reintroduced icon is the regression here, whatever class name it wears.
{
  const canvas = fs.readFileSync(path.join(ASSETS, 'canvas.js'), 'utf8');
  const css = fs.readFileSync(path.join(ASSETS, 'style.css'), 'utf8');

  check('no glyph overlay layer is built', !/id="badges"|class="badges"/.test(canvas));
  check('no overlay is repositioned on render', !/positionOverlay/.test(canvas));
  check('no overlay layer is left in the stylesheet', !/\.badges\b/.test(css));

  for (const glyph of ['ov-badge', 'ov-lock', 'ov-output']) {
    check(`[${glyph}] is gone from canvas.js`, !canvas.includes(glyph));
    check(`[${glyph}] has no stylesheet rule`, !css.includes('.' + glyph));
  }

  // The lock and the ▶ were entities; match those too, so a reintroduced icon
  // cannot pass by dropping the class name.
  check('no lock, play or badge entity is written by canvas.js',
    !canvas.includes('128274') && !canvas.includes('9654'));
  check('no unblocks badge is drawn on the canvas', !/\bunblocks\b/.test(canvas));

  // Blocked is now the node's own outline, and it is scoped to `todo`: a task
  // already started is not blocked, and red would fight the amber `doing`
  // border for the same edge.
  check('blocked is a node style, not a marker', /selector:\s*'node\.blocked'/.test(canvas));
  check('blocked only applies to todo',
    /t\.status === 'todo' && t\.blocked_by_open/.test(canvas));
}

if (failures > 0) {
  console.log('\n' + failures + ' check(s) failed');
  process.exit(1);
}
console.log('all detail panel checks passed');
