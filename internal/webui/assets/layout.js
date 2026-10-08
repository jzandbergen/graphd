/* graphd — LayoutEngine registry (SPEC §7.3).
 *
 * The server never computes layout. Each engine takes the graph and returns a
 * Map<nodeId, {x,y}>:
 *
 *   LayoutEngine = { id, layout(graph, opts) -> Map<nodeId, {x,y}> }
 *
 * Determinism is required: nodes are sorted by id ascending before being handed
 * to the engine, so the same graph + same engine + same options produces
 * byte-identical positions. There is an acceptance test for this (§11.6).
 *
 * Adding an engine later means adding one object here and one vendored file;
 * this interface must not change.
 *
 * IMPLEMENTATION NOTE (also in the README): the dagre engine calls dagre's
 * graphlib directly rather than going through cytoscape-dagre. cytoscape-dagre
 * asks cytoscape for each node's rendered dimensions, and headless cytoscape
 * reports 0x0 for nodes that were never drawn — which silently degenerates the
 * layout to a single point. Since our nodes have a *fixed* size by spec
 * (180x60, §7.2) the dimensions are known without asking the renderer at all,
 * so we hand them to dagre explicitly. Same algorithm, same fixed options, same
 * LR rank direction; the difference is that it is deterministic and testable
 * without a DOM. cytoscape-dagre is still vendored and loaded (it is in the
 * spec's table, and it registers the `name: 'dagre'` layout for anyone who
 * wants the cytoscape-integrated path).
 */
(function (global) {
  'use strict';

  var NODE_W = 180;   // fixed node size — makes layout deterministic and
  var NODE_H = 60;    // removes the measure-text-then-layout chicken-and-egg

  // Fixed dagre options. These are part of the spec, not a preference.
  // rankDir LR because work flows left to right: blockers left, dependents right.
  var DAGRE_OPTS = {
    rankDir: 'LR',
    nodeSep: 40,
    rankSep: 80,
    edgeSep: 10,
    ranker: 'network-simplex'
  };

  function sortedNodes(graph) {
    return graph.tasks.slice().sort(function (a, b) { return a.id - b.id; });
  }

  // ---- dagre (default): layered / Sugiyama ----

  var dagreEngine = {
    id: 'dagre',
    layout: function (graph, opts) {
      opts = opts || {};
      var nodes = sortedNodes(graph);
      var positions = new Map();
      if (nodes.length === 0) return positions;

      var dagre = global.dagre;
      if (!dagre || !dagre.graphlib) {
        // Without dagre, fall back to the grid engine rather than producing
        // nothing. (dagre is vendored, so this is a guard, not a path.)
        return gridEngine.layout(graph, opts);
      }

      var o = Object.assign({}, DAGRE_OPTS, opts.dagre || {});
      var g = new dagre.graphlib.Graph({ multigraph: true, compound: false });
      g.setGraph({
        rankdir: o.rankDir,
        nodesep: o.nodeSep,
        ranksep: o.rankSep,
        edgesep: o.edgeSep,
        ranker: o.ranker,
        marginx: 0,
        marginy: 0
      });
      g.setDefaultEdgeLabel(function () { return {}; });
      g.setDefaultNodeLabel(function () { return {}; });

      // Fixed dimensions, inserted in sorted order.
      nodes.forEach(function (t) {
        g.setNode('n' + t.id, { width: NODE_W, height: NODE_H });
      });
      graph.edges.forEach(function (e, i) {
        // dagre needs both endpoints present; a dangling edge would throw.
        if (!g.hasNode('n' + e.blocker_id) || !g.hasNode('n' + e.blocked_id)) return;
        g.setEdge('n' + e.blocker_id, 'n' + e.blocked_id, {}, 'e' + (e.id != null ? e.id : i));
      });

      dagre.layout(g);

      // dagre reports node centres. Read them back in sorted id order so the
      // map itself is deterministic too.
      nodes.forEach(function (t) {
        var n = g.node('n' + t.id);
        positions.set(t.id, {
          x: Math.round(n.x - NODE_W / 2),
          y: Math.round(n.y - NODE_H / 2)
        });
      });
      return positions;
    }
  };

  // ---- grid: topological columns, used for disconnected graphs and fallback ----
  //
  // Longest-path layering: a node sits one column to the right of its deepest
  // blocker. Disconnected nodes all land in column 0. Deterministic by
  // construction (sorted ids, stable column assignment).

  var gridEngine = {
    id: 'grid',
    layout: function (graph) {
      var nodes = sortedNodes(graph);
      var positions = new Map();
      if (nodes.length === 0) return positions;

      var indeg = new Map();
      var out = new Map();
      nodes.forEach(function (t) { indeg.set(t.id, 0); out.set(t.id, []); });
      graph.edges.forEach(function (e) {
        if (!out.has(e.blocker_id) || !indeg.has(e.blocked_id)) return;
        out.get(e.blocker_id).push(e.blocked_id);
        indeg.set(e.blocked_id, indeg.get(e.blocked_id) + 1);
      });

      // Kahn's algorithm, always taking the smallest id available so the
      // column assignment cannot depend on iteration order.
      var layer = new Map();
      nodes.forEach(function (t) { layer.set(t.id, 0); });
      var queue = nodes.filter(function (t) { return indeg.get(t.id) === 0; })
                       .map(function (t) { return t.id; });
      var remaining = new Map(indeg);
      var processed = 0;
      while (queue.length) {
        queue.sort(function (a, b) { return a - b; });
        var id = queue.shift();
        processed++;
        out.get(id).forEach(function (next) {
          if (layer.get(next) < layer.get(id) + 1) layer.set(next, layer.get(id) + 1);
          remaining.set(next, remaining.get(next) - 1);
          if (remaining.get(next) === 0) queue.push(next);
        });
      }
      if (processed !== nodes.length) {
        // A cycle slipped through (should be impossible; writes are rejected).
        // Fall back to index-based columns rather than hanging.
        nodes.forEach(function (t, i) { layer.set(t.id, i % 6); });
      }

      var byLayer = new Map();
      nodes.forEach(function (t) {
        var l = layer.get(t.id);
        if (!byLayer.has(l)) byLayer.set(l, []);
        byLayer.get(l).push(t.id);
      });
      var gapX = NODE_W + 80;
      var gapY = NODE_H + 40;
      byLayer.forEach(function (ids, l) {
        ids.sort(function (a, b) { return a - b; });
        ids.forEach(function (id, row) {
          positions.set(id, { x: l * gapX, y: row * gapY });
        });
      });
      return positions;
    }
  };

  var ENGINES = [dagreEngine, gridEngine];

  global.GraphdLayout = {
    NODE_W: NODE_W,
    NODE_H: NODE_H,
    DAGRE_OPTS: DAGRE_OPTS,
    engines: ENGINES,
    get: function (id) {
      for (var i = 0; i < ENGINES.length; i++) if (ENGINES[i].id === id) return ENGINES[i];
      return dagreEngine;
    }
  };

  // Expose for the headless determinism check (§11.6) under node.
  if (typeof module !== 'undefined' && module.exports) module.exports = global.GraphdLayout;
})(typeof window !== 'undefined' ? window : this);
