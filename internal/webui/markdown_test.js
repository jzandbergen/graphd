/* graphd — markdown rendering checks, including the XSS cases.
 *
 * Run with:  node internal/webui/markdown_test.js
 * (also invoked from internal/webui/markdown_test.go so `go test ./...` covers it)
 *
 * As with layout_test.js there is no npm here: marked is loaded from the
 * vendored UMD build via a tiny CommonJS shim.
 */
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const HERE = __dirname;
const ASSETS = path.join(HERE, 'assets');
const VENDOR = path.join(ASSETS, 'vendor');

// Minimal browser-ish global for the UMD bundles and markdown.js.
const sandbox = {
  console, setTimeout, clearTimeout,
  document: undefined
};
sandbox.window = sandbox;
sandbox.self = sandbox;
sandbox.global = sandbox;
sandbox.globalThis = sandbox;
vm.createContext(sandbox);

// marked's UMD branch wants module/exports.
sandbox.module = { exports: {} };
sandbox.exports = sandbox.module.exports;
vm.runInContext(fs.readFileSync(path.join(VENDOR, 'marked.min.js'), 'utf8'), sandbox,
  { filename: 'marked.min.js' });
const markedMod = sandbox.module.exports;
sandbox.marked = markedMod.marked || markedMod;

vm.runInContext(fs.readFileSync(path.join(ASSETS, 'markdown.js'), 'utf8'), sandbox,
  { filename: 'markdown.js' });

const md = sandbox.GraphdMarkdown;

let failures = 0;
function check(name, cond, detail) {
  if (cond) {
    console.log('  ok   ' + name);
  } else {
    failures++;
    console.log('  FAIL ' + name + (detail ? '\n         ' + detail : ''));
  }
}
function renders(text) { return md.render(text); }
function contains(html, needle) { return html.indexOf(needle) !== -1; }

console.log('markdown rendering');

// ---- formatting still works ----
check('bold', contains(renders('**b**'), '<strong>b</strong>'));
check('emphasis', contains(renders('*i*'), '<em>i</em>'));
check('heading', contains(renders('# H'), '<h1>H</h1>'));
check('bullet list', contains(renders('- one\n- two'), '<li>one</li>'));
check('ordered list', contains(renders('1. one'), '<li>one</li>'));
check('inline code', contains(renders('use `<div>`'), '<code>&lt;div&gt;</code>'));
check('fenced code block', contains(renders('```\nx\n```'), '<pre><code>'));
check('blockquote', contains(renders('> q'), '<blockquote>'));
check('table (gfm)', contains(renders('| a | b |\n| - | - |\n| 1 | 2 |'), '<table>'));
check('hr', contains(renders('---'), '<hr'));

// ---- code blocks are escaped exactly once (this is why input-escaping is wrong) ----
const fence = renders('```\n<script>x</script>\n```');
check('code block escapes markup once', contains(fence, '&lt;script&gt;x&lt;/script&gt;'),
  JSON.stringify(fence));
check('code block does not double-escape', !contains(fence, '&amp;lt;'), JSON.stringify(fence));
const inl = renders('`a & b`');
check('inline code escapes ampersand once', contains(inl, 'a &amp; b'), JSON.stringify(inl));

// ---- XSS: raw HTML is shown literally, never parsed ----
const cases = [
  ['<script>alert(1)</script>', 'script tag'],
  ['<img src=x onerror=alert(1)>', 'img onerror'],
  ['<iframe src="https://evil.test"></iframe>', 'iframe'],
  ['<style>body{display:none}</style>', 'style tag'],
  ['<svg/onload=alert(1)>', 'svg onload'],
  ['<a href="javascript:alert(1)">x</a>', 'anchor js scheme'],
  ['<form action="//evil.test"><input name=p>', 'form'],
  ['<object data="x"></object>', 'object'],
  ['<embed src="x">', 'embed'],
  ['<math><mtext></mtext></math>', 'math'],
  ['<base href="//evil.test">', 'base'],
];
for (const [payload, label] of cases) {
  const out = renders(payload);
  const bad = /<\s*(script|iframe|style|svg|form|input|object|embed|math|base)\b/i.test(out);
  check('raw HTML neutralised: ' + label, !bad, JSON.stringify(out));
}

// ---- XSS: dangerous link schemes are downgraded, text preserved ----
const schemes = [
  ['[c](javascript:alert(1))', 'javascript:'],
  ['[c](JavaScript:alert(1))', 'mixed case javascript:'],
  ['[c](java\tscript:alert(1))', 'tab-smuggled scheme'],
  ['[c](jav&#x61;script:alert(1))', 'entity-smuggled scheme'],
  ['[c](data:text/html,<script>alert(1)</script>)', 'data:'],
  ['[c](vbscript:msgbox(1))', 'vbscript:'],
  ['[c](blob:https://x/y)', 'blob:'],
];
for (const [payload, label] of schemes) {
  const out = renders(payload);
  check('link scheme blocked: ' + label,
    !/<a\s[^>]*href/i.test(out) && contains(out, 'c'),
    JSON.stringify(out));
}

// ---- safe links are kept, and hardened ----
const safe = renders('[ok](https://example.test/a?b=1&c=2)');
check('https link kept', contains(safe, 'href="https://example.test/a?b=1&amp;c=2"'), safe);
check('external link gets rel=noopener', contains(safe, 'rel="noopener noreferrer"'), safe);
check('mailto link kept', contains(renders('[m](mailto:a@b.test)'), 'href="mailto:a@b.test"'));
check('relative link kept', contains(renders('[r](/canvas)'), 'href="/canvas"'));
check('fragment link kept', contains(renders('[f](#sec)'), 'href="#sec"'));

// ---- images reduced to alt text: no src, no network ----
const img = renders('![alt text](https://evil.test/x.png)');
check('image emits no src', !contains(img, 'src='), img);
check('image keeps alt text', contains(img, 'alt text'), img);
check('image emits no img tag', !contains(img, '<img'), img);

// ---- task list checkboxes become text, not form controls ----
const tl = renders('- [x] done\n- [ ] todo');
check('task list emits no input element', !contains(tl, '<input'), tl);

// ---- entity handling: a literal entity in the source is not re-decoded ----
const ent = renders('literal &lt;script&gt; here');
check('literal entity stays escaped', !contains(ent, '<script>'), ent);

// ---- degenerate input ----
check('empty string renders empty', renders('') === '');
check('null-ish renders empty', renders(null) === '');
check('plain text still renders', contains(renders('just words'), 'just words'));

// ---- unicode passes through ----
check('unicode survives', contains(renders('日本語 and → arrows'), '日本語 and → arrows'));

// ---- the helper itself ----
check('safeHref allows https', md.safeHref('https://x.test') === 'https://x.test');
check('safeHref rejects javascript', md.safeHref('javascript:alert(1)') === null);
check('safeHref rejects entity-smuggled', md.safeHref('jav&#x61;script:x') === null);
check('escapeHtml escapes all five', md.escapeHtml(`&<>"'`) === '&amp;&lt;&gt;&quot;&#39;');

if (failures > 0) {
  console.log('\n' + failures + ' check(s) failed');
  process.exit(1);
}
console.log('all markdown checks passed');
