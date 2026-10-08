# graphd — Build Specification

A local-only work graph: tasks as nodes, blockers as labeled directed edges, auto-layout
on demand, and a computed ready-frontier. Plus a kanban board over the same state.
Single Go binary, embedded web UI, MCP server for agents.

**Status:** ready to build. Every ambiguous decision is resolved below. Where this document
is silent, make the boring choice and note it in the README.

---

## 0. READ THIS FIRST — hard constraints

These are not preferences. Violating any of them fails the build.

```
NO npm.  NO node_modules.  NO bundler.  NO transpiler.
NO React.  NO Vue.  NO Svelte.  NO Vite.  NO TypeScript.  NO JSX.
NO frontend build step of any kind.

Frontend = html/template + vanilla ES2020 JavaScript + vendored UMD libraries.
Assets embedded with //go:embed.
The entire build is:  go build ./...
```

The UI is served by the Go binary. There is no `package.json` in this repo. If you find
yourself reaching for one, you have made a wrong turn.

**Backend dependencies: exactly one.** `modernc.org/sqlite` (pure Go, no cgo). Everything
else is stdlib. Do not add chi/gorilla/mux — Go 1.22+ `http.ServeMux` supports
method+path patterns. Do not add a TOML/YAML parser. Do not add an MCP SDK; hand-roll it
(§8).

---

## 1. Purpose and scope

### 1.1 The problem

A dependency graph of work is the only view that answers "what can I start right now, and
what does starting it unblock." Ticket lists cannot. Kanban alone cannot. This tool is
that graph, locally, with no account, no server, no sync, no auth.

### 1.2 In scope

- Projects → tasks → blocking edges. Global state, not tied to one repo.
- Ready-frontier computation, ranked by leverage.
- Canvas: auto-layout on demand, manual drag, drag-to-create-edges.
- Kanban board over the same state.
- MCP server (stdio) so agents can read and write the graph.
- Live refresh: agent writes via MCP, UI updates without a reload.
- JSON export/import.

### 1.3 NON-GOALS — do not build these

| Not building | Why |
|---|---|
| Auth, users, permissions, RBAC | localhost, single user |
| Multi-user sync, websockets-to-server, cloud | local only |
| Postgres or any server DB | SQLite is correct here |
| Docs-on-nodes, rich text, attachments | out of scope; `notes` is a plain textarea |
| Prototypes, comments, review threads | out of scope |
| Time tracking, estimates, velocity, burndown | out of scope |
| Cross-project graph | one project visible at a time |
| Subtasks, epics, parent/child | flat. use tags if you must |
| Gantt, calendar, timeline views | out of scope |
| Cycle *display* in the UI | cycles are impossible; rejected at write time |
| Server-side graph layout | layout runs in the browser |
| Undo for server mutations | see §9.4 — undo is client-side and canvas-scoped |
| Notifications, email, webhooks | out of scope |

---

## 2. Semantics — the load-bearing rules

Get these wrong and the tool lies to you. They are not negotiable.

### 2.1 Status

```
todo | doing | done | cancelled
```

Nothing else. No `blocked` status. No `in_review`. No custom statuses.

- **Terminal statuses:** `done`, `cancelled`. Both unblock downstream tasks.
- **`blocked` is DERIVED, NEVER STORED.** A task is blocked because its blockers are
  open, not because someone set a field. Storing it guarantees drift: close a blocker and
  the dependent stays "blocked" forever. This is the single most important rule in this
  document.

### 2.2 Edges

Storage direction is **the direction work flows**, which is also the direction the arrow
is drawn. There is no inversion anywhere in the system.

```
edges(blocker_id, blocked_id, label)

  blocker_id  →  blocked_id      "blocker_id blocks blocked_id"

  Reading:  blocked_id cannot start until blocker_id reaches a terminal status.
```

Column names are `blocker_id` / `blocked_id` precisely so that no reader — human or model
— has to remember a convention. Do not rename them to `from`/`to`.

- **The label is decoration only.** Direction carries all the meaning. Do not branch logic
  on label contents. Two edges between the same pair in opposite directions are legal and
  meaningful.
- **No duplicate edges:** unique on `(blocker_id, blocked_id)`.
- **No self-edges:** rejected.
- **No cycles, ever.** Rejected at write time with the offending path in the error. A
  broken graph must never be persisted.

### 2.3 The ready-frontier

```
ready(t)  ⟺  t.status = 'todo'  AND  every blocker of t is in a terminal status

             Note: `doing` is NOT ready. Ready means "startable", not "in flight".
```

Equivalently, as SQL:

```sql
SELECT t.* FROM tasks t
WHERE t.project_id = ?1
  AND t.archived = 0
  AND t.status = 'todo'
  AND NOT EXISTS (
    SELECT 1 FROM edges e
    JOIN tasks b ON b.id = e.blocker_id
    WHERE e.blocked_id = t.id
      AND b.status NOT IN ('done','cancelled')
  );
```

A task with zero blockers is ready. This is the base case and it must work.

### 2.4 Leverage — ranking the frontier

Two derived integers per task. Both computed server-side; the client does no graph math.

**`unblocks(t)`** — the honest metric. The number of tasks that would become ready *right
now* if `t` were closed, holding everything else fixed.

```
unblocks(t):
  if t.status is terminal: return 0
  before = set of ready task ids
  after  = set of ready task ids computed with t.status forced to 'done'
  return |after \ before|
```

`t` itself is never counted: forcing it to `done` removes it from `after`.

Note this is **not** transitive. A task two hops downstream stays blocked, because its
intermediate blocker is still open. That is correct and intentional — it is the number
you can act on.

**`blast_radius(t)`** — the count of all transitive dependents of `t`, regardless of
current readiness. Display only, never used for ranking.

**Frontier ordering** (`get_ready`, and within kanban columns):

```
ORDER BY unblocks DESC, priority ASC, id ASC
```

`priority` is `INTEGER 1..5`, **1 = highest**, default `3`. The `id ASC` tiebreak makes
ordering total and deterministic. Do not omit it.

### 2.5 Archival

`delete_task` is a **soft delete**, and it is defined in terms of what already exists:

```
archive(t)  ⟹  t.archived = 1  AND  t.status = 'cancelled'
```

This is deliberate. Because `cancelled` is already terminal (§2.1), the frontier
predicate needs **no special case for archived tasks** — an archived blocker stops
blocking automatically, by the rule that already exists. Archived tasks are hidden from
the canvas and board by default (toggle to reveal). `restore_task` sets
`archived = 0, status = 'todo'`.

No row is ever hard-deleted. Edges survive archival, so history survives.

### 2.6 Tags

`tags` is a comma-separated `TEXT` column, not a join table. Used only for filtering and
canvas lenses. Not a hierarchy. Do not build a tag management UI.

---

## 3. Architecture

```
                    ┌──────────────────────────────────┐
                    │  graphd.db  (SQLite, WAL mode)   │
                    │  one file, the single source of  │
                    │  truth. both processes open it   │
                    │  directly. no daemon required.   │
                    └───────┬──────────────────┬───────┘
                            │                  │
              ┌─────────────┘                  └────────────┐
              │                                             │
    ┌─────────▼──────────┐                       ┌──────────▼─────────┐
    │  graphd serve      │                       │  graphd mcp        │
    │  HTTP + embedded   │                       │  stdio JSON-RPC    │
    │  web UI, SSE       │                       │  spawned by the    │
    │  127.0.0.1:7331    │                       │  agent client      │
    └─────────┬──────────┘                       └────────────────────┘
              │
    ┌─────────▼──────────┐
    │  browser           │
    │  Cytoscape canvas  │
    │  + kanban board    │
    │  layout client-side│
    └────────────────────┘
```

### 3.1 Two subcommands, one binary

```
graphd serve [--db PATH] [--listen ADDR] [--open]
graphd mcp   [--db PATH]
```

- `serve` — HTTP API, embedded UI, SSE. Long-running.
- `mcp` — stdio JSON-RPC. Spawned per-session by the agent client.

**The MCP process must not require the server to be running.** It opens the SQLite file
directly. Agents can file work while the UI is closed. This is a hard requirement.

### 3.2 Concurrency

Both processes write to the same file. Required pragmas on every connection:

```sql
PRAGMA journal_mode = WAL;
PRAGMA busy_timeout = 5000;
PRAGMA foreign_keys = ON;
PRAGMA synchronous = NORMAL;
```

Set `db.SetMaxOpenConns(1)` for writers. SQLite serializes writes anyway; a single
connection removes `SQLITE_BUSY` retry logic entirely. Reads may use a second connection.

### 3.3 Change propagation

A `meta` table holds a monotonic integer under key `revision`. **Every mutation bumps it
in the same transaction as the mutation itself.** Both processes do this.

`graphd serve` polls `revision` once per second (a single indexed row read — cheap). When
it changes, it pushes to all connected SSE clients. This is how an MCP write becomes a
live UI update without the two processes knowing about each other.

Do not use filesystem watching. Do not use a socket. The revision counter is enough.

### 3.4 The frontend build step does not exist

`internal/webui/assets/` is plain files, committed to the repo. Third-party libraries are
**vendored as UMD single-file builds and committed**, downloaded once by hand:

| File | Source | Purpose |
|---|---|---|
| `cytoscape.min.js` | cytoscape | graph rendering, pan/zoom/select |
| `dagre.min.js` | dagre | layered layout algorithm |
| `cytoscape-dagre.js` | cytoscape-dagre | binds dagre into cytoscape |
| `cytoscape-edgehandles.js` | cytoscape-edgehandles | drag-to-create-edge |

Four files. Nothing else. **No htmx** — the kanban needs real drag-and-drop JS
regardless, so htmx would be dead weight.

---

## 4. Data model

### 4.1 Schema — `internal/store/schema.sql`

```sql
CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
-- seed: INSERT INTO meta(key,value) VALUES ('revision','0')
--       INSERT INTO meta(key,value) VALUES ('schema_version','1')

CREATE TABLE IF NOT EXISTS projects (
  id               INTEGER PRIMARY KEY,
  name             TEXT    NOT NULL UNIQUE,
  key_prefix       TEXT    NOT NULL UNIQUE,   -- e.g. "RATE", uppercase, [A-Z][A-Z0-9]*
  next_task_number INTEGER NOT NULL DEFAULT 1,
  created_at       TEXT    NOT NULL           -- RFC3339 UTC
);

CREATE TABLE IF NOT EXISTS tasks (
  id         INTEGER PRIMARY KEY,
  project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  key        TEXT    NOT NULL,                -- "RATE-7", unique per project
  label      TEXT    NOT NULL,
  notes      TEXT    NOT NULL DEFAULT '',
  status     TEXT    NOT NULL CHECK (status IN ('todo','doing','done','cancelled')),
  priority   INTEGER NOT NULL DEFAULT 3 CHECK (priority BETWEEN 1 AND 5),
  tags       TEXT    NOT NULL DEFAULT '',     -- comma-separated
  x          REAL,                            -- NULL until placed
  y          REAL,
  archived   INTEGER NOT NULL DEFAULT 0,
  created_at TEXT    NOT NULL,
  updated_at TEXT    NOT NULL,
  UNIQUE (project_id, key)
);

CREATE INDEX IF NOT EXISTS idx_tasks_project ON tasks(project_id, archived);
CREATE INDEX IF NOT EXISTS idx_tasks_status  ON tasks(project_id, status);

CREATE TABLE IF NOT EXISTS edges (
  id          INTEGER PRIMARY KEY,
  project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  blocker_id  INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  blocked_id  INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  label       TEXT    NOT NULL DEFAULT '',
  created_at  TEXT    NOT NULL,
  UNIQUE (project_id, blocker_id, blocked_id),
  CHECK (blocker_id <> blocked_id)
);

CREATE INDEX IF NOT EXISTS idx_edges_blocker ON edges(blocker_id);
CREATE INDEX IF NOT EXISTS idx_edges_blocked ON edges(blocked_id);
```

`edges.project_id` is denormalized for query convenience. Validate on insert that both
tasks belong to the same project, and reject otherwise with `cross_project_edge`.

### 4.2 Key generation

On task create, inside the same transaction: read `next_task_number`, assign
`key = key_prefix || '-' || n`, increment the counter. Explicit `key` may be supplied;
if so, validate uniqueness and **do not** advance the counter past it — collisions are
the caller's problem.

Keys are monotonic and never reused. `next_task_number` only ever increases.

### 4.3 IDs vs keys

Both are valid identifiers in the API and MCP surface.

- **Integer `id`** — stable, used internally and in all edge references.
- **`key`** — human-facing, what you type and what agents echo back.

Resolution rule for any `task` parameter: if the string is all digits, try integer `id`
first, then fall back to `key`. Otherwise match `key` (case-insensitive). Document this;
it is a common source of confusion.

---

## 5. Graph algorithms

All in `internal/store/graph.go`. Pure functions over an in-memory graph struct. Load the
whole project graph once per request — a project is hundreds of nodes, not millions. Do
not write recursive CTEs.

```go
type Graph struct {
    Tasks map[int64]*Task
    // blocker -> blocked
    Out   map[int64][]int64
    // blocked -> blocker
    In    map[int64][]int64
    Edges []*Edge
}
```

### 5.1 `LoadGraph(projectID)`

One query for tasks, one for edges. Build adjacency maps. O(V+E).

### 5.2 `ReadySet() map[int64]bool`

```
terminal := {done, cancelled}
ready := {}
for each t in Tasks:
    if t.Archived or t.Status != "todo": continue
    ok := true
    for each b in In[t.id]:
        if Tasks[b].Status not in terminal: ok = false; break
    if ok: ready.add(t.id)
```

### 5.3 `Unblocks(t) int`

```
if t.Status in terminal: return 0
before := ReadySet()
saved := t.Status
t.Status = "done"          // mutate a copy, never the DB
after := ReadySet()
t.Status = saved
return len(after) - len(before ∩ after)
```

Equivalently `|after \ before|`. Implement it as a set difference; do not hand-optimize it
into something clever until a profile says you must.

### 5.4 `BlastRadius(t) int`

BFS over `Out` from `t`, counting distinct reachable tasks. Do not count `t`.

### 5.5 `WouldCycle(blocker, blocked) ([]int64, bool)`

Adding edge `blocker → blocked` closes a cycle **iff `blocked` can already reach
`blocker`** by following `Out` edges.

```
if blocker == blocked: return [blocker, blocker], true
path := BFS(Out, from=blocked, to=blocker)
if path exists:
    return append(path, blocker), true     // e.g. [3,4,7,3]
return nil, false
```

Return the full cycle path, closed (first element repeated at the end). It goes into the
API error body and into the MCP error text. A model that gets this error should be able
to fix its own plan without asking.

### 5.6 Determinism

`ReadySet` iterates `Tasks`, which is a Go map. **Sort by id before returning** anywhere
order is observable. Every list in every response is sorted by an explicit, total
ordering. Nothing may depend on Go's map iteration order.

---

## 6. HTTP API

Base `http://127.0.0.1:7331`. All responses JSON unless noted. All mutations bump
`revision` and emit SSE.

```
GET    /                              → 302 /canvas
GET    /canvas                        → HTML shell
GET    /board                         → HTML shell
GET    /api/projects                  → {projects:[Project]}
POST   /api/projects                  → Project
GET    /api/projects/{pid}            → Project
DELETE /api/projects/{pid}            → 204   (requires ?confirm=<name>)

GET    /api/projects/{pid}/graph      ?include_archived=0|1
GET    /api/projects/{pid}/ready      ?limit=N
GET    /api/projects/{pid}/events     → text/event-stream
GET    /api/projects/{pid}/export     → full JSON, Content-Disposition attachment
POST   /api/projects/{pid}/import     ← full JSON, transactional, replaces project contents

POST   /api/projects/{pid}/tasks      → Task
GET    /api/tasks/{tid}               → Task
PATCH  /api/tasks/{tid}               → Task      {label?,notes?,status?,priority?,tags?,x?,y?}
POST   /api/tasks/{tid}/archive       → Task
POST   /api/tasks/{tid}/restore       → Task

POST   /api/projects/{pid}/edges      → Edge      {blocker_id, blocked_id, label?}
DELETE /api/edges/{eid}               → 204
PATCH  /api/edges/{eid}               → Edge      {label}

POST   /api/projects/{pid}/positions  ← [{id,x,y},...]   bulk, one transaction
POST   /api/projects/{pid}/relayout   → {positions:[...]} server-side copy of last saved
```

`PATCH /api/tasks/{tid}` with `x`/`y` is the drag-persist path. `positions` is the
bulk path used after auto-layout. Both are just writes; the server does not compute
layout.

### 6.1 Response shapes

```jsonc
// Project
{"id":1,"name":"rate-limiting","key_prefix":"RATE","created_at":"2026-01-01T00:00:00Z"}

// Task — as returned inside graph
{
  "id": 3,
  "key": "RATE-3",
  "label": "Token bucket middleware",
  "notes": "",
  "status": "todo",
  "priority": 1,
  "tags": "",
  "x": 120.0, "y": 80.0,          // null when unplaced
  "archived": false,
  "created_at": "...", "updated_at": "...",

  // ---- all server-derived, client never computes these ----
  "ready": true,
  "blocked_by": [1, 2, 16],       // all blockers, any status
  "blocked_by_open": [],          // blockers that are not terminal
  "unblocks": 3,
  "blast_radius": 14
}

// Edge
{"id":7,"blocker_id":3,"blocked_id":4,"label":"","satisfied":false}
//   satisfied = blocker is in a terminal status

// Graph
{"project":{...},"revision":42,"tasks":[...],"edges":[...]}

// Ready
{"project_id":1,"revision":42,
 "ready":[{"id":3,"key":"RATE-3","label":"...","priority":1,
           "unblocks":3,"blast_radius":14,"blocked_by_open":[]}]}
```

The client is dumb on purpose. It renders `ready`, `unblocks`, `satisfied` as given. No
graph traversal in JavaScript. This keeps the interesting logic in one tested place.

### 6.2 Errors

```jsonc
{"error": {"code":"cycle_detected",
           "message":"adding RATE-7 -> RATE-3 creates a cycle: RATE-3 -> RATE-4 -> RATE-7 -> RATE-3",
           "cycle": [3,4,7,3]}}
```

| code | HTTP | when |
|---|---|---|
| `not_found` | 404 | unknown project/task/edge |
| `cycle_detected` | 409 | `WouldCycle` returned true |
| `self_edge` | 400 | `blocker_id == blocked_id` |
| `duplicate_edge` | 409 | unique violation on `(blocker, blocked)` |
| `cross_project_edge` | 400 | blocker and blocked in different projects |
| `duplicate_key` | 409 | task key already exists in project |
| `invalid_status` | 400 | status not in enum |
| `invalid_priority` | 400 | priority outside 1..5 |
| `invalid_prefix` | 400 | prefix fails `^[A-Z][A-Z0-9]{0,7}$` |
| `name_required` | 400 | empty name |
| `confirm_required` | 400 | project delete without `?confirm=` |

### 6.3 SSE

```
GET /api/projects/{pid}/events

event: changed
data: {"revision":43}

```

Heartbeat comment line `: ping` every 20s to keep the connection alive. On client
reconnect, `EventSource` retries automatically — no `Last-Event-ID` handling needed, the
client just refetches current state on every event.

---

## 7. Web UI

Two views, one header, one source of truth. Routes `/canvas` and `/board`.

### 7.1 Header (both views)

```
[ graphd ]  [ project ▾ ]  [ + new project ]     [ Canvas | Board ]     [ search ]  [ ⟳ ]
```

Project switcher is a `<select>`. Switching navigates to `/canvas?p=<id>`. Project is
persisted in the URL query string and in `localStorage` as the default for next visit.

### 7.2 Canvas — `/canvas`

Cytoscape.js. Server sends graph JSON; client renders.

**Node rendering.** Fixed size **180×60**. This is not cosmetic — fixed node dimensions
make layout deterministic and eliminate the measure-text-then-layout chicken-and-egg.
Label wraps to 2 lines, then truncates with `…`. Full label and notes live in the detail
panel.

| state | style |
|---|---|
| `todo` | neutral fill, solid 1px border |
| `doing` | amber fill, 2px border |
| `done` | green tint, 60% opacity |
| `cancelled` | grey, dashed border, 40% opacity |
| **ready** | 3px accent ring, subtle outer glow |
| **blocked** | small lock glyph, bottom-left |
| `unblocks > 0` | badge in the corner with the number |

Ready ring and blocked glyph are computed from the server's `ready` field. Never
recomputed client-side.

**Edge rendering.** Arrow drawn `blocker → blocked`, label centered on the edge when
non-empty.

| state | style |
|---|---|
| open | solid grey arrow |
| satisfied (`satisfied:true`) | dashed, dimmed green |

**Interactions.**

| action | result |
|---|---|
| drag node | move; persist via `PATCH /api/tasks/{id}` debounced 300ms after drop |
| drag from node handle → node | create edge via edgehandles; label empty by default |
| click node | open detail panel |
| click edge | select; `Delete` or trash button removes it |
| `Delete` on selected node | confirm → archive |
| marquee drag on background | multi-select |
| scroll / pinch | zoom (built in) |
| `L` or **Layout** button | run layout engine, persist all positions |
| `F` or **Fit** button | zoom to fit |
| `Ctrl+Z` | undo (canvas ops only, §9.4) |

**Creating an edge must not open a modal.** Empty label, editable afterward in the detail
panel. A modal on every edge makes the tool tedious within an hour.

**Detail panel.** Right sidebar, ~340px, toggled. Contents: key, label (editable), notes
(textarea), status (radio group), priority (1–5 select), tags (text input), derived
readouts (`ready`, `unblocks`, `blast_radius`), and two edge lists — *blocked by* and
*blocks* — each row with a remove button and a picker to add. Save on blur/change;
no explicit save button.

### 7.3 Auto-layout

**Runs in the browser. The server never computes layout.**

```
LayoutEngine = { id: string, layout(graph, opts) -> Map<nodeId, {x,y}> }

engines:
  "dagre"  (default) — layered / Sugiyama, via cytoscape-dagre
  "grid"             — topological grid, used for disconnected graphs and as fallback
```

Default dagre options — fix these, they are part of the spec:

```js
{ name: 'dagre', rankDir: 'LR', nodeSep: 40, rankSep: 80, edgeSep: 10, ranker: 'network-simplex' }
```

`rankDir: 'LR'` because work flows left to right: blockers on the left, dependents on the
right. This matches the arrow direction and reads correctly.

**Determinism is required.** Sort nodes by `id` ascending before handing them to the
engine. Same graph + same engine + same options ⇒ byte-identical positions. There is an
acceptance test for this (§11.6).

**Layout clobbers; undo restores.** Do not attempt to pin nodes, preserve manual
positions, or run partial layout. There is no `pinned` column and there must not be one.
Layout is a deliberate button press; the user expects it to rearrange everything. The
undo stack (§9.4) is the escape hatch.

**Unplaced nodes** (`x`/`y` null) are arranged in a grid client-side at render time and
**not persisted** — until the user drags one or presses Layout. A freshly
agent-scaffolded graph therefore appears as a tidy grid, ready for one Layout press.

**Engine is configurable** via the header: a small dropdown beside the Layout button.
Selection persists in `localStorage`. Adding an engine later means adding one object to
the registry and one vendored file; the interface above must not change.

### 7.4 Board — `/board`

Four fixed columns: **Todo · Doing · Done · Cancelled**. Not configurable. No custom
columns, no swimlanes, no WIP limits.

- Card: key, label (2-line clamp), priority chip, `unblocks` badge when > 0, ready dot,
  and `blocked by N` when `blocked_by_open` is non-empty.
- Column header: name + count.
- **Sort within column:** ready first, then `unblocks DESC, priority ASC, id ASC` — the
  same ordering as the frontier. The top of the Todo column is the answer to "what do I
  do next."
- Drag card between columns → `PATCH` status. Optimistic: move the card immediately,
  reconcile on response, revert on error.
- Archived tasks hidden behind a header toggle.

Board is server-rendered HTML (a Go template fragment), re-fetched on SSE revision change
and swapped wholesale. Drag-and-drop is ~40 lines of HTML5 drag events. Do not build a
client-side rendering framework for this.

### 7.5 Live refresh

Both views hold an `EventSource` to `/api/projects/{pid}/events`. On `changed`, refetch
graph + ready and re-render.

**Guard against clobbering an in-flight interaction:** if a node drag or card drag is in
progress, set a `dirty` flag and defer the refetch until the interaction completes. One
boolean. Do not build a diffing layer.

Preserve viewport (pan/zoom) across refresh — capture `cy.pan()`/`cy.zoom()` before
re-render and restore after. Losing your viewport every time an agent writes a task is
the fastest way to make the tool unusable.

### 7.6 Visual language

Dark theme. No gradients, no drop shadows, no rounded-everything. Monospace for keys and
IDs, system sans for labels. Dense, not airy — this is a tool, not a landing page.
One accent color for "ready" and nothing else competing for attention.

---

## 8. MCP server

`graphd mcp` — stdio transport. Hand-rolled. No SDK dependency.

### 8.1 Protocol

**Newline-delimited JSON-RPC 2.0 over stdin/stdout.** One complete JSON object per line.
No `Content-Length` framing. Never write anything to stdout that is not a JSON-RPC
message — all logging goes to stderr.

Protocol version: `2024-11-05`.

| method | direction | response |
|---|---|---|
| `initialize` | → | `{protocolVersion, capabilities:{tools:{}}, serverInfo:{name:"graphd",version}}` |
| `notifications/initialized` | → | *(notification — no response)* |
| `tools/list` | → | `{tools:[{name,description,inputSchema}]}` |
| `tools/call` | → | `{content:[{type:"text",text:"..."}], isError:bool}` |
| `ping` | → | `{}` |

**Tool-level failures are not protocol errors.** A cycle rejection returns a normal
`tools/call` result with `isError:true` and the explanation in the text content. Reserve
JSON-RPC error objects for malformed requests and unknown methods.

Tool results: pretty-printed JSON in a single `text` content block.

### 8.2 Tool set — exactly these thirteen

Do not invent more. Do not split tools "for clarity". A small, sharp surface is the point.

| tool | arguments | returns |
|---|---|---|
| `list_projects` | — | `[{id,name,key_prefix,task_count}]` |
| `create_project` | `name`, `key_prefix` | `{id,name,key_prefix}` |
| `get_graph` | `project`, `include_archived?` | tasks + edges, **no x/y** |
| `get_ready` | `project`, `limit?` | ranked frontier |
| `get_next_task` | `project` | `{next, reason, in_progress}` |
| `create_task` | `project`, `label`, `notes?`, `status?`, `priority?`, `tags?` | task |
| `update_task` | `task`, `label?`, `notes?`, `status?`, `priority?`, `tags?` | task |
| `archive_task` | `task` | task |
| `restore_task` | `task` | task |
| `add_edge` | `project`, `blocker`, `blocked`, `label?` | edge |
| `remove_edge` | `project`, `blocker`, `blocked` | `{removed:true}` |
| `scaffold_plan` | `project`, `tasks[]`, `edges[]` | summary |
| `export_json` | `project` | full export |

**`scaffold_plan` is transactional** — all tasks and edges are created, or none are. This
is the tool that matters most for agents: a whole plan in one call instead of thirty
round-trips. Its `tasks[]` entries may carry a local `ref` string; `edges[]` reference
those refs (`{blocker:"design", blocked:"mw", label?}`), resolved to real ids during the
transaction. If any edge would create a cycle, the whole call rolls back and the error
names the refs involved.

**`get_graph` deliberately omits `x`/`y`.** Agents do not read or write positions (§9.2).
Omitting them also keeps payloads small.

**`get_next_task` response:**

```jsonc
{"next": {"id":3,"key":"RATE-3","label":"...","priority":1,"unblocks":3,
          "blocked_by_open":[]},
 "reason": "ok",
 "in_progress": [{"id":5,"key":"RATE-5","label":"...","status":"doing"}]}
```

`reason` is `"ok"` or `"no_ready_tasks"` (with `next: null`). `in_progress` is
informational — it lets a resuming agent see what is already claimed without a second
call.

### 8.3 Project resolution

Every tool that takes `project` resolves in this order:

1. Integer `id` if the value is all digits
2. `name` (exact, case-insensitive)
3. `key_prefix` (exact, case-insensitive)

If unresolved → `isError:true`, text listing available projects. **If `project` is
omitted**, fall back to the `GRAPHD_PROJECT` environment variable; if that is also unset,
error with the project list. Do **not** infer from the current working directory — this
tool is explicitly not repo-bound, and cwd magic would contradict that.

---

## 9. Behaviour details

### 9.1 Transactions

Every mutation is one transaction that (a) writes the change and (b) bumps `revision`.
Never bump outside the transaction — a reader could observe a revision that does not
reflect the data.

### 9.2 Positions

Only the browser writes `x`/`y`. The API accepts them on `PATCH /api/tasks/{id}` and on
`POST /positions`. MCP has no position-write path at all. An agent-scaffolded graph
arrives unplaced, renders as a grid, and waits for one Layout press.

### 9.3 Import/export

Export is lossless: projects, tasks, edges, all fields including positions and archived
flags, plus `schema_version` and `exported_at`. Import is transactional and **replaces**
the target project's contents (never merges). Import validates: every edge's endpoints
exist, no cycles, no duplicate keys. On any validation failure, roll back entirely and
report the first failure with its code.

### 9.4 Undo — client-side, canvas-scoped, session-only

An in-memory stack, capped at 50 entries, covering **canvas operations only**: node move,
layout, edge create, edge delete, task create from canvas. `Ctrl+Z` pops and applies.

Not covered: status changes, priority edits, text edits, archival. Every one of those is
trivially reversible by hand in the UI, so a server-side undo log would be cost without
benefit. Do not build one. Do not persist the undo stack.

### 9.5 Logging

`log/slog` to stderr. `serve` logs requests at `info` (method, path, status, duration),
errors at `error`. `mcp` logs only to stderr — **stdout is the protocol channel and any
stray write corrupts the session.** This is the most likely bug in the MCP layer; put a
comment on it in the code.

---

## 10. Configuration and operations

```
Flags (override env, override defaults):

  --db PATH        default: $XDG_DATA_HOME/graphd/graphd.db
                            (fallback ~/.local/share/graphd/graphd.db)
  --listen ADDR    default: 127.0.0.1:7331
  --open           open the browser on start (serve only)
  --log-level      default: info

Env:
  GRAPHD_DB, GRAPHD_LISTEN, GRAPHD_PROJECT, GRAPHD_LOG_LEVEL
```

No config file. Flags and env vars are sufficient for two subcommands, and a config file
would mean a TOML dependency for no real gain.

**`serve` MUST refuse to bind a non-loopback address** unless `--allow-remote` is passed.
There is no authentication of any kind; a remote bind is a real vulnerability. Check that
the resolved address is loopback and exit with a clear error if not. Test this.

Directories are created with `0755` if missing. The DB file is `0644`.

---

## 11. Acceptance tests

Every one of these must be a real test in the repo. They are the definition of done.

### 11.1 The fixture

`internal/store/fixture.go` seeds a deterministic 20-task / 24-edge project. It is used by
tests **and** exposed as `graphd serve --seed-fixture` so a human can eyeball the UI
against known-correct data on day one.

Project `fixture`, prefix `FIX`.

Tasks — `(id, label, status, priority)`:

```
 1  Design schema                 done      3
 2  Write migrations              done      3
 3  Token bucket middleware       todo      1
 4  Redis counter store           todo      2
 5  Config loader                 todo      1
 6  Unit tests for limiter        todo      2
 7  Integration tests             todo      2
 8  Metrics export                todo      3
 9  Dashboard panel               todo      4
10  Rollout behind flag           todo      2
11  Docs                          todo      3
12  Load test                     todo      3
13  Per-tenant quotas             cancelled 4
14  Admin override API            todo      2
15  Alerting                      todo      3
16  Review threat model           done      2
17  Rate limit headers            todo      2
18  Client SDK update             todo      3
19  Backfill tenant configs       todo      3
20  Deprecate old limiter         todo      4
```

Edges — `(blocker → blocked)`, 24 total:

```
 1→2   1→3   2→3   16→3   3→4   3→5   3→17  3→13
 4→6   5→6   4→7   6→7   4→12  6→12  6→8   8→9
 8→15  7→10  5→10  7→20  10→20 17→18 16→14 5→19
```

The graph is a DAG. Verify that in a test.

### 11.2 Frontier correctness — the crux

```
ReadySet() == {3, 11, 14}
```

Nothing else. `13` is `cancelled`, so it is not ready. Everything else has at least one
open blocker.

### 11.3 Leverage correctness

```
unblocks(3)  == 3      # closing 3 readies 4, 5, and 17
unblocks(11) == 0
unblocks(14) == 0
unblocks(1)  == 0      # already terminal
blast_radius(3) == 14
```

Then close task 3 (`status = done`) and re-assert:

```
ReadySet() == {4, 5, 11, 14, 17}
unblocks(5)  == 1      # closing 5 readies 19
unblocks(17) == 1      # closing 17 readies 18
unblocks(4)  == 0      # 6 still needs 5
```

That last assertion is the important one: it proves leverage is not naively transitive.

### 11.4 Frontier ordering

`get_ready` on the unmodified fixture returns exactly:

```
[ FIX-3  (unblocks 3, priority 1),
  FIX-14 (unblocks 0, priority 2),
  FIX-11 (unblocks 0, priority 3) ]
```

Ties on `unblocks` break by priority, not by id. Assert the full ordered slice.

### 11.5 Cycle rejection

```
add_edge(blocker=7, blocked=3)
  → error code "cycle_detected"
  → cycle path == [3, 4, 7, 3]
  → graph unchanged (edge count still 24, revision unchanged)
```

The path exists because `3→4` and `4→7`. Also assert:

```
add_edge(3, 3)   → "self_edge"
add_edge(3, 4)   → "duplicate_edge"      (already exists)
add_edge(1, 2)   → "duplicate_edge"
```

### 11.6 Layout determinism

Node in a JS test or a headless check: given the fixture graph, running the `dagre` engine
twice with the fixed options produces **identical** positions for every node. This is the
test that catches "the layout engine iterates a map somewhere."

### 11.7 Persistence

Set positions via `POST /positions`, stop the server, restart it, `GET /graph` → the
positions are unchanged. Drag-persist survives restart.

### 11.8 Live update across processes

1. Start `graphd serve`.
2. Open an SSE client on `/api/projects/1/events`.
3. Run `graphd mcp` in a separate process; `tools/call create_task`.
4. The SSE client receives a `changed` event with a bumped `revision` within 2s.

This is the integration that proves the revision-counter design works.

### 11.9 MCP protocol

- `initialize` returns protocol version `2024-11-05` and a `tools` capability.
- `tools/list` returns exactly 13 tools, each with a valid JSON Schema `inputSchema`.
- `tools/call get_next_task` on the fixture returns `FIX-3` with `unblocks: 3`.
- `tools/call add_edge` for a cycle returns `isError:true` and a message containing the
  cycle path — **not** a JSON-RPC error object.
- Stdout contains nothing but JSON-RPC lines across a full session.

### 11.10 `scaffold_plan` atomicity

Call `scaffold_plan` with 5 tasks and an edge set that contains a cycle. Assert: zero
tasks created, zero edges created, `revision` unchanged. Then call it with a valid set and
assert all 5 tasks and all edges exist with correct ref resolution.

### 11.11 Bind guard

`graphd serve --listen 0.0.0.0:7331` exits non-zero with a clear error and does not bind.
`--allow-remote` permits it.

### 11.12 Archival semantics

Archive task 4. Assert: task 4 disappears from the default graph, `archived = 1` and
`status = 'cancelled'` in the DB, and — because `cancelled` is terminal — any task blocked
*by* task 4 becomes ready. This test exists to prove the "no special case for archived"
design actually holds.

---

## 12. File layout

```
graphd/
├── go.mod                          module graphd, go 1.24
├── main.go                         subcommand dispatch, flag parsing
├── README.md
├── SPEC.md                         this file
│
├── internal/
│   ├── store/
│   │   ├── schema.sql              embedded via //go:embed
│   │   ├── store.go                Open, migrate, Tx helper, bumpRevision
│   │   ├── projects.go             CRUD, key counter
│   │   ├── tasks.go                CRUD, status/priority validation
│   │   ├── edges.go                add (with cycle check), remove, list
│   │   ├── graph.go                LoadGraph, ReadySet, Unblocks, BlastRadius,
│   │   │                           WouldCycle            ← the heart of it
│   │   ├── export.go               export/import, validation, rollback
│   │   ├── fixture.go              SeedFixture()         (test + --seed-fixture)
│   │   ├── graph_test.go           §11.2 §11.3 §11.4 §11.5
│   │   ├── export_test.go          §11.3 rollback, §11.10
│   │   └── fixture_test.go         fixture is a DAG
│   │
│   ├── api/
│   │   ├── server.go               ServeMux routes, middleware, loopback guard
│   │   ├── handlers.go             one handler per route
│   │   ├── views.go                html/template rendering for board fragments
│   │   ├── sse.go                  revision poller + broadcaster
│   │   ├── errors.go               error codes → HTTP + JSON
│   │   └── api_test.go             §11.7 §11.8 §11.11 §11.12
│   │
│   ├── mcp/
│   │   ├── server.go               stdio read loop, JSON-RPC envelope, dispatch
│   │   ├── tools.go                the 13 tool definitions + inputSchema
│   │   ├── scaffold.go             transactional scaffold_plan
│   │   └── mcp_test.go             §11.9 §11.10
│   │
│   └── webui/
│       ├── embed.go                //go:embed all:assets  → webui.FS
│       └── assets/
│           ├── index.html          both view shells
│           ├── app.js              bootstrap, project switcher, SSE, view routing
│           ├── canvas.js           cytoscape, detail panel, edgehandles, undo
│           ├── layout.js           LayoutEngine registry: dagre + grid
│           ├── board.js            column drag/drop, fragment swap
│           ├── style.css
│           └── vendor/
│               ├── cytoscape.min.js
│               ├── dagre.min.js
│               ├── cytoscape-dagre.js
│               └── cytoscape-edgehandles.js
```

Roughly 4,000–4,500 lines including tests. `graph.go` is ~250 lines and is where the
value lives; treat it as the crown jewel and test it first.

---

## 13. Milestones

Each milestone ends with something runnable. Do not proceed until the previous one's
acceptance tests pass.

### M0 — skeleton *(~150 LOC)*

`go.mod`, `main.go` with both subcommands, flag parsing, loopback guard.
`graphd serve` returns "hello" on `/`. `graphd mcp` responds correctly to `initialize`.

**Done when:** §11.11 passes.

### M1 — store and graph logic *(~900 LOC)*

Schema, migrations, `graph.go`, fixture, all CRUD. No HTTP, no UI. Add temporary CLI
subcommands so it is usable: `graphd task add`, `graphd ready`, `graphd edge add`,
`graphd seed-fixture`.

**Done when:** §11.2, §11.3, §11.4, §11.5, §11.12 pass. **This is the milestone that
matters.** If the frontier and leverage math is right here, everything downstream is
plumbing.

### M2 — HTTP API *(~600 LOC)*

All routes from §6, error codes, export/import, transactions + revision bumping.
No SSE yet.

**Done when:** §11.7 passes and every route has a handler test.

### M3 — canvas, read-only *(~700 LOC)*

`/canvas`, `go:embed`, vendored libs, graph fetch, cytoscape render, node/edge styling,
Layout button with dagre, Fit, SSE live refresh with viewport preservation.

**Done when:** §11.6 and §11.8 pass, and the fixture renders legibly with three nodes
ringed as ready.

### M4 — canvas editing + board *(~900 LOC)*

Drag-persist, edgehandles, detail panel, edge delete, archive, undo stack. `/board` with
four columns, drag-between-columns, server-rendered fragments, same SSE refresh.

**Done when:** you can build the fixture graph by hand from an empty project without
touching the API.

### M5 — MCP *(~450 LOC)*

stdio loop, JSON-RPC envelope, 13 tools, `scaffold_plan`, project resolution.

**Done when:** §11.9 and §11.10 pass, and an agent can `scaffold_plan` a project that then
appears in the open canvas within two seconds.

### M6 — polish *(~500 LOC)*

Search, tag filters, archived toggle, canvas lenses (ready-frontier only / a node's blast
radius / connected component of a selection), keyboard nav, empty states, README.

**Done when:** you stop opening plandesk.

---

## 14. Open questions — resolve during the build, note in README

These are deliberately left to the implementer. None block starting.

1. **Multiple projects on one canvas?** Currently no (§1.3). If you want it later, the
   clean way is a read-only overlay, not a merged graph.
2. **`graphd tui`?** A terminal frontier view (`graphd ready --watch`) would be ~80 lines
   and genuinely useful. Cheap. Consider it post-M6.
3. **Second layout engine (ELK)?** The registry in §7.3 supports it. `elkjs` is ~1.5MB
   vendored versus dagre's ~200KB, so only add it if dagre's output actually disappoints
   you on a real graph.
4. **Should `get_ready` include `doing` tasks?** Currently no. If agents keep asking
   "what am I working on", add a flag rather than changing the default.
5. **Revision history / audit log?** Not in scope. If wanted, append to a
   `task_events` table in the same transaction — the transaction boundaries already exist
   in the right places for it.

---

## 15. The one-paragraph summary, for the model reading this

Build a single Go binary called `graphd`. It has two subcommands: `serve` (HTTP + embedded
vanilla-JS web UI on localhost) and `mcp` (stdio JSON-RPC for agents). Both open the same
SQLite file directly; a `revision` counter in a `meta` table, bumped in every write
transaction, lets the server push SSE updates when the MCP process writes. Tasks have one
of four statuses and edges run `blocker_id → blocked_id`; `blocked` is always derived,
never stored; the ready-frontier is `status = 'todo'` AND all blockers terminal; and the
frontier is ranked by `unblocks` (how many tasks become ready if this one closes),
then priority, then id. Cycles are rejected at write time with the path. The canvas is
Cytoscape with dagre, layout runs in the browser and is deterministic, the Layout button
clobbers positions and undo restores them, and agents never touch positions. The board is
four fixed columns over the same state. No npm, no build step, one Go dependency, ~4,000
lines. The fixture graph in §11.1 with its asserted frontier `{3, 11, 14}` and
`unblocks(3) == 3` is the correctness anchor for the whole thing — build and test that
first.
