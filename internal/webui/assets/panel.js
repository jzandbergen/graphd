/* graphd — detail panel logic.
 *
 * Pure functions only: no DOM, no globals beyond the export. That is what makes
 * them testable under node (see internal/webui/panel_test.js) with the same
 * no-npm, no-build-step approach as layout.js and markdown.js.
 *
 * The panel splits into a glance strip and four tabs. These helpers are the two
 * decisions that are worth getting right and are easy to get wrong: what a
 * collapsed prose pane says in one line, and how the tab ring wraps.
 */
(function (global) {
  'use strict';

  // PANES is the tab order, left to right. It is also the order [ and ] cycle
  // through, so there is exactly one list to keep in step.
  var PANES = ['notes', 'output', 'inputs', 'links'];

  // clamp truncates to n characters, appending an ellipsis, without splitting a
  // surrogate pair. That last part is why this exists rather than every caller
  // doing `s.slice(0, n)`: a naive slice can cut an emoji in half and leave a
  // lone surrogate, which renders as a replacement glyph.
  function clamp(s, n) {
    if (s.length <= n) return s;
    var cut = s.slice(0, n - 1);
    // Do not end on a lone high surrogate.
    var last = cut.charCodeAt(cut.length - 1);
    if (last >= 0xd800 && last <= 0xdbff) cut = cut.slice(0, -1);
    return cut + '\u2026';
  }

  // nextPane returns the tab reached from `current` by moving `delta` places
  // around the ring. It wraps in both directions and treats an unknown current
  // pane as "start at the beginning", so a stale value can never wedge the
  // keyboard navigation.
  function nextPane(current, delta) {
    var i = PANES.indexOf(current);
    if (i < 0) i = 0;
    var n = PANES.length;
    return PANES[((i + delta) % n + n) % n];
  }

  // tabCount is the small badge on each tab: a dot for prose that exists, a
  // number for the list panes. '' means "show nothing".
  function tabCount(pane, task) {
    if (!task) return '';
    if (pane === 'notes') return (task.notes || '').trim() ? '\u2022' : '';
    if (pane === 'output') return (task.output || '').trim() ? '\u2022' : '';
    if (pane === 'inputs') {
      var n = (task.inputs || []).length;
      return n ? String(n) : '';
    }
    return '';
  }

  global.GraphdPanel = {
    PANES: PANES,
    clamp: clamp,
    nextPane: nextPane,
    tabCount: tabCount
  };
})(window);
