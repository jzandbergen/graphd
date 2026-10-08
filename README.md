# graphd

A local-only work graph: **tasks as nodes, blockers as labeled directed edges**, auto-layout
on demand, and a computed **ready-frontier**. Plus a kanban board over the same state.

Single Go binary, embedded web UI, MCP server for agents. No accounts, no server, no sync,
no auth.

---

## Build and run

```sh
go build ./...            # the entire build. no npm, no node_modules, no bundler.
go test ./...             # the acceptance tests
go vet ./...

# try it against known-correct data on day one
go run . serve --seed-fixture --open

# file work while the UI is closed
go run . mcp
```

```
graphd serve [--db PATH] [--listen ADDR] [--open] [--log-level LEVEL]
             [--allow-remote] [--seed-fixture]
graphd mcp   [--db PATH] [--log-level LEVEL]
```

| flag | default |
|---|---|
| `--db` | `$XDG_DATA_HOME/graphd/graphd.db`, else `~/.local/share/graphd/graphd.db` |
| `--listen` | `127.0.0.1:7331` |
| `--log-level` | `info` (`mcp` defaults to `warn`) |

Env: `GRAPHD_DB`, `GRAPHD_LISTEN`, `GRAPHD_PROJECT`, `GRAPHD_LOG_LEVEL`. Flags override env.
There is no config file.

**`serve` refuses to bind a non-loopback address** unless `--allow-remote` is passed. There
is no authentication of any kind, so a remote bind is a real vulnerability. See
`TestBindGuard`.

---

## The load-bearing semantics

Two rules carry the whole design. Everything else is plumbing.

### `blocked` is derived, never stored

There are exactly four statuses — `todo`, `doing`, `done`, `cancelled` — and no `blocked`
among them. A task is blocked because its blockers are open, not because someone set a
field. Storing it guarantees drift: close a blocker and the dependent stays "blocked"
forever.

### Archival needs no special case

`delete_task` is a soft delete defined in terms of what already exists:

```
archive(t)  ⟹  t.archived = 1  AND  t.status = 'cancelled'
```

Because `cancelled` is already terminal, the frontier predicate needs **no special case for
archived tasks** — an archived blocker stops blocking automatically, by the rule that
already exists. Edges survive archival, so history survives. `TestArchivalSemantics` exists
to prove this holds.

The ready-frontier is therefore:

```
ready(t)  ⟺  t.status = 'todo'  AND  every blocker of t is in a terminal status
```

A task with zero blockers is ready (the base case). `doing` is **not** ready: ready means
startable, not in flight.

Edges run **`blocker_id → blocked_id`** — the direction work flows, which is also the
direction the arrow is drawn. There is no inversion anywhere in the system. The label is
decoration only; no logic branches on it.

---

## Leverage

Two derived integers, both computed server-side; the client does no graph math.

- **`unblocks(t)`** — the number of tasks that would become ready *right now* if `t` were
  closed, holding everything else fixed. This is deliberately **not transitive**: a task two
  hops downstream stays blocked because its intermediate blocker is still open. That is the
  number you can act on. `TestLeverageFixture` asserts `unblocks(4) == 0` after closing 3,
  precisely to catch a naive transitive implementation.
- **`blast_radius(t)`** — all transitive dependents, regardless of readiness. Display only.

Frontier ordering, everywhere:

```
unblocks DESC, priority ASC, id ASC
```

`priority` is 1..5, **1 = highest**, default 3. The `id ASC` tiebreak makes the ordering
total and deterministic — do not omit it.

---

## Architecture

```
              graphd.db  (SQLite, WAL)
              one file, the single source of truth
                 │                    │
      graphd serve                 graphd mcp
      HTTP + embedded UI           stdio JSON-RPC
      127.0.0.1:7331               spawned per agent session
                 │
              browser
              Cytoscape canvas + kanban board
              layout runs client-side
```

Both processes open the same SQLite file **directly**. The MCP process does not require the
server to be running: agents can file work while the UI is closed.

**Change propagation.** A `meta` table holds a monotonic integer under key `revision`.
Every mutation bumps it in the same transaction as the mutation itself. `serve` polls that
one indexed row once per second and pushes to SSE clients when it changes. No filesystem
watching, no socket, no shared daemon — the revision counter is enough. `TestLiveUpdateAcrossProcesses`
proves an MCP write reaches an SSE client within 2s.

---

## Layout

Layout runs in the browser. The server never computes it. The registry is:

```js
LayoutEngine = { id, layout(graph, opts) -> Map<nodeId, {x,y}> }
```

Engines: **`dagre`** (default, layered/Sugiyama, `rankDir: 'LR'`) and **`grid`**
(topological columns; disconnected graphs and fallback). Fixed dagre options:
`nodeSep: 40, rankSep: 80, edgeSep: 10, ranker: 'network-simplex'`. `rankDir: 'LR'` because
work flows left to right: blockers on the left, dependents on the right.

**Orientation is an override, not a second engine.** The header toggle (and `O`) flips
`rankDir` between `LR` (horizontal) and `TB` (vertical) and re-runs the layout. It rides on
the `opts` argument that was already in the `LayoutEngine` signature, so the interface is
unchanged and the spec's `LR` is still the default for a bare `layout(graph)` call. The
choice persists in `localStorage` under `graphd.orientation`. `layout_test.js` asserts that
the default still equals explicit `LR`, that `TB` is deterministic, and that `TB` puts a
blocker above its dependents — the same check the spec makes for `LR` along x.

**Determinism is required** and tested (§11.6): nodes are sorted by `id` before being handed
to the engine, so the same graph + same engine + same options yields byte-identical
positions. Run it directly with `node internal/webui/assets/layout_test.js`.

**Layout clobbers; undo restores.** There is no `pinned` column and there must not be one.
The undo stack (canvas ops only, capped at 50, session-only) is the escape hatch.

**Unplaced nodes** (`x`/`y` null) are arranged in a grid client-side at render time and are
**not persisted** until the user drags one or presses Layout. An agent-scaffolded graph
therefore appears as a tidy grid, waiting for one Layout press. Agents never read or write
positions — `get_graph` omits `x`/`y` on purpose.

---

## MCP

`graphd mcp` — newline-delimited JSON-RPC 2.0 over stdin/stdout, hand-rolled, no SDK.
Protocol version `2024-11-05`. **Stdout is the protocol channel**: nothing but JSON-RPC
messages is ever written there; all logging goes to stderr.

Exactly thirteen tools: `list_projects`, `create_project`, `get_graph`, `get_ready`,
`get_next_task`, `create_task`, `update_task`, `archive_task`, `restore_task`, `add_edge`,
`remove_edge`, `scaffold_plan`, `export_json`.

Tool-level failures are **not** protocol errors: a cycle rejection comes back as a normal
`tools/call` result with `isError: true` and the offending path in the text, so a model can
fix its own plan without asking. JSON-RPC error objects are reserved for malformed requests
and unknown methods.

`scaffold_plan` is transactional — a whole plan in one call, all of it or none of it. Task
entries carry a local `ref`; edges reference those refs and are resolved inside the
transaction.

Every tool taking `project` resolves in order: integer id, then name (case-insensitive),
then `key_prefix` (case-insensitive). If `project` is omitted it falls back to
`GRAPHD_PROJECT`; if that is unset it errors with the project list. It never infers from the
working directory — this tool is explicitly not repo-bound.

---

## Notes are markdown

A task's `notes` is stored, transported and exported as **plain text** — the column stays
`TEXT`, the API and MCP keep returning a string, and export/import stays byte-lossless.
Markdown is applied as a *view* in the detail panel. Nothing here introduces a document
entity, a rich-text editor or an attachment, so this stays inside the SPEC's "notes is a
plain textarea" non-goal: markdown is the one formatting choice that *is* plain text.

The detail panel defaults to a rendered preview with an **edit / preview** toggle; the
editor is still a plain `<textarea>`.

**Sanitisation matters here**, because notes are typically written by a model and rendered
as HTML. `marked` passes raw HTML straight through and does not filter link schemes, so
rendering its output with `innerHTML` would execute whatever a task's notes contain.
Escaping the *input* first is the wrong fix — it double-escapes fenced code blocks, which is
exactly where a build contract's pseudocode and interfaces live. `assets/markdown.js`
therefore sanitises at the *renderer* level, where marked has already decided whether a span
of text is raw HTML, a code block or a link:

- raw HTML is escaped and shown literally, never parsed
- code blocks stay correct — escaped exactly once, by marked's own renderer
- links are checked against an `http`/`https`/`mailto` allowlist (plus relative and
  fragment), with entities and control characters decoded *before* the check so
  `jav&#x61;script:` and `java\tscript:` cannot smuggle a scheme through
- images are reduced to their alt text: no `src`, no network request
- GFM task-list checkboxes render as text, so the output contains no form controls

`internal/webui/markdown_test.js` covers this, including the XSS cases, and runs as part of
`go test ./...`.

## Task notes carry a build contract

The `notes` field description on `create_task`, `update_task` and `scaffold_plan` asks for a
**build contract** — Problem, Action Items, Interfaces, Pseudocode, Validation contract,
Non-goals, References — so a worker can execute a task without re-reading a parent document.

It lives in the *field description* rather than a document on purpose: tool schemas are
re-sent to the model on every call, so the guidance reaches it whether or not the harness
loads any skill file. It is a *shape* ("what a finished note contains"), not a procedure;
multi-step method belongs in a prompt, not a field description.

Note what this does **not** give you: graphd has no project-level document, so a
project-wide design/RFC still has nowhere to live. The graphd-native answer is to make the
design a task — put the RFC in its `notes` and have the other tasks depend on it via an
edge. It then shows up on the canvas, in the frontier, and in blast-radius math for free.

## Decisions this build had to make

Where the spec was silent, the boring choice was taken. Notes:

1. **Cycle path closure (§5.5 vs §11.5).** §5.5's pseudocode says `append(path, blocker)`,
   but its own worked example and the §11.5 assertion are `[3,4,7,3]`. That is only a
   closed cycle if the final element is the *blocked* node, so the code returns
   `blocked → … → blocker → blocked`. §11.5 is authoritative.
2. **Two extra error codes.** The §6.2 table has no code for a `projects.name` /
   `key_prefix` collision (`duplicate_project`, 409) or for a malformed request body
   (`invalid_input`, 400). Both are extensions; everything in the table is implemented as
   specified.
3. **Project delete is the one hard delete.** `DELETE /api/projects/{pid}` cascades. Task
   "deletion" is always the soft archive. This matches "no row is ever hard-deleted" as it
   applies to tasks. The UI exposes it as a header **delete** button; because the endpoint
   guards on `?confirm=<name>`, the client asks you to *type* the project name rather than
   press Enter on a prefilled prompt.
4. **Export keys edges by task key**, not integer id, so an export is portable between
   databases. Positions and archived flags round-trip.
5. **The board fragment is a separate endpoint** (`GET /api/projects/{pid}/board`) rather
   than a partial on `/board`. `/board` serves the HTML shell; the fragment is swapped in on
   every SSE revision change. This keeps the shell static and the fragment cheap.
6. **The dagre engine calls dagre's graphlib directly** instead of going through
   cytoscape-dagre. cytoscape-dagre asks cytoscape for each node's *rendered* dimensions,
   and headless cytoscape reports 0×0 for nodes that were never drawn, silently degenerating
   the layout to a single point. Node size is fixed at 180×60 by spec (§7.2), so the
   dimensions are known without asking the renderer. Same algorithm, same fixed options,
   same `LR` rank direction — but deterministic and testable without a DOM.
   `cytoscape-dagre` is still vendored and loaded (it is in the §3.4 table and registers the
   `name: 'dagre'` cytoscape layout).
7. **`/assets/` is served with `http.FileServerFS`** over the embedded FS; the shell is
   rendered from the same `index.html` with the view name substituted.
8. **`--seed-fixture` is a `serve` flag**, as specified — there is no separate seed
   subcommand. Tests start `serve --seed-fixture`, then stop it.
9. **Markdown notes and the build-contract guidance are additions beyond the SPEC.** The
   SPEC says `notes` is "a plain textarea"; rendering markdown is a view over unchanged
   storage, and the contract guidance is a tool-schema description. Both are deliberate
   drift toward plan-shaped tasks — see the two sections above. A fifth vendored file
   (`marked`) comes with the markdown rendering.

Open questions from §14 left for later: multiple projects on one canvas (a read-only
overlay, not a merged graph), a `graphd ready --watch` terminal view, and a second layout
engine (ELK) behind the same registry interface.

---

## Layout of the repo

```
main.go                     subcommand dispatch, flag parsing, loopback guard
internal/store/             schema, CRUD, graph algorithms, fixture, export/import
  graph.go                  LoadGraph, ReadySet, Unblocks, BlastRadius, WouldCycle
internal/api/               routes, handlers, board fragments, SSE, error codes
internal/mcp/               stdio loop, the 13 tools, transactional scaffold_plan
internal/webui/assets/      index.html, app.js, canvas.js, layout.js, board.js,
                            markdown.js, style.css
  vendor/                   cytoscape, dagre, cytoscape-dagre, cytoscape-edgehandles,
                            marked, lodash-shim.js (see below)
internal/webui/layout_test.js    the §11.6 determinism check, run under node
internal/webui/markdown_test.js  markdown rendering + XSS checks, run under node
```

The vendored libraries are the four in §3.4 plus one 40-line shim:
`cytoscape-edgehandles`'s UMD build declares `lodash.memoize` and `lodash.throttle`
as *external* modules and resolves them from a global `_`. Rather than vendor the whole of
lodash (~70KB) for two helpers, `vendor/lodash-shim.js` provides exactly those two. It is a
plain committed file with no build step, like everything else here.

The only backend dependency is `modernc.org/sqlite` (pure Go, no cgo). Everything else is
stdlib. The vendored front-end libraries are UMD single-file builds committed to the repo.
