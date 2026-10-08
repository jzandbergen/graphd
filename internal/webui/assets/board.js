/* graphd — board view. Server-rendered fragments, swapped wholesale on every
 * SSE revision change. Drag-and-drop is plain HTML5 drag events; there is no
 * client-side rendering framework here on purpose (SPEC §7.4).
 */
(function (global) {
  'use strict';

  var app = null;
  var root = null;
  var graph = null;
  var draggingId = null;
  var dirty = false;          // a card drag is in flight; defer refresh
  var pendingRefresh = false;

  function mount(container, ctx) {
    app = ctx;
    root = container;
    root.innerHTML = '<div class="board-loading">loading…</div>';
  }

  function setGraph(payload) {
    graph = payload;
    refresh();
  }

  // The board fragment endpoint takes include_archived; the header toggle drives
  // it. Re-fetch on every revision change.
  function refresh() {
    if (!graph || !root) return;
    var url = '/api/projects/' + graph.project.id + '/board' +
      (app.showArchived() ? '?include_archived=1' : '');
    fetch(url, { headers: { 'Accept': 'text/html' } })
      .then(function (r) {
        if (!r.ok) throw new Error('board fetch failed: ' + r.status);
        return r.text();
      })
      .then(function (html) {
        root.innerHTML = html;
        wire();
        app.setDirty(false);
      })
      .catch(function (err) { app.toast(err.message, true); });
  }

  function wire() {
    var cards = root.querySelectorAll('.card');
    cards.forEach(function (card) {
      card.addEventListener('dragstart', function (e) {
        draggingId = parseInt(card.dataset.id, 10);
        card.classList.add('dragging');
        dirty = true;
        app.setDirty(true);
        e.dataTransfer.effectAllowed = 'move';
        // Firefox requires data to be set for a drag to start at all.
        e.dataTransfer.setData('text/plain', String(draggingId));
      });
      card.addEventListener('dragend', function () {
        card.classList.remove('dragging');
        root.querySelectorAll('.column-body').forEach(function (b) { b.classList.remove('drop-target'); });
        dirty = false;
        app.setDirty(false);
        if (pendingRefresh) { pendingRefresh = false; refresh(); }
      });
      card.addEventListener('click', function () {
        app.openDetail(parseInt(card.dataset.id, 10));
      });
    });

    root.querySelectorAll('.column-body').forEach(function (body) {
      body.addEventListener('dragover', function (e) {
        e.preventDefault();
        e.dataTransfer.dropEffect = 'move';
        body.classList.add('drop-target');
      });
      body.addEventListener('dragleave', function () { body.classList.remove('drop-target'); });
      body.addEventListener('drop', function (e) {
        e.preventDefault();
        body.classList.remove('drop-target');
        var id = draggingId;
        if (id == null) id = parseInt(e.dataTransfer.getData('text/plain'), 10);
        if (isNaN(id)) return;
        var status = body.dataset.drop;
        moveCard(id, status);
      });
    });
  }

  // Optimistic: move the card immediately, reconcile on response, revert on
  // error (SPEC §7.4).
  function moveCard(id, status) {
    var card = root.querySelector('.card[data-id="' + id + '"]');
    var origin = card ? card.parentElement : null;
    if (card) {
      card.classList.add('optimistic');
      var target = root.querySelector('.column-body[data-drop="' + status + '"]');
      if (target) target.appendChild(card);
    }
    app.api('PATCH', '/api/tasks/' + id, { status: status })
      .then(function () { app.refreshNow(); })
      .catch(function (err) {
        app.toast(err.message, true);
        if (card && origin) origin.appendChild(card);
        if (card) card.classList.remove('optimistic');
      });
  }

  function destroy() { root = null; }

  // Guard against clobbering an in-flight drag: one boolean, no diffing layer
  // (SPEC §7.5).
  function onExternalChange() {
    if (dirty) { pendingRefresh = true; return; }
    refresh();
  }

  global.GraphdBoard = {
    mount: mount,
    setGraph: setGraph,
    refresh: refresh,
    destroy: destroy,
    onExternalChange: onExternalChange
  };
})(window);
