/* §11.6 — layout determinism.
 *
 * Given the fixture graph, running the dagre engine twice with the fixed
 * options must produce identical positions for every node. This is the test
 * that catches "the layout engine iterates a map somewhere".
 *
 * Run with:  node internal/webui/assets/layout_test.js
 * (also invoked from internal/webui/layout_test.go so `go test ./...` covers it)
 *
 * There is no npm and no node_modules here. The vendored UMD builds are loaded
 * by handing them a tiny CommonJS shim; nothing is installed.
 */
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

// This file lives in internal/webui/ next to the Go test that invokes it; the
// front-end assets it exercises live in ./assets/.
const HERE = __dirname;
const ASSETS = path.join(HERE, 'assets');
const VENDOR = path.join(ASSETS, 'vendor');

// A minimal CommonJS environment for the vendored UMD bundles. Each library is
// evaluated once and cached by the name it exports under.
function makeLoader() {
  const cache = {};

  function load(file) {
    if (cache[file]) return cache[file];
    const full = path.join(VENDOR, file);
    const src = fs.readFileSync(full, 'utf8');
    const module = { exports: {} };
    const sandbox = {
      module,
      exports: module.exports,
      require: function (name) { return load(requireName(name)); },
      window: {},
      self: {},
      document: undefined,
      console,
      setTimeout,
      clearTimeout
    };
    sandbox.global = sandbox;
    sandbox.globalThis = sandbox;
    vm.createContext(sandbox);
    vm.runInContext(src, sandbox, { filename: full });
    cache[file] = module.exports;
    return module.exports;
  }

  function requireName(name) {
    switch (name) {
      case 'dagre': return 'dagre.min.js';
      case 'cytoscape': return 'cytoscape.min.js';
      case 'lodash.memoize': return 'lodash.memoize.js';
      case 'lodash.throttle': return 'lodash.throttle.js';
      default: throw new Error('vendored module not available: ' + name);
    }
  }

  return load;
}

const load = makeLoader();

// cytoscape and dagre need to be globals before layout.js runs, because
// layout.js calls cytoscape() directly the way the browser does.
global.cytoscape = load('cytoscape.min.js');
global.dagre = load('dagre.min.js');
load('cytoscape-dagre.js'); // registers the layout, side-effect only

// layout.js is a plain browser script; evaluate it in this process's global
// scope so it attaches GraphdLayout to `global`.
(function () {
  const src = fs.readFileSync(path.join(ASSETS, 'layout.js'), 'utf8');
  vm.runInThisContext(src, { filename: path.join(ASSETS, 'layout.js') });
})();

const GraphdLayout = global.GraphdLayout;

// The fixture from SPEC §11.1: 20 tasks, 24 edges, ids 1..20.
const TASKS = [
  [1, 'Design schema', 'done', 3],
  [2, 'Write migrations', 'done', 3],
  [3, 'Token bucket middleware', 'todo', 1],
  [4, 'Redis counter store', 'todo', 2],
  [5, 'Config loader', 'todo', 1],
  [6, 'Unit tests for limiter', 'todo', 2],
  [7, 'Integration tests', 'todo', 2],
  [8, 'Metrics export', 'todo', 3],
  [9, 'Dashboard panel', 'todo', 4],
  [10, 'Rollout behind flag', 'todo', 2],
  [11, 'Docs', 'todo', 3],
  [12, 'Load test', 'todo', 3],
  [13, 'Per-tenant quotas', 'cancelled', 4],
  [14, 'Admin override API', 'todo', 2],
  [15, 'Alerting', 'todo', 3],
  [16, 'Review threat model', 'done', 2],
  [17, 'Rate limit headers', 'todo', 2],
  [18, 'Client SDK update', 'todo', 3],
  [19, 'Backfill tenant configs', 'todo', 3],
  [20, 'Deprecate old limiter', 'todo', 4]
];

const EDGES = [
  [1, 2], [1, 3], [2, 3], [16, 3],
  [3, 4], [3, 5], [3, 17], [3, 13],
  [4, 6], [5, 6], [4, 7], [6, 7],
  [4, 12], [6, 12], [6, 8], [8, 9],
  [8, 15], [7, 10], [5, 10], [7, 20],
  [10, 20], [17, 18], [16, 14], [5, 19]
];

function fixtureGraph() {
  return {
    tasks: TASKS.map(function (t) {
      return { id: t[0], key: 'FIX-' + t[0], label: t[1], status: t[2], priority: t[3] };
    }),
    edges: EDGES.map(function (e, i) {
      return { id: i + 1, blocker_id: e[0], blocked_id: e[1], label: '' };
    })
  };
}

function run(engineId, graph, opts) {
  const engine = GraphdLayout.get(engineId);
  return engine.layout(graph, opts);
}

function serialize(map) {
  const ids = Array.from(map.keys()).sort(function (a, b) { return a - b; });
  return JSON.stringify(ids.map(function (id) {
    const p = map.get(id);
    return [id, p.x, p.y];
  }));
}

let failures = 0;

function check(name, cond, detail) {
  if (cond) {
    console.log('  ok   ' + name);
  } else {
    failures++;
    console.log('  FAIL ' + name + (detail ? ' — ' + detail : ''));
  }
}

console.log('layout determinism (§11.6)');

// ---- dagre twice must be identical ----
const a = serialize(run('dagre', fixtureGraph()));
const b = serialize(run('dagre', fixtureGraph()));
check('dagre is deterministic across two runs', a === b, a === b ? '' : 'run1 != run2');

// ---- and across a shuffled input order (this is the real map-iteration trap) ----
const shuffled = fixtureGraph();
shuffled.tasks.reverse();
shuffled.edges.reverse();
const c = serialize(run('dagre', shuffled));
check('dagre ignores input order', a === c, a === c ? '' : 'sorted != shuffled');

// ---- grid engine too ----
const g1 = serialize(run('grid', fixtureGraph()));
const g2 = serialize(run('grid', fixtureGraph()));
check('grid is deterministic across two runs', g1 === g2);

// ---- every node got a position ----
const dagre = run('dagre', fixtureGraph());
check('dagre places all 20 nodes', dagre.size === 20, 'placed ' + dagre.size);
const grid = run('grid', fixtureGraph());
check('grid places all 20 nodes', grid.size === 20, 'placed ' + grid.size);

// ---- no NaN coordinates ----
let nan = 0;
dagre.forEach(function (p) { if (!isFinite(p.x) || !isFinite(p.y)) nan++; });
check('dagre produced no NaN positions', nan === 0, nan + ' bad nodes');

// ---- dagre rankDir LR: a blocker sits left of what it blocks ----
// Task 3 blocks 4, 5, 17; with LR the blocker must have a smaller x.
const x3 = dagre.get(3).x;
const dependents = [4, 5, 17].filter(function (id) { return dagre.get(id).x > x3; });
check('rankDir LR puts blockers left of dependents', dependents.length === 3,
  dependents.length + '/3 dependents are to the right of FIX-3');

// ---- orientation: rankDir is an opts override, not a new default ----
// The default must stay LR (horizontal) — a bare layout(graph) call is the spec
// behaviour, and every existing caller relies on it.
const def = serialize(run('dagre', fixtureGraph()));
const explicitLR = serialize(run('dagre', fixtureGraph(), { rankDir: 'LR' }));
check('rankDir LR matches the default layout', def === explicitLR,
  def === explicitLR ? '' : 'default != explicit LR');

// TB (vertical) is deterministic too, and it is a *different* layout.
const tb1 = serialize(run('dagre', fixtureGraph(), { rankDir: 'TB' }));
const tb2 = serialize(run('dagre', fixtureGraph(), { rankDir: 'TB' }));
check('dagre TB is deterministic across two runs', tb1 === tb2);
check('TB differs from LR', tb1 !== def, 'TB == LR');

// TB stacks blockers above their dependents: same edge, y instead of x.
const tb = run('dagre', fixtureGraph(), { rankDir: 'TB' });
const tbDependents = [4, 5, 17].filter(function (id) { return tb.get(id).y > tb.get(3).y; });
check('rankDir TB puts blockers above dependents', tbDependents.length === 3,
  tbDependents.length + '/3 dependents are below FIX-3');

// An unknown rankDir falls back to the default rather than producing garbage.
const bogus = serialize(run('dagre', fixtureGraph(), { rankDir: 'NOPE' }));
check('unknown rankDir falls back to LR', bogus === def, 'bogus != default');

// grid honours orientation as well (same nodes, transposed axes).
const gridH = run('grid', fixtureGraph(), { rankDir: 'LR' });
const gridV = run('grid', fixtureGraph(), { rankDir: 'TB' });
check('grid places all 20 nodes horizontally', gridH.size === 20);
check('grid places all 20 nodes vertically', gridV.size === 20);
check('grid LR advances layers along x', gridH.get(3).x < gridH.get(4).x);
check('grid TB advances layers along y', gridV.get(3).y < gridV.get(4).y);

// ---- disconnected graphs do not crash either engine ----
const lonely = { tasks: [{ id: 1, key: 'X-1', label: 'lonely', status: 'todo' }], edges: [] };
check('single node graph lays out', run('dagre', lonely).size === 1);
check('empty graph lays out', run('dagre', { tasks: [], edges: [] }).size === 0);

if (failures > 0) {
  console.log('\n' + failures + ' check(s) failed');
  process.exit(1);
}
console.log('all layout checks passed');
