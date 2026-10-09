/* graphd — canvas view. Cytoscape rendering, detail panel, edgehandles, undo.
 *
 * The client is dumb on purpose (SPEC §6.1): ready, satisfied and
 * blocked_by_open are rendered exactly as the server sends them. No graph
 * traversal happens in this file.
 */
(function (global) {
  'use strict';

  var L = global.GraphdLayout;
  var ACCENT = '#5ad1a0';

  // ---- module state ----
  var cy = null;                 // cytoscape instance
  var eh = null;                 // edgehandles instance
  var graph = null;              // last payload from the server
  var app = null;                // the shared app context
  var dirty = false;             // an interaction is in flight; defer refresh
  var pendingRefresh = false;
  var selected = null;           // selected task id
  var undoStack = [];            // canvas ops only, capped at 50
  var filterText = '';
  var lens = 'none';
  var saveTimer = null;

  // ---- style ----

  // Node state is expressed as classes and rendered from the stylesheet, so the
  // styling lives in one place and nothing depends on per-element inline style.
  //
  //   todo      neutral fill, solid 1px border
  //   doing     amber fill, 2px border
  //   done      green tint, 60% opacity
  //   cancelled grey, dashed border, 40% opacity
  //   ready     3px accent ring, subtle outer glow   (from the server's `ready`)
  //   blocked   subtle red outline                   (from `blocked_by_open`)
  //
  // State is carried by the node's own shape and colour, with no glyph overlay:
  // a corner badge or a lock icon is a second thing to read at a zoom level
  // where the label is already marginal, and the node outline says the same
  // thing without competing with the text.
  function classesFor(t) {
    var cls = ['st-' + t.status];
    if (t.ready) cls.push('ready');
    // `blocked` means "cannot start", so it belongs to `todo` alone: a task you
    // have already started is not blocked, and a red outline would otherwise
    // fight the amber `doing` border for the same edge.
    if (t.status === 'todo' && t.blocked_by_open && t.blocked_by_open.length > 0) cls.push('blocked');
    // human-owned work carries a different shape, not an icon: the outline and
    // fill already carry state, so ownership rides on geometry instead.
    if (t.owner === 'human') cls.push('human');
    return cls.join(' ');
  }

  // ---- mount ----

  function mount(container, ctx) {
    app = ctx;
    container.innerHTML = '<div id="cy"></div>';
  }

  function ensureCy() {
    if (cy) return cy;
    var host = document.getElementById('cy');
    if (!host) return null;
    cy = cytoscape({
      container: host,
      wheelSensitivity: 0.2,
      boxSelectionEnabled: true,
      selectionType: 'single',
      minZoom: 0.1,
      maxZoom: 3,
      style: [
        {
          selector: 'node',
          style: {
            'width': L.NODE_W,
            'height': L.NODE_H,
            'shape': 'round-rectangle',
            'label': 'data(label)',
            'color': '#d6dae3',
            'font-family': 'system-ui, -apple-system, Segoe UI, Roboto, sans-serif',
            'font-size': 11,
            'text-wrap': 'wrap',
            'text-max-width': '164px',
            'text-valign': 'center',
            'text-halign': 'center',
            'text-overflow-wrap': 'anywhere',
            'border-width': 1,
            'border-color': '#3a4150',
            'background-color': '#232733'
          }
        },
        // state classes (see classesFor)
        { selector: 'node.st-doing', style: { 'background-color': '#3a2f18', 'border-color': '#d7a24a', 'border-width': 2 } },
        { selector: 'node.st-done', style: { 'background-color': '#1d2c22', 'border-color': '#4f9e6a', 'opacity': 0.6 } },
        { selector: 'node.st-cancelled', style: { 'background-color': '#22242a', 'border-color': '#4a5060', 'border-style': 'dashed', 'opacity': 0.4 } },
        { selector: 'node.ready', style: { 'border-color': ACCENT, 'border-width': 3, 'shadow-blur': 14, 'shadow-color': ACCENT, 'shadow-opacity': 0.35 } },
        { selector: 'node.blocked', style: { 'border-color': '#c25b5b', 'border-width': 2 } },
        // Human-owned work is drawn as a cut-corner box: a shape difference is
        // legible at a zoom where a glyph on a 180x60 node is not, and it needs
        // no div layer to keep in sync with the canvas.
        { selector: 'node.human', style: { 'shape': 'cut-rectangle', 'border-style': 'dashed' } },
        { selector: 'node:selected', style: { 'border-color': ACCENT, 'border-width': 3 } },
        { selector: 'node.dimmed', style: { 'opacity': 0.18 } },
        { selector: 'node.hit', style: { 'border-color': ACCENT } },
        { selector: 'node.hidden-lens', style: { 'display': 'none' } },
        {
          selector: 'edge',
          style: {
            'width': 1.4,
            'line-color': '#4a5060',
            'target-arrow-color': '#4a5060',
            'target-arrow-shape': 'triangle',
            'curve-style': 'bezier',
            'arrow-scale': 0.9,
            'label': 'data(label)',
            'font-size': 9,
            'color': '#8b93a3',
            'text-background-color': '#12141a',
            'text-background-opacity': 0.85,
            'text-background-padding': '1px'
          }
        },
        {
          selector: 'edge.satisfied',
          style: { 'line-style': 'dashed', 'line-color': '#2c6a52', 'target-arrow-color': '#2c6a52', 'opacity': 0.7 }
        },
        { selector: 'edge:selected', style: { 'line-color': ACCENT, 'target-arrow-color': ACCENT, 'width': 2 } },
        { selector: 'edge.dimmed', style: { 'opacity': 0.1 } }
      ]
    });

    eh = cy.edgehandles({
      snap: true,
      snapThreshold: 30,
      snapFrequency: 15,
      noEdgeEventsInDraw: true,
      disableBrowserGestures: true,
      handleNodes: 'node',
      edgeParams: function () { return { data: { label: '' } }; }
    });

    // ---- interactions ----

    cy.on('ehcomplete', function (event, source, target) {
      var blocker = idOf(source), blocked = idOf(target);
      if (blocker == null || blocked == null) return;
      createEdge(blocker, blocked);
    });

    cy.on('dragfree', 'node', function (evt) {
      var n = evt.target;
      var id = idOf(n);
      var p = n.position();
      var before = n.data('pos') || { x: 0, y: 0 };
      // Only record an undo entry if the node actually moved.
      if (Math.abs(before.x - p.x) > 0.5 || Math.abs(before.y - p.y) > 0.5) {
        pushUndo({
          label: 'move ' + n.data('key'),
          undo: function () {
            var nn = cy.getElementById('n' + id);
            if (nn.empty()) return;
            nn.position(before);
            savePositions([{ id: id, x: before.x, y: before.y }]);
          }
        });
      }
      n.data('pos', { x: p.x, y: p.y });
      scheduleSave(id, p.x, p.y);
    });

    cy.on('tap', 'node', function (evt) {
      // Open the panel; app.openDetail calls back into selectTask to highlight.
      app.openDetail(idOf(evt.target));
    });

    cy.on('tap', 'edge', function (evt) {
      var eid = evt.target.data('edgeId');
      app.selectEdge(eid);
    });

    cy.on('tap', function (evt) {
      if (evt.target === cy) app.clearSelection();
    });

    // Track drag start/finish on the background too, so a marquee drag defers
    // refresh.
    cy.on('boxstart', function () { setDirty(true); });
    cy.on('boxend', function () { setDirty(false); });

    return cy;
  }

  function idOf(ele) {
    var d = ele.data('id');
    return typeof d === 'number' ? d : parseInt(String(d).replace(/^n/, ''), 10);
  }

  // ---- render ----

  function setGraph(payload) {
    graph = payload;
    if (!ensureCy()) return;
    var viewport = { pan: cy.pan(), zoom: cy.zoom() };
    var hadViewport = cy.elements().length > 0;

    cy.batch(function () {
      cy.elements().remove();
      var tasks = (graph.tasks || []).slice().sort(function (a, b) { return a.id - b.id; });
      var grid = gridPositions(tasks);

      tasks.forEach(function (t) {
        var pos = (t.x != null && t.y != null) ? { x: t.x, y: t.y } : grid.get(t.id);
        cy.add({
          group: 'nodes',
          data: {
            id: 'n' + t.id,
            taskId: t.id,
            key: t.key,
            label: labelFor(t),
            task: t,
            pos: { x: pos.x, y: pos.y }
          },
          position: pos,
          classes: classesFor(t)
        });
      });

      (graph.edges || []).forEach(function (e) {
        if (cy.getElementById('n' + e.blocker_id).empty()) return;
        if (cy.getElementById('n' + e.blocked_id).empty()) return;
        cy.add({
          group: 'edges',
          data: {
            id: 'e' + e.id,
            edgeId: e.id,
            source: 'n' + e.blocker_id,
            target: 'n' + e.blocked_id,
            label: e.label || ''
          },
          classes: e.satisfied ? 'satisfied' : ''
        });
      });
    });

    // Restore viewport (SPEC §7.5) — losing it on every agent write is the
    // fastest way to make the tool unusable.
    if (hadViewport) {
      cy.zoom(viewport.zoom);
      cy.pan(viewport.pan);
    } else {
      fit();
    }
    applyFilter();
    applyLens();
    if (selected != null) highlightSelection();
  }

  function labelFor(t) {
    return t.key + '\n' + t.label;
  }

  // Unplaced nodes are arranged in a grid client-side and NOT persisted, until
  // the user drags one or presses Layout (SPEC §7.3).
  function gridPositions(tasks) {
    var cols = Math.max(1, Math.ceil(Math.sqrt(tasks.length)));
    var gapX = L.NODE_W + 40, gapY = L.NODE_H + 40;
    var m = new Map();
    var i = 0;
    tasks.forEach(function (t) {
      var col = i % cols, row = Math.floor(i / cols);
      m.set(t.id, { x: col * gapX, y: row * gapY });
      i++;
    });
    return m;
  }

  // ---- layout ----

  function runLayout() {
    if (!cy || !graph) return;
    var engine = L.get(app.engine());
    var before = snapshotPositions();
    // rankDir is an opts override, not a second engine: the registry interface
    // is unchanged (SPEC §7.3), and the orientation toggle just picks LR or TB.
    var positions = engine.layout(
      { tasks: graph.tasks, edges: graph.edges },
      { rankDir: app.orientation ? app.orientation() : 'LR' });
    cy.batch(function () {
      positions.forEach(function (p, id) {
        var n = cy.getElementById('n' + id);
        if (!n.empty()) {
          n.position(p);
          n.data('pos', p);
        }
      });
    });
    var payload = [];
    positions.forEach(function (p, id) { payload.push({ id: id, x: p.x, y: p.y }); });
    savePositions(payload);
    pushUndo({
      label: 'layout (' + engine.id + ')',
      undo: function () {
        cy.batch(function () {
          before.forEach(function (p, id) {
            var n = cy.getElementById('n' + id);
            if (!n.empty()) { n.position(p); n.data('pos', p); }
          });
        });
        var b = [];
        before.forEach(function (p, id) { b.push({ id: id, x: p.x, y: p.y }); });
        savePositions(b);
      }
    });
    fit();
  }

  function snapshotPositions() {
    var m = new Map();
    if (!cy) return m;
    cy.nodes().forEach(function (n) { m.set(idOf(n), n.position()); });
    return m;
  }

  function fit() {
    if (!cy || cy.elements().length === 0) return;
    cy.fit(cy.elements(), 40);
  }

  // ---- persistence ----

  function scheduleSave(id, x, y) {
    clearTimeout(saveTimer);
    saveTimer = setTimeout(function () {
      savePositions([{ id: id, x: x, y: y }]);
    }, 300);
  }

  function savePositions(list) {
    if (!list.length || !graph) return Promise.resolve();
    return app.api('POST', '/api/projects/' + graph.project.id + '/positions', list)
      .catch(function (err) { app.toast('save position failed: ' + err.message, true); });
  }

  // ---- edge creation / deletion ----

  function createEdge(blocker, blocked) {
    var pid = graph.project.id;
    return app.api('POST', '/api/projects/' + pid + '/edges', {
      blocker_id: blocker, blocked_id: blocked, label: ''
    }).then(function (edge) {
      pushUndo({
        label: 'create edge',
        undo: function () { app.api('DELETE', '/api/edges/' + edge.id); }
      });
      app.refreshNow();
    }).catch(function (err) {
      // A cycle comes back with the offending path; showing it verbatim is the
      // point (SPEC §6.2).
      app.toast(err.message, true);
    });
  }

  function deleteEdge(eid) {
    var e = (graph.edges || []).filter(function (x) { return x.id === eid; })[0];
    app.api('DELETE', '/api/edges/' + eid).then(function () {
      if (e) {
        pushUndo({
          label: 'delete edge',
          undo: function () {
            app.api('POST', '/api/projects/' + graph.project.id + '/edges', {
              blocker_id: e.blocker_id, blocked_id: e.blocked_id, label: e.label || ''
            });
          }
        });
      }
      app.refreshNow();
    }).catch(function (err) { app.toast(err.message, true); });
  }

  // ---- selection + detail panel ----

  // selectTask only highlights. The panel is opened by the caller, so this is
  // safe to call from app.openDetail without recursing.
  function selectTask(id) {
    selected = id;
    highlightSelection();
  }

  function highlightSelection() {
    if (!cy) return;
    cy.nodes().unselect();
    if (selected != null) {
      var n = cy.getElementById('n' + selected);
      if (!n.empty()) n.select();
    }
  }

  // ---- filters and lenses ----

  function setFilter(text) {
    filterText = (text || '').trim().toLowerCase();
    applyFilter();
  }

  function applyFilter() {
    if (!cy) return;
    cy.nodes().removeClass('dimmed hit');
    if (!filterText) return;
    cy.nodes().forEach(function (n) {
      var t = n.data('task');
      var hay = (t.key + ' ' + t.label + ' ' + (t.tags || '')).toLowerCase();
      if (hay.indexOf(filterText) >= 0) n.addClass('hit');
      else n.addClass('dimmed');
    });
    cy.edges().forEach(function (e) {
      var s = e.source(), t = e.target();
      if (s.hasClass('dimmed') || t.hasClass('dimmed')) e.addClass('dimmed');
    });
  }

  // Lenses (SPEC M6): ready-frontier only, a node's blast radius, the connected
  // component of the selection. Pure display; nothing is persisted.
  function setLens(name) {
    lens = name || 'none';
    applyLens();
  }

  function applyLens() {
    if (!cy) return;
    cy.nodes().removeClass('hidden-lens dimmed');
    cy.edges().removeClass('hidden-lens dimmed');
    if (lens === 'none') { applyFilter(); return; }

    var keep = {};
    if (lens === 'ready') {
      cy.nodes().forEach(function (n) { if (n.data('task').ready) keep[idOf(n)] = true; });
    } else if (lens === 'human') {
      // The human's own queue: every node marked for human consumption,
      // whatever its status, so a plan's manual steps can be seen at once.
      cy.nodes().forEach(function (n) { if (n.data('task').owner === 'human') keep[idOf(n)] = true; });
    } else if (lens === 'blast' && selected != null) {
      // The server already told us the radius count; for the *display* we walk
      // forward through the rendered edges. This is the one place the client
      // follows the graph, and it is a view filter, not logic.
      keep[selected] = true;
      var frontier = [selected];
      while (frontier.length) {
        var cur = frontier.shift();
        (graph.edges || []).forEach(function (e) {
          if (e.blocker_id === cur && !keep[e.blocked_id]) {
            keep[e.blocked_id] = true;
            frontier.push(e.blocked_id);
          }
        });
      }
    } else if (lens === 'component' && selected != null) {
      keep[selected] = true;
      var q = [selected];
      while (q.length) {
        var c = q.shift();
        (graph.edges || []).forEach(function (e) {
          var other = null;
          if (e.blocker_id === c) other = e.blocked_id;
          else if (e.blocked_id === c) other = e.blocker_id;
          if (other != null && !keep[other]) { keep[other] = true; q.push(other); }
        });
      }
    }
    cy.nodes().forEach(function (n) {
      if (!keep[idOf(n)]) n.addClass('hidden-lens');
    });
    cy.edges().forEach(function (e) {
      if (!keep[idOf(e.source())] || !keep[idOf(e.target())]) e.addClass('hidden-lens');
    });
  }

  // ---- undo (canvas ops only, session-only, capped at 50) ----

  function pushUndo(entry) {
    undoStack.push(entry);
    if (undoStack.length > 50) undoStack.shift();
    app.setUndoEnabled(undoStack.length > 0);
  }

  function undo() {
    var entry = undoStack.pop();
    if (!entry) return;
    try { entry.undo(); } catch (e) { app.toast('undo failed: ' + e.message, true); }
    app.setUndoEnabled(undoStack.length > 0);
    app.refreshNow();
  }

  function noteCreatedTask(task) {
    pushUndo({
      label: 'create ' + task.key,
      undo: function () { app.api('POST', '/api/tasks/' + task.id + '/archive'); }
    });
  }

  // ---- dirty flag (SPEC §7.5) ----

  function setDirty(v) {
    dirty = v;
    if (!dirty && pendingRefresh) {
      pendingRefresh = false;
      app.refreshNow();
    }
  }

  function onExternalChange() {
    if (dirty) { pendingRefresh = true; return; }
    app.refreshNow();
  }

  function destroy() {
    if (cy) { cy.destroy(); cy = null; eh = null; }
  }

  function getSelected() { return selected; }
  function getGraph() { return graph; }

  global.GraphdCanvas = {
    mount: mount,
    setGraph: setGraph,
    destroy: destroy,
    runLayout: runLayout,
    fit: fit,
    undo: undo,
    setFilter: setFilter,
    setLens: setLens,
    selectTask: selectTask,
    deleteEdge: deleteEdge,
    onExternalChange: onExternalChange,
    setDirty: setDirty,
    noteCreatedTask: noteCreatedTask,
    getSelected: getSelected,
    getGraph: getGraph,
    clearSelection: function () { selected = null; highlightSelection(); }
  };
})(window);
