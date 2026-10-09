/* graphd — bootstrap. Project switcher, SSE, view routing, the detail panel and
 * the shared API helper. One header, one source of truth, two views.
 */
(function (global) {
  'use strict';

  var state = {
    projects: [],
    projectId: null,
    view: document.documentElement.dataset.view || 'canvas',
    graph: null,
    ready: null,
    archived: false,
    engine: 'dagre',
    orientation: 'LR', // 'LR' horizontal (default) | 'TB' vertical
    source: null,      // EventSource
    refreshTimer: null,
    dirty: false
  };

  var view = null;     // GraphdCanvas or GraphdBoard

  // ---- small helpers ----

  function $(sel) { return document.querySelector(sel); }
  function $$(sel) { return Array.prototype.slice.call(document.querySelectorAll(sel)); }

  function toast(msg, isError) {
    var el = $('#toast');
    el.textContent = msg;
    el.classList.toggle('error', !!isError);
    el.hidden = false;
    clearTimeout(el._t);
    el._t = setTimeout(function () { el.hidden = true; }, isError ? 6000 : 2500);
  }

  function api(method, path, body) {
    var opts = { method: method, headers: {} };
    if (body !== undefined) {
      opts.headers['Content-Type'] = 'application/json';
      opts.body = JSON.stringify(body);
    }
    return fetch(path, opts).then(function (r) {
      if (r.status === 204) return null;
      return r.text().then(function (text) {
        var data = null;
        if (text) { try { data = JSON.parse(text); } catch (e) { data = null; } }
        if (!r.ok) {
          var msg = (data && data.error && data.error.message) || (text || ('HTTP ' + r.status));
          var err = new Error(msg);
          err.code = data && data.error && data.error.code;
          err.cycle = data && data.error && data.error.cycle;
          throw err;
        }
        return data;
      });
    });
  }

  // ---- project management ----

  function loadProjects() {
    return api('GET', '/api/projects').then(function (data) {
      state.projects = (data && data.projects) || [];
      renderProjectPicker();
      return state.projects;
    });
  }

  function renderProjectPicker() {
    var sel = $('#project-picker');
    var del = $('#delete-project');
    if (del) del.disabled = state.projects.length === 0;
    sel.innerHTML = '';
    if (state.projects.length === 0) {
      var opt = document.createElement('option');
      opt.textContent = '— no projects —';
      opt.value = '';
      sel.appendChild(opt);
      return;
    }
    state.projects.forEach(function (p) {
      var o = document.createElement('option');
      o.value = String(p.id);
      o.textContent = p.name + ' (' + p.key_prefix + ')';
      if (p.id === state.projectId) o.selected = true;
      sel.appendChild(o);
    });
  }

  function currentProject() {
    for (var i = 0; i < state.projects.length; i++) {
      if (state.projects[i].id === state.projectId) return state.projects[i];
    }
    return null;
  }

  function pickProject(id, push) {
    state.projectId = id;
    try { localStorage.setItem('graphd.project', String(id)); } catch (e) {}
    if (push !== false) {
      var url = new URL(window.location.href);
      url.searchParams.set('p', String(id));
      history.replaceState(null, '', url.toString());
    }
    return refreshAll();
  }

  function newProject() {
    var name = prompt('project name');
    if (!name) return;
    var prefix = prompt('key prefix (e.g. RATE — uppercase, 1-8 chars, letter first)');
    if (!prefix) return;
    api('POST', '/api/projects', { name: name, key_prefix: prefix })
      .then(function (p) {
        return loadProjects().then(function () { return pickProject(p.id); });
      })
      .catch(function (err) { toast(err.message, true); });
  }

  // deleteProject is the one hard delete in the system (README decision 3): the
  // server cascades to every task and edge. The API requires ?confirm=<name>,
  // so the guard is typed, not just clicked — a prompt prefilled with the name
  // is not enough because it would let one Enter destroy a project.
  function deleteProject() {
    var p = currentProject();
    if (!p) { toast('no project selected', true); return; }
    var typed = prompt(
      'Delete project "' + p.name + '" and all of its tasks and edges?\n\n' +
      'This cannot be undone. Type the project name to confirm:');
    if (typed === null) return;
    if (typed.trim() !== p.name) {
      if (typed.trim() !== '') toast('name did not match — nothing deleted', true);
      return;
    }
    api('DELETE', '/api/projects/' + p.id + '?confirm=' + encodeURIComponent(p.name))
      .then(function () {
        // Drop the deleted project from local state before the refetch so
        // pickProject cannot re-select it.
        state.projects = state.projects.filter(function (x) { return x.id !== p.id; });
        state.projectId = null;
        try { localStorage.removeItem('graphd.project'); } catch (e) {}
        if (state.source) { state.source.close(); state.source = null; }
        toast('deleted project "' + p.name + '"');
        return loadProjects().then(function (projects) {
          if (projects.length === 0) {
            if (view) view.setGraph({ project: { id: 0 }, tasks: [], edges: [] });
            $('#status').textContent = '';
            return;
          }
          return pickProject(projects[0].id).then(connectSSE);
        });
      })
      .catch(function (err) { toast(err.message, true); });
  }

  // ---- refresh ----

  // refreshAll refetches graph (+ ready for the frontier readouts) and hands the
  // payload to the active view.
  function refreshAll() {
    if (!state.projectId) {
      if (view) view.setGraph({ project: { id: 0 }, tasks: [], edges: [] });
      return Promise.resolve();
    }
    var pid = state.projectId;
    var graphURL = '/api/projects/' + pid + '/graph' + (state.archived ? '?include_archived=1' : '');
    return Promise.all([
      api('GET', graphURL),
      api('GET', '/api/projects/' + pid + '/ready')
    ]).then(function (res) {
      state.graph = res[0];
      state.ready = res[1];
      if (view) view.setGraph(state.graph);
      updateStatus();
      // Repaint the panel from the fresh payload — but never over a field the
      // user is currently editing. An SSE refresh fires on every write from any
      // process, and repopulating the inputs wholesale would silently discard
      // whatever is being typed. The focused element is left alone; its own blur
      // handler writes it out and the next refresh picks it up.
      if (selectedTaskId != null) populateDetail(selectedTaskId, { preserveFocus: true });
    }).catch(function (err) { toast(err.message, true); });
  }

  // refreshNow is the coalesced refresh used by views and SSE.
  function refreshNow() {
    clearTimeout(state.refreshTimer);
    state.refreshTimer = setTimeout(function () { refreshAll(); }, 40);
  }

  function updateStatus() {
    if (!state.graph) { $('#status').textContent = ''; return; }
    var n = state.graph.tasks.length;
    var r = state.ready ? state.ready.ready.length : 0;
    $('#status').textContent = n + ' tasks · ' + r + ' ready · rev ' + state.graph.revision;
  }

  // ---- SSE ----

  function connectSSE() {
    if (state.source) { state.source.close(); state.source = null; }
    if (!state.projectId) return;
    var src = new EventSource('/api/projects/' + state.projectId + '/events');
    src.addEventListener('changed', function () {
      if (view && view.onExternalChange) view.onExternalChange();
      else refreshNow();
    });
    state.source = src;
  }

  // ---- detail panel ----

  var selectedTaskId = null;
  var notesPreview = true;  // notes render as markdown by default
  var outputPreview = true; // so does output

  // Panel UI state. All of it is remembered across selections and reloads: the
  // panel is a workspace you keep, not a form you fill in once.
  var activePane = 'notes';
  var widePane = null;      // pane expanded to full width, or null
  var detailWidth = 380;    // px, draggable and persisted

  function openDetail(id) {
    var switching = selectedTaskId !== id;
    selectedTaskId = id;
    var panel = $('#detail');
    panel.hidden = false;
    // Opening a *different* task resets the reading position; re-opening the
    // same one (a refresh, a click on the node already selected) leaves the
    // panel exactly as the user arranged it.
    if (switching) {
      setWide(null);
      var panes = $('.panes');
      if (panes) panes.scrollTop = 0;
      // Land on the most useful tab for this task: its output if it produced
      // one, otherwise its notes. That is what you came to read.
      var t = taskById(id);
      if (t && (t.output || '').trim()) setActivePane('output');
      else if (t && (t.notes || '').trim()) setActivePane('notes');
    } else {
      // Re-opening the task already shown must not undo an expansion the user
      // chose — clicking a node to inspect it should leave the panel as they
      // arranged it. Only clear an expansion that belonged to a *different*
      // pane, so the widen button never shows "on" for a pane that is not open.
      if (widePane && widePane !== activePane) setWide(null);
    }
    populateDetail(id);
    if (state.view === 'canvas') GraphdCanvas.selectTask(id);
  }

  function closeDetail() {
    selectedTaskId = null;
    $('#detail').hidden = true;
    if (widePane) setWide(null);
    if (state.view === 'canvas') GraphdCanvas.clearSelection();
  }

  function taskById(id) {
    if (!state.graph) return null;
    for (var i = 0; i < state.graph.tasks.length; i++) {
      if (state.graph.tasks[i].id === id) return state.graph.tasks[i];
    }
    return null;
  }

  // Notes and output are both stored as plain text and rendered as markdown. The
  // renderer escapes raw HTML and filters link schemes, so the result is safe to
  // assign to innerHTML (see markdown.js). The two fields differ in meaning, not
  // in mechanics — notes is what the task must do, output is what it produced —
  // so the same machinery serves both.
  function renderMarkdownInto(el, text, emptyText) {
    if (!el) return;
    if (!text || !text.trim()) {
      el.innerHTML = '<span class="md-empty">' + emptyText + '</span>';
      return;
    }
    el.innerHTML = GraphdMarkdown.render(text);
  }

  function renderNotes(text) {
    renderMarkdownInto($('#detail-notes-rendered'), text, 'no notes');
  }

  // showTextPreview wires one (textarea, rendered-div, edit-btn, preview-btn)
  // group. Returns the current preview flag for that group.
  function showTextPreview(cfg, preview) {
    var ta = $(cfg.textarea), el = $(cfg.rendered);
    if (!ta || !el) return preview;
    if (preview) {
      renderMarkdownInto(el, ta.value, cfg.empty);
      el.hidden = false;
      ta.hidden = true;
    } else {
      el.hidden = true;
      ta.hidden = false;
    }
    var eb = $(cfg.editBtn), pb = $(cfg.previewBtn);
    if (eb) eb.classList.toggle('on', !preview);
    if (pb) pb.classList.toggle('on', preview);
    return preview;
  }

  var notesCfg = {
    textarea: '#detail-notes', rendered: '#detail-notes-rendered',
    editBtn: '#notes-edit-btn', previewBtn: '#notes-preview-btn', empty: 'no notes'
  };
  var outputCfg = {
    textarea: '#detail-output', rendered: '#detail-output-rendered',
    editBtn: '#output-edit-btn', previewBtn: '#output-preview-btn', empty: 'no output recorded'
  };

  function showNotesPreview(preview) { notesPreview = showTextPreview(notesCfg, preview); }
  function showOutputPreview(preview) { outputPreview = showTextPreview(outputCfg, preview); }

  // ---- pane state ----

  function setActivePane(name) {
    activePane = name;
    $$('#detail-tabs .tab').forEach(function (b) {
      b.classList.toggle('on', b.dataset.pane === name);
    });
    $$('.pane').forEach(function (p) {
      p.classList.toggle('on', p.dataset.pane === name);
    });
    if (widePane && widePane !== name) setWide(null);
  }

  // setWide expands one prose pane to full width. It is a *reading* mode: the
  // tabs and the other panes step aside so a long document gets the whole
  // column. Toggling it off restores the normal panel.
  function setWide(name) {
    widePane = name;
    var panel = $('#detail');
    panel.classList.toggle('wide', !!name);
    if (name) setActivePane(name);
    var btn = $('#notes-wide-btn'), obtn = $('#output-wide-btn');
    if (btn) btn.classList.toggle('on', name === 'notes');
    if (obtn) obtn.classList.toggle('on', name === 'output');
  }

  // Tab badges come from panel.js, which holds the pure logic and is covered by
  // panel_test.js. Keeping it out of here is what makes it testable without a
  // DOM.
  function updateTabCounts(t) {
    setText('#count-notes', GraphdPanel.tabCount('notes', t));
    setText('#count-output', GraphdPanel.tabCount('output', t));
    setText('#count-inputs', GraphdPanel.tabCount('inputs', t));
    var links = (t.blocked_by || []).length + countBlocks(t);
    setText('#count-links', links ? String(links) : '');
  }

  function setText(sel, v) { var el = $(sel); if (el) el.textContent = v; }

  function countBlocks(t) {
    var n = 0;
    if (state.graph && state.graph.edges) {
      state.graph.edges.forEach(function (e) { if (e.blocker_id === t.id) n++; });
    }
    return n;
  }

  // populateDetail repaints the panel from the current graph payload.
  //
  // opts.preserveFocus is set by the refresh path: an SSE-driven repaint must
  // not overwrite a field the user is typing into, nor yank the panel back to a
  // different tab while they are reading. Structure (tabs, expansion, scroll) is
  // never reset by a repaint at all — only by opening a different task.
  function populateDetail(id, opts) {
    opts = opts || {};
    var t = taskById(id);
    if (!t) { closeDetail(); return; }
    var active = document.activeElement;
    var editing = active && (active.tagName === 'INPUT' || active.tagName === 'TEXTAREA');
    var keep = opts.preserveFocus && editing;

    $('#detail-key').textContent = t.key + '  #' + t.id;

    if (!keep || active.id !== 'detail-label') $('#detail-label').value = t.label;
    if (!keep || active.id !== 'detail-tags') $('#detail-tags').value = t.tags || '';
    if (!keep || active.id !== 'detail-notes') $('#detail-notes').value = t.notes || '';
    if (!keep || active.id !== 'detail-output') $('#detail-output').value = t.output || '';

    showNotesPreview(notesPreview);
    showOutputPreview(outputPreview);
    updateTabCounts(t);
    renderInputs(t);

    $('#detail-priority').value = String(t.priority);
    $('#d-ready').textContent = t.ready ? 'ready' : 'not ready';
    $('#d-ready').classList.toggle('on', !!t.ready);
    $('#d-unblocks').innerHTML = '&#8635; ' + t.unblocks;
    $('#d-unblocks').classList.toggle('on', t.unblocks > 0);
    $('#d-unblocks').classList.toggle('zero', !t.unblocks);
    $('#d-blast').innerHTML = '&#8709; ' + t.blast_radius;
    $('#d-blast').classList.toggle('zero', !t.blast_radius);
    $('#btn-archive').hidden = t.archived;
    $('#btn-restore').hidden = !t.archived;

    // status as a segmented control
    var wrap = $('#detail-status');
    wrap.innerHTML = '';
    ['todo', 'doing', 'done', 'cancelled'].forEach(function (st) {
      var lab = document.createElement('label');
      lab.className = st === t.status ? 'on' : '';
      var r = document.createElement('input');
      r.type = 'radio';
      r.name = 'status';
      r.value = st;
      r.checked = st === t.status;
      r.addEventListener('change', function () { patchTask(id, { status: st }); });
      lab.appendChild(r);
      lab.appendChild(document.createTextNode(st));
      wrap.appendChild(lab);
    });

    renderEdgeLists(t);
  }

  // renderInputs shows the derived inputs: the outputs of this task's immediate
  // blockers, computed server-side (t.inputs). It is read-only — a dependent
  // never edits what an upstream task produced; it edits its own output, and
  // that is what its own dependents see.
  function renderInputs(t) {
    var wrap = $('#d-inputs');
    if (!wrap) return;
    wrap.innerHTML = '';
    var inputs = t.inputs || [];
    if (!inputs.length) {
      var none = document.createElement('div');
      none.className = 'md-empty';
      none.textContent = t.blocked_by && t.blocked_by.length
        ? 'blockers have recorded no output'
        : 'no upstream inputs';
      wrap.appendChild(none);
      return;
    }
    inputs.forEach(function (inp) {
      var item = document.createElement('div');
      item.className = 'input-item';

      var head = document.createElement('div');
      head.className = 'input-head';
      head.title = 'open ' + inp.key;
      var k = document.createElement('span');
      k.className = 'key';
      k.textContent = inp.key;
      var st = document.createElement('span');
      st.className = 'chip';
      st.textContent = inp.status;
      var lab = document.createElement('span');
      lab.className = 'input-label';
      lab.textContent = truncate(inp.label, 40);
      head.appendChild(k);
      head.appendChild(st);
      head.appendChild(lab);
      head.addEventListener('click', function () { openDetail(inp.id); });

      var body = document.createElement('div');
      body.className = 'input-body';
      renderMarkdownInto(body, inp.output, 'no output recorded');

      item.appendChild(head);
      item.appendChild(body);
      if (inp.truncated) {
        var tr = document.createElement('div');
        tr.className = 'input-truncated';
        tr.textContent = 'truncated — open ' + inp.key + ' for the full output';
        item.appendChild(tr);
      }
      wrap.appendChild(item);
    });
  }

  function renderEdgeLists(t) {
    var byId = {};
    state.graph.tasks.forEach(function (x) { byId[x.id] = x; });

    var blockedBy = t.blocked_by || [];
    var blocks = [];
    state.graph.edges.forEach(function (e) {
      if (e.blocker_id === t.id) blocks.push(e.blocked_id);
    });

    fillEdgeList($('#d-blockedby'), blockedBy, byId, function (otherId) {
      api('DELETE', '/api/edges/' + edgeIdBetween(otherId, t.id));
    });
    fillEdgeList($('#d-blocks'), blocks, byId, function (otherId) {
      api('DELETE', '/api/edges/' + edgeIdBetween(t.id, otherId));
    });

    fillPicker($('#add-blocker'), state.graph.tasks, t.id, blockedBy);
    fillPicker($('#add-blocked'), state.graph.tasks, t.id, blocks);

    $('#add-blocker-btn').onclick = function () {
      var other = parseInt($('#add-blocker').value, 10);
      if (isNaN(other)) return;
      addEdge(other, t.id);
    };
    $('#add-blocked-btn').onclick = function () {
      var other = parseInt($('#add-blocked').value, 10);
      if (isNaN(other)) return;
      addEdge(t.id, other);
    };
  }

  function edgeIdBetween(blocker, blocked) {
    var found = null;
    state.graph.edges.forEach(function (e) {
      if (e.blocker_id === blocker && e.blocked_id === blocked) found = e.id;
    });
    return found;
  }

  function fillEdgeList(ul, ids, byId, onRemove) {
    ul.innerHTML = '';
    if (!ids.length) {
      var li = document.createElement('li');
      li.className = 'edge-empty';
      li.textContent = 'none';
      ul.appendChild(li);
      return;
    }
    ids.forEach(function (oid) {
      var other = byId[oid];
      var li = document.createElement('li');
      var name = document.createElement('span');
      name.className = 'edge-name';
      name.textContent = other ? (other.key + ' ' + truncate(other.label, 40)) : ('#' + oid);
      name.addEventListener('click', function () { openDetail(oid); });
      var rm = document.createElement('button');
      rm.className = 'rm';
      rm.textContent = '\u2715';
      rm.title = 'remove edge';
      rm.addEventListener('click', function () {
        Promise.resolve(onRemove(oid)).then(refreshNow).catch(function (e) { toast(e.message, true); });
      });
      li.appendChild(name);
      li.appendChild(rm);
      ul.appendChild(li);
    });
  }

  function fillPicker(sel, tasks, excludeId, already) {
    var have = {};
    (already || []).forEach(function (id) { have[id] = true; });
    sel.innerHTML = '';
    tasks
      .filter(function (x) { return x.id !== excludeId && !have[x.id] && !x.archived; })
      .sort(function (a, b) { return a.id - b.id; })
      .forEach(function (x) {
        var o = document.createElement('option');
        o.value = String(x.id);
        o.textContent = x.key + ' ' + truncate(x.label, 44);
        sel.appendChild(o);
      });
  }

  // Truncation goes through GraphdPanel.clamp so a cut never lands inside a
  // surrogate pair. A plain slice can split an emoji and leave a lone surrogate,
  // which renders as a replacement glyph in the middle of a label.
  function truncate(s, n) {
    if (!s) return '';
    return GraphdPanel.clamp(s, n);
  }

  function addEdge(blocker, blocked) {
    api('POST', '/api/projects/' + state.projectId + '/edges', {
      blocker_id: blocker, blocked_id: blocked, label: ''
    }).then(refreshNow).catch(function (err) {
      $('#detail-error').textContent = err.message;
      toast(err.message, true);
    });
  }

  function patchTask(id, patch) {
    api('PATCH', '/api/tasks/' + id, patch)
      .then(function () { $('#detail-error').textContent = ''; refreshNow(); })
      .catch(function (err) {
        $('#detail-error').textContent = err.message;
        toast(err.message, true);
      });
  }

  function wireDetail() {
    $('#detail-close').onclick = closeDetail;
    $('#detail-label').addEventListener('blur', function () {
      if (selectedTaskId != null) patchTask(selectedTaskId, { label: $('#detail-label').value });
    });
    $('#detail-notes').addEventListener('blur', function () {
      if (selectedTaskId != null) patchTask(selectedTaskId, { notes: $('#detail-notes').value });
    });
    $('#detail-output').addEventListener('blur', function () {
      if (selectedTaskId != null) patchTask(selectedTaskId, { output: $('#detail-output').value });
    });

    // tabs
    $$('#detail-tabs .tab').forEach(function (b) {
      b.onclick = function () { setActivePane(b.dataset.pane); };
    });

    // prose pane full-width toggle
    $('#notes-wide-btn').onclick = function () { setWide(widePane === 'notes' ? null : 'notes'); };
    $('#output-wide-btn').onclick = function () { setWide(widePane === 'output' ? null : 'output'); };

    $('#notes-edit-btn').onclick = function () {
      // Entering edit mode flushes whatever is in the textarea first.
      showNotesPreview(false);
    };
    $('#notes-preview-btn').onclick = function () {
      if (selectedTaskId != null) {
        var v = $('#detail-notes').value;
        var t = taskById(selectedTaskId);
        if (!t || t.notes !== v) patchTask(selectedTaskId, { notes: v });
      }
      showNotesPreview(true);
    };
    $('#output-edit-btn').onclick = function () {
      showOutputPreview(false);
    };
    $('#output-preview-btn').onclick = function () {
      if (selectedTaskId != null) {
        var v = $('#detail-output').value;
        var t = taskById(selectedTaskId);
        if (!t || t.output !== v) patchTask(selectedTaskId, { output: v });
      }
      showOutputPreview(true);
    };
    $('#detail-priority').addEventListener('change', function () {
      if (selectedTaskId != null) patchTask(selectedTaskId, { priority: parseInt($('#detail-priority').value, 10) });
    });
    $('#detail-tags').addEventListener('blur', function () {
      if (selectedTaskId != null) patchTask(selectedTaskId, { tags: $('#detail-tags').value });
    });
    wireDetailWidth();
    $('#btn-archive').onclick = function () {
      if (selectedTaskId == null) return;
      if (!confirm('archive this task? it becomes cancelled and unblocks whatever it was blocking.')) return;
      api('POST', '/api/tasks/' + selectedTaskId + '/archive').then(refreshNow).catch(function (e) { toast(e.message, true); });
    };
    $('#btn-restore').onclick = function () {
      if (selectedTaskId == null) return;
      api('POST', '/api/tasks/' + selectedTaskId + '/restore').then(refreshNow).catch(function (e) { toast(e.message, true); });
    };
  }

  // ---- panel width ----

  // The grip drags the panel's left edge. Width persists in localStorage so the
  // panel stays the size you made it; that is the cheapest possible answer to
  // "the sidebar is too narrow for this content".
  function applyDetailWidth(px) {
    var panel = $('#detail');
    if (!panel) return;
    detailWidth = Math.max(280, Math.min(Math.round(window.innerWidth * 0.9), px));
    panel.style.width = detailWidth + 'px';
  }

  function wireDetailWidth() {
    try {
      var saved = parseInt(localStorage.getItem('graphd.detailWidth'), 10);
      if (!isNaN(saved)) detailWidth = saved;
    } catch (e) {}
    applyDetailWidth(detailWidth);

    var grip = $('#detail-grip');
    if (!grip) return;
    var dragging = false;
    grip.addEventListener('mousedown', function (e) {
      e.preventDefault();
      dragging = true;
      document.body.style.cursor = 'col-resize';
      document.body.style.userSelect = 'none';
    });
    document.addEventListener('mousemove', function (e) {
      if (!dragging) return;
      applyDetailWidth(window.innerWidth - e.clientX);
    });
    document.addEventListener('mouseup', function () {
      if (!dragging) return;
      dragging = false;
      document.body.style.cursor = '';
      document.body.style.userSelect = '';
      try { localStorage.setItem('graphd.detailWidth', String(detailWidth)); } catch (e) {}
    });
  }

  // ---- orientation ----

  // The orientation toggle is a rank-direction choice for the layout engine:
  // horizontal keeps blockers on the left (the spec default, rankDir LR),
  // vertical stacks blockers above their dependents (rankDir TB). It persists
  // in localStorage like the engine picker, and re-runs the layout when the
  // canvas is showing so the change is visible immediately.
  function setOrientation(dir, opts) {
    opts = opts || {};
    if (dir !== 'LR' && dir !== 'TB') dir = 'LR';
    state.orientation = dir;
    try { localStorage.setItem('graphd.orientation', dir); } catch (e) {}
    renderOrientation();
    if (opts.relayout !== false && state.view === 'canvas' && state.graph) {
      GraphdCanvas.runLayout();
    }
  }

  function toggleOrientation() {
    setOrientation(state.orientation === 'LR' ? 'TB' : 'LR');
  }

  function renderOrientation() {
    var btn = $('#btn-orient');
    if (!btn) return;
    var vertical = state.orientation === 'TB';
    btn.innerHTML = (vertical ? '\u2195' : '\u2194') + ' ' + (vertical ? 'vertical' : 'horizontal');
    btn.title = 'layout orientation: ' + (vertical ? 'vertical (top\u2192bottom)' : 'horizontal (left\u2192right)') +
      ' \u2014 click to switch (O)';
  }

  // ---- header wiring ----

  function wireHeader() {
    $('#project-picker').addEventListener('change', function (e) {
      var id = parseInt(e.target.value, 10);
      if (!isNaN(id)) pickProject(id).then(connectSSE);
    });
    $('#new-project').onclick = newProject;
    $('#delete-project').onclick = deleteProject;
    $('#nav-canvas').onclick = function (e) { e.preventDefault(); switchView('canvas'); };
    $('#nav-board').onclick = function (e) { e.preventDefault(); switchView('board'); };
    $('#search').addEventListener('input', function (e) {
      if (state.view === 'canvas') GraphdCanvas.setFilter(e.target.value);
    });
    $('#show-archived').addEventListener('change', function (e) {
      state.archived = e.target.checked;
      refreshNow();
    });
    $('#engine-picker').addEventListener('change', function (e) {
      state.engine = e.target.value;
      try { localStorage.setItem('graphd.engine', state.engine); } catch (err) {}
    });
    $('#lens-picker').addEventListener('change', function (e) {
      if (state.view === 'canvas') GraphdCanvas.setLens(e.target.value);
    });
    $('#btn-orient').onclick = toggleOrientation;
    $('#btn-layout').onclick = function () { if (state.view === 'canvas') GraphdCanvas.runLayout(); };
    $('#btn-fit').onclick = function () { if (state.view === 'canvas') GraphdCanvas.fit(); };
    $('#btn-undo').onclick = function () { if (state.view === 'canvas') GraphdCanvas.undo(); };
    $('#btn-refresh').onclick = function () { refreshAll(); };
  }

  function switchView(name) {
    if (state.view === name) return;
    var url = new URL(window.location.href);
    url.pathname = '/' + name;
    window.location.href = url.toString();
  }

  function setUndoEnabled(on) {
    var b = $('#btn-undo');
    if (b) b.disabled = !on;
  }

  function setDirty(v) {
    state.dirty = v;
    if (state.view === 'canvas') GraphdCanvas.setDirty(v);
  }

  // cyclePane moves between the four tabs with [ and ]. The ring itself lives in
  // panel.js so its wrap-around is covered by a test.
  function cyclePane(delta) {
    setActivePane(GraphdPanel.nextPane(activePane, delta));
  }

  function wireKeys() {
    document.addEventListener('keydown', function (e) {
      var tag = (e.target.tagName || '').toLowerCase();
      if (tag === 'input' || tag === 'textarea' || tag === 'select') return;
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'z') {
        e.preventDefault();
        if (state.view === 'canvas') GraphdCanvas.undo();
        return;
      }
      // Panel keys work in both views — the panel is shared.
      if (e.key === 'Escape') {
        if (widePane) setWide(null);
        else if (!$('#detail').hidden) closeDetail();
        return;
      }
      if (e.key === '[') { cyclePane(-1); return; }
      if (e.key === ']') { cyclePane(1); return; }
      if (e.key === 'l' || e.key === 'L') { if (state.view === 'canvas') GraphdCanvas.runLayout(); }
      else if (e.key === 'o' || e.key === 'O') { if (state.view === 'canvas') toggleOrientation(); }
      else if (e.key === 'f' || e.key === 'F') { if (state.view === 'canvas') GraphdCanvas.fit(); }
      else if (e.key === 'Delete' || e.key === 'Backspace') {
        var sel = state.view === 'canvas' ? GraphdCanvas.getSelected() : selectedTaskId;
        if (sel != null) {
          if (confirm('archive this task?')) {
            api('POST', '/api/tasks/' + sel + '/archive').then(refreshNow).catch(function (err) { toast(err.message, true); });
          }
        }
      }
    });
  }

  // ---- boot ----

  function boot() {
    wireHeader();
    wireDetail();
    wireKeys();
    setActivePane(activePane);

    var params = new URLSearchParams(window.location.search);
    var wantId = parseInt(params.get('p'), 10);
    if (isNaN(wantId)) {
      var saved = null;
      try { saved = parseInt(localStorage.getItem('graphd.project'), 10); } catch (e) {}
      if (!isNaN(saved)) wantId = saved;
    }
    try {
      var eng = localStorage.getItem('graphd.engine');
      if (eng) state.engine = eng;
    } catch (e) {}
    $('#engine-picker').value = state.engine;

    try {
      var orient = localStorage.getItem('graphd.orientation');
      if (orient === 'LR' || orient === 'TB') state.orientation = orient;
    } catch (e) {}
    renderOrientation();

    // Mark the active nav link.
    var nav = $('#nav-' + state.view);
    if (nav) nav.classList.add('active');

    var app = $('#app');
    if (state.view === 'canvas') {
      view = GraphdCanvas;
      view.mount(app, appContext());
    } else {
      view = GraphdBoard;
      view.mount(app, appContext());
    }

    loadProjects().then(function (projects) {
      if (projects.length === 0) {
        toast('no projects yet — create one with "+ new project"');
        if (view) view.setGraph({ project: { id: 0 }, tasks: [], edges: [] });
        return;
      }
      var exists = projects.some(function (p) { return p.id === wantId; });
      var id = exists ? wantId : projects[0].id;
      return pickProject(id, false).then(connectSSE);
    });
  }

  function appContext() {
    return {
      api: api,
      toast: toast,
      refreshNow: refreshNow,
      refreshAll: refreshAll,
      openDetail: openDetail,
      clearSelection: closeDetail,
      selectEdge: function (eid) { openDetailEdge(eid); },
      engine: function () { return state.engine; },
      orientation: function () { return state.orientation; },
      showArchived: function () { return state.archived; },
      setUndoEnabled: setUndoEnabled,
      setDirty: setDirty
    };
  }

  function openDetailEdge(eid) {
    var e = null;
    (state.graph.edges || []).forEach(function (x) { if (x.id === eid) e = x; });
    if (!e) return;
    var byId = {};
    state.graph.tasks.forEach(function (t) { byId[t.id] = t; });
    var b = byId[e.blocker_id], d = byId[e.blocked_id];
    var msg = 'edge: ' + (b ? b.key : e.blocker_id) + ' \u2192 ' + (d ? d.key : e.blocked_id) +
      (e.satisfied ? '  (satisfied)' : '  (open)') + '\n\nRemove this edge?';
    if (confirm(msg)) {
      if (state.view === 'canvas') GraphdCanvas.deleteEdge(eid);
      else api('DELETE', '/api/edges/' + eid).then(refreshNow).catch(function (err) { toast(err.message, true); });
    }
  }

  global.GraphdApp = { boot: boot, api: api, toast: toast };
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot);
  else boot();
})(window);
