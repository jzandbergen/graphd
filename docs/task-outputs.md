# Task outputs and derived inputs — SPEC delta

**Status:** implemented on `feat/task-outputs`.
**Touches:** SPEC §2 (semantics), §4.1 (schema), §6.1 (response shapes), §8.2 (MCP tool set), §9.3 (export).
**Amends:** nothing is removed. One column is added, one derived field is added, and three tool
signatures gain an optional argument.

---

## 1. The problem this closes

The graph models *ordering* well and *handoff* not at all. A task can say "I cannot start until
you finish" but not "here is what I found, use it". The README already names half of this gap:

> graphd has no project-level document, so a project-wide design/RFC still has nowhere to live.
> The graphd-native answer is to make the design a task — put the RFC in its `notes` and have the
> other tasks depend on it via an edge.

That solves the **input** side. It leaves the **result** side with nowhere to go:

- Step 1 analyses something. Its conclusion is not a note *about step 1*; it is the artifact
  step 2 consumes.
- Step 2 depends on step 1, so the graph already knows the relationship. But nothing reads
  step 1's conclusion back out. `get_next_task` and `get_ready` return
  `{id, key, label, priority, unblocks, blocked_by_open}` — no notes, no output. An agent
  asking "what next" is told *what* to do and not *with what*.
- The two workarounds are both wrong. Copying the analysis into step 2's `notes` is a snapshot
  that rots when step 1 is revised. Putting it in the edge `label` is forbidden outright:
  SPEC §2.2 says the label is decoration and no logic may branch on it.

## 2. The design rule

**An output is not an input.**

- An **output** is a fact the producer owns. It lives on the producing task, in one column, with
  exactly one writer. It is edited by whoever is doing that task.
- An **input** is a *reference* from a consumer to a producer's output. It is **derived**, at read
  time, from the edges that already exist. It is never stored.

This is deliberately the same shape as the two rules the codebase already treats as load-bearing:

| stored (owned) | derived (computed) |
|---|---|
| `status` | `blocked`, `ready` |
| `archived` | whether an archived blocker still blocks |
| `output` | `inputs` |

Copying an output into the consumer would be the same class of mistake as storing `blocked`: the
moment the producer is revised, the copy is wrong and nothing notices.

**The dependency is the edge that already exists.** `step1 → step2` means *step 2 consumes step
1's output*. There is no second relationship, no `input` column, and no new edge type. A task with
three blockers has three inputs; a task with none has none.

## 3. Storage

One column on `tasks`:

```sql
output TEXT NOT NULL DEFAULT ''
```

Plain text. Markdown is a *view*, applied in the detail panel exactly as it is for `notes` — so
this stays inside SPEC §1.3's "docs-on-nodes, rich text, attachments" non-goal the same way notes
did. No document entity, no attachment, no rich-text editor, and the column stays lossless through
export/import.

`output` is **not** the same field as `notes` and the two are not merged:

- `notes` describes **what to do** — for non-trivial work, the build contract (problem, action
  items, interfaces, validation contract). It is written *before* the work and read by the person
  doing it.
- `output` records **what was found or produced** — the analysis, the decision, the measurements,
  the resulting artifact reference. It is written *after* the work and read by whoever depends on
  it.

Merging them would destroy the distinction between an instruction and a result, which is the whole
point.

## 4. Derived inputs

`inputs` is computed per task from its **finished** blockers, in the same pass as `ready` /
`blocked_by` / `unblocks` (`Graph.Derive`), and never stored:

```jsonc
"inputs": [
  {
    "id": 3,
    "key": "RATE-3",
    "label": "Token bucket middleware",
    "status": "done",
    "output": "…the producer's text…",
    "truncated": false
  }
]
```

Ordering is by blocker id ascending, matching `blocked_by` and every other list in the system
(SPEC §5.6).

### 4.1 Rules

- **Finished blockers only.** An input is material the consumer can actually use, and an open
  blocker has not produced anything yet. Its *promise* is already modelled — that is exactly what
  `blocked_by_open` reports — so listing it again as an input would say the same thing twice, and
  would fill the board with `no output recorded` markers for work nobody has started. When a task
  is ready every blocker is terminal, so a ready task's `inputs` are the complete handoff.
- **Direct dependents only.** Inputs are the outputs of *immediate* blockers, not the transitive
  closure. This is the same call SPEC §2.4 makes for `unblocks`, and for the same reason: two hops
  down, the intermediate task's output is where the synthesis belongs. A transitive dump would
  re-create the naive-transitivity bug that `TestLeverageFixture` exists to catch.
- **Archived blockers contribute nothing.** An archived task is cancelled; its output is no longer
  an input, in the same way it no longer blocks. One rule, no special case.
- **Readiness stays status-based.** A blocker that reached `done` with an empty output is a real
  state — "closed without recording findings". It is reported (the entry is present, with an empty
  `output`) rather than treated as a blocker. Gating readiness on output would make the frontier
  depend on prose, which would make it unpredictable. Note the asymmetry with the first rule, and
  that it is deliberate: an empty output from a *finished* task is reported; an absent output from
  an *open* one is not listed at all.
- **One output per task.** Multiple named outputs would be a document entity wearing a hat. Two
  artifacts are two tasks, connected by edges.

### 4.2 Size, and where truncation happens

Analyses are long and `get_ready` is meant to be cheap, so the frontier is the wrong place for a
full text dump. Truncation is a **presentation** concern and happens in the frontier only:

| surface | `inputs` | truncation |
|---|---|---|
| `GET /api/projects/{pid}/graph` | full text | none |
| `GET /api/tasks/{tid}` | full text | none |
| `get_graph` (MCP) | full text | none |
| `GET /api/projects/{pid}/ready` | full text | none |
| `get_ready` (MCP) | capped | 512 bytes per input, `truncated: true` |
| `get_next_task` (MCP) | capped | 512 bytes per input, `truncated: true` |
| board fragment | markers only | n/a |

Truncation is applied at the MCP boundary (`mcp.toolGetReady` / `toolGetNextTask`), not in the
store, so `GET /ready` and the MCP tool do not have to agree on a number. A truncated input ends
at a line boundary where one is available within the cap, so pseudocode and list items do not get
cut mid-token; `truncated: true` tells the caller to fetch the full text with `get_graph` or by
reading the task.

### 4.3 No suppression flag

There is no `include_inputs` argument on any surface. The frontier is where a caller learns what it
can start, so it is also where it should learn what it would be starting with; keeping that text out
of a payload is a *presentation* decision and belongs at the boundary that cares. The store always
returns what is in the column, whole, and the MCP tools cap it. `GetReady` therefore keeps the
signature it always had:

```go
func (s *Store) GetReady(ctx context.Context, projectID int64, limit int) (*Ready, error)
```

`inputs` is always present on a frontier entry — `[]`, never a missing field — so a client never has
to distinguish "this task has no inputs" from "this response shape does not carry inputs".

## 5. Where it pays off

`get_next_task` is the tool that matters, because it is the one an agent calls to decide what to
do. It now returns the work **and its inputs**:

```jsonc
{"next": {
   "id": 4, "key": "RATE-4", "label": "Redis counter store", "priority": 2,
   "unblocks": 0, "blast_radius": 6, "blocked_by_open": [],
   "inputs": [{"id": 3, "key": "RATE-3", "status": "done",
               "output": "Counter key scheme: rl:{tenant}:{window}…", "truncated": false}]
 },
 "reason": "ok",
 "in_progress": []}
```

One call, no second lookup, and the handoff is in the response that says what to do. That is the
difference between a tracker and a handoff mechanism.

## 6. API and MCP surface changes

Nothing is removed; every addition is optional and defaults to the old behaviour.

| surface | change |
|---|---|
| `POST /api/projects/{pid}/tasks` | accepts `output` |
| `PATCH /api/tasks/{tid}` | accepts `output` |
| `GET /api/tasks/{tid}`, graph, ready | returns `output` and `inputs` |
| `create_task` | new optional `output` |
| `update_task` | new optional `output` |
| `scaffold_plan` | `tasks[].output` |
| `get_graph`, `get_ready`, `get_next_task` | return `output`; frontier entries carry `inputs` |
| export/import | `output` round-trips byte-losslessly |
| board fragment | `output` marker on a card that has one; `inputs` marker when non-empty |

The MCP tool count is **unchanged at thirteen**. `output` rides on tools that already exist; it
does not add one. SPEC §8.2's "do not invent more tools" is respected.

## 7. Migration

`meta.schema_version` goes `1 → 2`. The column is added by an idempotent step in `migrate()` that
checks `pragma_table_info('tasks')` before issuing

```sql
ALTER TABLE tasks ADD COLUMN output TEXT NOT NULL DEFAULT ''
```

so an existing database is upgraded in place and a fresh one is not touched twice. `CREATE TABLE
IF NOT EXISTS` alone cannot do this — it silently does nothing to a table that already exists,
which would leave every pre-existing database without the column and every query failing at
runtime.

`SchemaVersion` becomes `2` and is what `export` reports.

## 8. What this deliberately does not do

- No new status, no `blocked`-like stored flag.
- No second edge type, no `input` column, no separate "artifact" entity.
- No transitive input closure.
- No readiness gate on output presence.
- No multiple named outputs per task.
- No new MCP tool.

## 9. Validation contract

1. `TestInputsDerivedFromBlockers` — closing a blocker surfaces its output on the dependent's
   `inputs`, in blocker-id order, with the producer's key and status; an empty output still yields
   an entry.
2. `TestInputsFinishedBlockersOnly` — an open blocker contributes no input; once it reaches a
   terminal status its output appears. A finished blocker with an empty output still yields an
   entry.
3. `TestInputsDirectOnly` — a task two hops down does **not** see the first task's output. This is
   the analogue of `unblocks(4) == 0` and exists to catch a naive transitive implementation.
4. `TestInputsExcludeArchived` — archiving a blocker removes its output from the dependent's
   `inputs`, with no special case.
5. `TestOutputMigration` — a v1 database (no `output` column) opens, gains the column, keeps its
   rows, and reports `schema_version = 2`.
6. `TestOutputRoundTrip` — `output` survives export → import byte-for-byte.
7. `TestMCPGetNextTaskCarriesInputs` — `get_next_task` returns the top task's inputs, and a long
   output is truncated with `truncated: true` while `get_graph` still returns it whole.
8. `TestAPIOutputPatch` — `PATCH /api/tasks/{tid}` sets `output` and the value comes back on the
   task.
