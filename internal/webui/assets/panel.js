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

  // summarize condenses a prose field into the single line shown while the pane
  // is collapsed.
  //
  // It is deliberately a *shape*, not a truncation of the text. "412 chars ·
  // 3 headings · 5 items" answers "is this worth opening?" far better than the
  // first forty characters do — the opening line of an analysis is usually its
  // least informative part. A short lead-in is appended as a hint of content,
  // but the counts carry the decision.
  //
  // Returns null for empty/whitespace input so the caller can fall back to its
  // own "no notes" / "no output recorded" wording.
  function summarize(text) {
    if (!text || !String(text).trim()) return null;
    var src = String(text);
    var lines = src.split('\n');
    var headings = 0, bullets = 0, blocks = 0, inFence = false;

    for (var i = 0; i < lines.length; i++) {
      var ln = lines[i];
      // A fence toggles; only count the opening one, and ignore everything
      // inside so a `#` in a shell snippet is not mistaken for a heading.
      if (/^\s*(```|~~~)/.test(ln)) {
        inFence = !inFence;
        if (inFence) blocks++;
        continue;
      }
      if (inFence) continue;
      if (/^\s*#{1,6}\s/.test(ln)) headings++;
      else if (/^\s*([-*+]|\d+[.)])\s/.test(ln)) bullets++;
    }

    var bits = [src.length + ' chars'];
    if (headings) bits.push(headings + (headings === 1 ? ' heading' : ' headings'));
    if (bullets) bits.push(bullets + (bullets === 1 ? ' item' : ' items'));
    if (blocks) bits.push(blocks + (blocks === 1 ? ' block' : ' blocks'));

    var lead = firstProseLine(lines);
    var out = bits.join(' \u00b7 ');
    if (lead) out += ' \u2014 ' + lead;
    return out;
  }

  // firstProseLine finds the first line that carries actual content: not blank,
  // not a heading, not a fence. Used as the one-line hint after the counts.
  function firstProseLine(lines) {
    for (var i = 0; i < lines.length; i++) {
      var s = lines[i].trim();
      if (!s) continue;
      if (/^#{1,6}\s/.test(s)) continue;
      if (/^(```|~~~)/.test(s)) continue;
      if (/^[-*_]{3,}$/.test(s)) continue; // horizontal rule
      // Strip the list marker and inline emphasis so the hint reads as prose.
      s = s.replace(/^([-*+]|\d+[.)])\s+/, '').replace(/[`*_]/g, '').replace(/^>\s*/, '');
      s = s.trim();
      if (s) return clamp(s, 38);
    }
    return '';
  }

  // clamp truncates to n characters, appending an ellipsis. Counts code units,
  // which is fine for a display hint; it does not split a surrogate pair.
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

  // paneSummaryLine is what the collapsed row reads for a given pane, so the
  // wording lives in one place rather than being spelled out at each call site.
  function paneSummaryLine(pane, task) {
    if (!task) return '';
    if (pane === 'notes') return summarize(task.notes) || 'no notes';
    if (pane === 'output') return summarize(task.output) || 'no output recorded';
    if (pane === 'inputs') {
      var n = (task.inputs || []).length;
      if (!n) {
        return (task.blocked_by || []).length
          ? 'blockers have recorded no output'
          : 'no upstream inputs';
      }
      return n + (n === 1 ? ' upstream output' : ' upstream outputs');
    }
    return '';
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
    summarize: summarize,
    firstProseLine: firstProseLine,
    clamp: clamp,
    nextPane: nextPane,
    paneSummaryLine: paneSummaryLine,
    tabCount: tabCount
  };
})(window);
