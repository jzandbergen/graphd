/* graphd — markdown rendering for task notes.
 *
 * Notes are stored and transported as plain text (the column stays TEXT, the
 * API and MCP keep returning a string). Markdown is applied as a *view*: the
 * detail panel renders it. That keeps export/import lossless, keeps search and
 * diffing working, and stays inside SPEC.md's "notes is a plain textarea"
 * non-goal — nothing here introduces a document entity, a rich-text editor, or
 * an attachment.
 *
 * SAFETY. marked passes raw HTML through and does not filter link schemes, so
 * rendering its output with innerHTML would execute whatever an agent or a
 * paste put in a task's notes. Escaping the *input* first is the wrong fix: it
 * double-escapes fenced code blocks, which is precisely where a build
 * contract's pseudocode and interfaces live.
 *
 * So the sanitising happens at the renderer level instead — at the point where
 * marked has already decided whether a span of text is raw HTML, a code block,
 * or a link. That way:
 *
 *   - raw HTML is escaped and shows literally (never parsed)
 *   - code blocks stay correct, escaped once, by marked's own code renderer
 *   - links are checked against a scheme allowlist before being emitted
 *   - images are reduced to their alt text: no network, no src attribute
 *
 * This is deliberately a small allowlist rather than a general sanitiser. It
 * covers everything marked can emit, which is the whole attack surface here.
 */
(function (global) {
  'use strict';

  // Schemes a note may link to. Everything else (javascript:, data:, vbscript:,
  // blob:, ...) is downgraded to plain text.
  var ALLOWED_SCHEMES = ['http', 'https', 'mailto'];

  var ESCAPES = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };

  function escapeHtml(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) { return ESCAPES[c]; });
  }

  // Decode the entity forms an attacker would use to smuggle a scheme past a
  // naive prefix check: "javascript&colon;" and "&#106;avascript:" must both be
  // recognised as javascript:.
  var NAMED = {
    amp: '&', lt: '<', gt: '>', quot: '"', apos: "'", colon: ':', sol: '/',
    tab: '\t', newline: '\n', newline_: '\n', 'new-line': '\n'
  };

  function decodeEntities(s) {
    return String(s)
      .replace(/&#x([0-9a-f]{1,6});?/gi, function (_, hex) {
        try { return String.fromCodePoint(parseInt(hex, 16)); } catch (e) { return ''; }
      })
      .replace(/&#([0-9]{1,7});?/g, function (_, dec) {
        try { return String.fromCodePoint(parseInt(dec, 10)); } catch (e) { return ''; }
      })
      .replace(/&([a-z][a-z0-9-]*);?/gi, function (m, name) {
        var k = name.toLowerCase();
        return Object.prototype.hasOwnProperty.call(NAMED, k) ? NAMED[k] : m;
      });
  }

  // safeHref returns the href to emit, or null when it must not be a link.
  function safeHref(href) {
    if (href == null) return null;
    var h = decodeEntities(href);
    // Control characters and raw whitespace inside a scheme ("java\tscript:")
    // are stripped by browsers, so strip them before deciding.
    h = h.replace(/[\u0000-\u0020\u007f]+/g, '');
    if (h === '') return null;
    var lower = h.toLowerCase();
    // Fragment-only and relative links are fine and common.
    if (lower.charAt(0) === '#' || lower.charAt(0) === '/') return h;
    if (lower.indexOf('./') === 0 || lower.indexOf('../') === 0) return h;
    var m = /^([a-z][a-z0-9+.\-]*):/.exec(lower);
    if (!m) return h; // no scheme: relative
    for (var i = 0; i < ALLOWED_SCHEMES.length; i++) {
      if (m[1] === ALLOWED_SCHEMES[i]) return h;
    }
    return null;
  }

  // marked v12 passes renderer methods either a token object (newer shape) or
  // legacy positional arguments. These two helpers read either, so the
  // overrides are not pinned to one calling convention.
  function tokenField(a, name, legacy, index) {
    if (a && typeof a === 'object') {
      var v = a[name];
      return v == null ? '' : v;
    }
    // positional: link(href, title, text) / image(href, title, text)
    if (index === 0) return a == null ? '' : a;
    return legacy == null ? '' : legacy;
  }

  // tokenText returns the human-visible text. When the value came from marked
  // as a link's `text`, it is already rendered and escaped and must be passed
  // through untouched; an image's alt text is raw and gets escaped.
  function tokenText(a, legacy, alreadyRendered) {
    var v;
    if (a && typeof a === 'object') {
      v = a.text != null ? a.text : (a.raw != null ? a.raw : '');
    } else {
      v = legacy;
    }
    if (v == null) v = '';
    return alreadyRendered ? String(v) : escapeHtml(v);
  }

  var configured = false;

  function configure(marked) {
    if (configured) return;
    configured = true;
    marked.use({
      gfm: true,
      // Notes are typed into a textarea, where a single newline means a line
      // break. Honouring that is what people expect; it is also what makes a
      // heading-and-bullets build contract read correctly.
      breaks: true,
      // marked v12 calls renderer methods with legacy positional arguments
      // — link(href, title, text), html(html), image(href, title, text) — where
      // a link's `text` has already been rendered and escaped by marked. The
      // helpers below also accept the token-object shape so this keeps working
      // if a future version switches to it.
      renderer: {
        // Raw HTML is never parsed. It shows as the text the author typed.
        html: function (a) {
          return escapeHtml(tokenText(a));
        },
        link: function (a, b, c) {
          var href = tokenField(a, 'href', b, 0);
          var title = tokenField(a, 'title', b, 1);
          var text = tokenText(a, c, /* alreadyRendered */ true);
          var safe = safeHref(href);
          if (safe === null) return text; // downgrade to plain text, keep the words
          var t = title ? ' title="' + escapeHtml(title) + '"' : '';
          return '<a href="' + escapeHtml(safe) + '"' + t +
            ' rel="noopener noreferrer" target="_blank">' + text + '</a>';
        },
        // No images: no src attribute, no network request, nothing to exploit.
        // The alt text survives so nothing disappears silently.
        image: function (a, b, c) {
          return escapeHtml(tokenText(a, c, /* alreadyRendered */ false));
        },
        // GFM task lists would otherwise emit <input> elements. Render them as
        // text so the output contains no form controls at all.
        checkbox: function (a) {
          var checked = (a && typeof a === 'object') ? a.checked : !!a;
          return checked ? '\u2611 ' : '\u2610 ';
        }
      }
    });
  }

  // render turns note text into HTML safe to assign to innerHTML.
  function render(text) {
    var marked = global.marked;
    if (!text) return '';
    if (!marked || typeof marked.parse !== 'function') {
      // No renderer available: show the raw text, escaped, rather than nothing.
      return '<pre class="md-plain">' + escapeHtml(text) + '</pre>';
    }
    configure(marked);
    var html;
    try {
      html = marked.parse(String(text));
    } catch (e) {
      return '<pre class="md-plain">' + escapeHtml(text) + '</pre>';
    }
    // Belt and braces: nothing marked emits should contain these, and if a
    // future version starts emitting them the failure is a visible no-op rather
    // than a live payload.
    return String(html).replace(/<\s*\/?\s*(script|style|iframe|object|embed|form|input|svg|math)\b[^>]*>/gi, '');
  }

  global.GraphdMarkdown = {
    render: render,
    escapeHtml: escapeHtml,
    safeHref: safeHref,
    decodeEntities: decodeEntities
  };
})(typeof window !== 'undefined' ? window : this);
