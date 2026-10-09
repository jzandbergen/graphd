# Task owners — SPEC delta

**Status:** implemented.
**Touches:** SPEC §4.1 (schema), §6.1 (response shapes), §6.2 (error codes), §8.2 (MCP tool
set), §9.3 (export).
**Amends:** nothing is removed. One column is added, one field rides on shapes that already
exist, and three tool signatures gain an optional argument.

---

## 1. The problem this closes

A graph of agent work has no way to say *"this step is mine."*

The plan for a real infrastructure project — migrate database X to a new VM — is mostly
agent work (provision the VM, dump and restore, write the new config) with one or two steps
that only a person can do: **the app switchover**, where the application is taken down,
repointed and started. That is not a task to hand to a model, and it is not a *blocker* in
the graph sense either. It is a task like any other; it simply has a different owner.

Without a way to mark it, two bad things happen:

- A worker running `get_next_task` in a loop is offered the switchover and may attempt it.
- A human reading the canvas cannot see, at a glance, where they personally come in. On a
  20-node plan, "which of these is mine" is exactly the question the canvas should answer
  and currently cannot.

## 2. The design rule

**Ownership is authored, not derived — and it is orthogonal to readiness.**

This is the whole argument, and it is why the feature is small.

The codebase has one load-bearing rule about stored fields: *derive what you can, because a
stored copy drifts* (`blocked`, `ready`, and a blocker's effect on its dependents are all
derived, never stored). A gate or an approval flag fails that rule — it is a fact about the
task's state that some other fact contradicts the moment the task is edited, so it drifts
and lies.

`owner` is not that. There is **nothing in the graph to derive it from**. No edge, no status
and no timestamp implies "a person must do this". It is the same kind of field as `label` or
`priority`: authored once by whoever wrote the plan, owned by them, and **nothing depends on
it being true for the frontier math to be correct**. If it is wrong the consequence is that
an agent is offered work it should have handed over — annoying and recoverable, not
corrupting. So it is stored, deliberately, and that is the correct choice rather than a
tolerated compromise.

And because it is orthogonal:

| | |
|---|---|
| `ready` | **unchanged.** A human-owned task is ready. It can be started right now — by a person. |
| `unblocks`, `blast_radius` | **unchanged.** Closing a human task readies its dependents exactly like any other. |
| `ReadySet`, `WouldCycle`, frontier ordering | **untouched.** `graph.go` does not learn that this field exists. |

That last row is the tell. Every existing frontier and leverage assertion still passes
without modification, because the graph algorithms never see the field.

## 3. The one place it changes behaviour

A flag nobody acts on is decoration, and the point of marking the switchover is that an
agent should not attempt it. So `get_next_task` — the tool that answers *"what should **I**,
an agent, do next"* — partitions on owner.

**It partitions by reporting, not by dropping.** The human work comes back in its own
bucket:

```jsonc
{"next": {"key": "DB-15", "label": "Decommission old VM", "owner": "agent", ...},
 "reason": "ok",
 "in_progress": [],
 "awaiting_human": [
   {"id": 14, "key": "DB-14", "label": "App switchover", "priority": 1, "owner": "human",
    "notes": "Take the app down, repoint DB_HOST, start, verify /health",
    "inputs": [{"key": "DB-11", "status": "done",
                "output": "new VM 10.0.4.19, pg16, role app_rw"}]}
 ]}
```

Three decisions are load-bearing here.

**The human task is not `next`.** `next` is the single field every harness reads and acts
on. Putting a production change there is an invitation to attempt it, and it stalls a
headless agent that cannot. A human task is also *not a blocker for the agent's queue*: the
agent should keep working downstream while the person gets to theirs. `next` stays
agent-owned.

**The bucket is reported, not silent.** The tempting implementation is to filter human tasks
out of the frontier. That would make `next: null` read as *"the project is finished"* when it
is only waiting on a person — a lie of omission, and this codebase is fastidious about not
telling one (`in_progress` exists for exactly this reason). Hence a third `reason` value:

| `reason` | means |
|---|---|
| `ok` | there is agent work ready |
| `no_ready_tasks` | nothing is ready; the project is blocked or finished |
| `awaiting_human` | nothing is ready **for an agent**, but the human has work |

`awaiting_human` is always present — `[]`, never a missing field — so a client never has to
tell "no human work" apart from "this shape does not carry the field", exactly like `inputs`.

**The frontier itself is not filtered.** `get_ready`, `GET /ready`, the canvas and the board
all show every ready task with its `owner` set. Filtering the frontier would hide the human's
own queue from the human, which is the opposite of the point. Only the *"what do I do"* tool
partitions.

**The instruction goes in the tool description**, not only in this document: a schema is
re-sent to the model on every call, so the guidance reaches it whether or not the harness
loads any skill file. That is the same mechanism the build-contract guidance uses.

> `awaiting_human` lists ready tasks owned by the human. Report these to the user and do not
> attempt them yourself: instruct the user to perform the task, and mark it done only after
> they confirm. When `next` is null and `awaiting_human` is non-empty the project is waiting
> on the user, not finished.

**The agent relays; the human confirms.** "Mark it done" is deliberately *not* "the agent
decides it is done". The flag exists because the agent cannot verify the work happened; an
agent that closes the task off its own say-so has re-introduced the problem the flag was
there to avoid. The agent surfaces the task and waits. The human then either clicks done in
the UI, or says "done" and the agent writes it — the human is the authority, the agent is
the hands.

### 3.1 Handing a task over mid-flight

An agent that discovers a task is not its to do can hand it over: `update_task` with
`owner: "human"` on its own task is allowed, and is the natural move — it has the context to
describe what it found. This is not a privileged operation and needs no new tool.

But a hand-off that produces a task nobody can see is worse than no hand-off, and there is a
gap here worth stating precisely. `awaiting_human` is drawn from the frontier, and the
frontier requires `status = 'todo'`. So a task that is `doing` **and** human-owned appears in
*no* bucket of `get_next_task`:

| state | where it is reported |
|---|---|
| human, `todo` | `awaiting_human` |
| human, `doing` | `in_progress` **only** — not in the frontier |
| human, `done`/`cancelled` | nowhere; it is closed |

That third row is why `in_progress` carries `owner`. Without it, an agent that handed a task
over while it was in flight would have written it into a hole: invisible to the agent queue
(it is not `todo`), absent from `awaiting_human` (same reason), and unlabelled in the one list
it does appear in — so nothing told the user it had become theirs. `TestMCPAgentCanHandTaskOver`
pins the field.

The gap itself is inherent to `doing` (a claimed task is invisible to every other worker by
design, §2.1) and is **not** fixed by widening `awaiting_human` to include `doing` tasks: that
would list work already in flight as though it were waiting to be started, and would fight the
frontier ordering. The answer is the documented one — an agent handing work over should return
the task to `todo` in the same write (or in the next), so it lands in `awaiting_human` where
the human will see it. The `in_progress.owner` field is the safety net for the case where it
does not.

### 3.2 The agent must not close the human's work

The whole reason `owner` exists is that an agent cannot verify a person's work happened. So
the one write that would defeat it is an agent closing a human-owned task. `update_task`
(`status: done`/`cancelled`) and `archive_task` therefore refuse it over MCP:

```jsonc
{"isError": true,
 "text": "human_confirmation_required: FIX-3 is owned by the human, so it cannot be closed
          from here. Report it to the user, wait for them to confirm the work is done, and
          only then record it: update_task with status=done (or archive_task). If the work
          was actually yours to do, take it back first with owner=agent."}
```

**This is a guard, not a permission system**, and it is deliberately narrow:

- It fires only on an agent **closing** a human task. Every other field stays writable — an
  agent can set `notes`, `priority`, `tags`, or `status: doing` on a human task, and can
  still hand its own task over or take one back.
- It follows the owner and does not latch: taking the task back (`owner: agent`) restores the
  ability to close it.
- It is enforced at the **MCP boundary only**. The HTTP API is the human's own surface — it is
  how you mark your task done from the panel — and is unguarded. A per-request identity does
  not exist in graphd (localhost, single user, no auth), so this is a *cooperative* guard
  against the realistic failure — a model marking its own homework — and not a boundary
  against a determined caller. A cooperative guard is enough here for the same reason the
  `doing` claim is cooperative: the actor it constrains is the one being helped.

The MCP tool descriptions carry the same instruction, so a model is told before it tries:
`get_next_task` says to mark a human task done *only after they confirm*, and `update_task`
and `archive_task` say they refuse it.

The alternative — no guard — is defensible on the grounds that the confirmation contract is
in the prompt and the tool description already. It was rejected because the failure is silent
and corrupting: an agent that closes your switchover task leaves you with a project that
claims to be finished and a graph that has moved past your step.

## 4. What falls out for free

- **A human task that blocks an agent task keeps blocking it.** Closing it readies the
  dependent whatever the owner, so `unblocks` counts it normally. No code.
- **The handoff works unchanged.** `inputs` is derived from the edges, so a human task
  receives its blockers' `output` exactly as an agent task does. The person doing the
  switchover sees the new VM's address in the same panel an agent would.
- **Archival is unchanged.** An archived human task is cancelled and stops blocking, by the
  rule that already exists.

## 5. Storage and migration

One column:

```sql
owner TEXT NOT NULL DEFAULT 'agent' CHECK (owner IN ('agent','human'))
```

`meta.schema_version` goes `2 → 3`. The column is added by the same idempotent step in
`migrate()` that added `output` — it checks `pragma_table_info('tasks')` before issuing the
`ALTER TABLE`, so an existing database is upgraded in place and opening it twice is harmless.
Rows written before the field existed default to `agent`: work filed before this feature was
agent work, never human. (`ALTER TABLE` cannot carry a `CHECK`, so the constraint is enforced
in the store on every write path; `TestOwnerMigration` asserts the invalid value is refused
after a migration.)

`owner` is validated on create, patch, scaffold and import. An empty value means `agent` —
a caller that never heard of the field keeps working, and the default is the boring one.

## 6. Surface changes

| surface | change |
|---|---|
| `POST /api/projects/{pid}/tasks` | accepts `owner` |
| `PATCH /api/tasks/{tid}` | accepts `owner` |
| `GET /api/tasks/{tid}`, graph, ready, board | returns `owner`; a human card carries a marker |
| `create_task`, `update_task` | new optional `owner` |
| `scaffold_plan` | `tasks[].owner` |
| `get_graph`, `get_ready` | return `owner`; frontier entries carry it |
| `get_next_task` | partitions on owner; `awaiting_human` bucket; `reason` gains `awaiting_human`; `in_progress` entries carry `owner` |
| `update_task` | **refuses** `status: done`/`cancelled` on a human-owned task |
| `archive_task` | **refuses** a human-owned task |
| export/import | `owner` round-trips (`omitempty`; an absent value means agent) |
| error codes | `invalid_owner` (400), `human_confirmation_required` (409) |
| canvas | human-owned nodes are cut-corner boxes; a `human` lens |
| detail panel | an owner control in the glance strip |
| `--seed-fixture` | task 10, "Rollout behind flag", is human-owned — a realistic manual step |

The MCP tool count is **unchanged at thirteen**. `owner` rides on tools that already exist.

## 7. What this deliberately does not do

- **No gate.** Owner is not "may this be closed". Nothing about closing is *blocked* by the
  graph; the MCP guard in §3.2 is about *who may write the close*, not about whether the work
  is finished, and there is no approval state to drift.
- **No fifth status.** A human task is `todo`, `doing`, `done` or `cancelled` like any other.
- **No `blocked`-style derived field.** Ownership cannot be derived, so it is stored — and
  it is the kind of stored field that is safe.
- **No filter on the frontier.** `get_ready` and the UI show everything.
- **No new tool, no lock, no claim, no identity.** The guard in §3.2 is cooperative and
  applies to MCP only; there is no per-request user.

## 8. Validation contract

1. `TestOwnerDoesNotChangeFrontier` — marking the frontier's head human leaves `ReadyIDs`
   and `unblocks` byte-identical, and `/ready` still lists it first with `owner` set. This is
   the assertion that the feature is orthogonal.
2. `TestGetNextTaskPartitionsHuman` — the agent's `next` skips the human task and the human
   task appears under `awaiting_human`, in frontier order.
3. `TestGetNextTaskAwaitingHumanReason` — when every ready task is human, `next` is null and
   `reason == "awaiting_human"`, not `no_ready_tasks`.
4. `TestGetNextTaskNoReadyTasksUnchanged` — a project with nothing ready and nothing human
   still reports `no_ready_tasks` with `awaiting_human: []`. The pre-existing behaviour is
   pinned.
5. `TestOwnerValidationAndDefault` — omitted means agent; garbage is refused on create and
   patch with `invalid_owner`; a refused patch changes nothing.
6. `TestOwnerMigration` — a v2 database (has `output`, no `owner`) opens, gains the column,
   defaults its rows to agent, is writable, and rejects an invalid value.
7. `TestOutputMigration` — a v1 database gains both columns and reports `schema_version = 3`.
8. `TestExportImportRoundTrip` — `owner` survives export → import; an untouched task stays
   agent-owned.
9. `TestMCPGetNextTaskPartitionsHuman` / `TestMCPGetNextTaskAwaitingHumanReason` /
   `TestMCPNextTaskAwaitingHumanAlwaysPresent` — the partition over the wire, and the stable
   shape.
10. `TestMCPOwnerGuidanceInSchema` — the hand-off instruction reaches the model through the
    `get_next_task` description and the `owner` field description.
11. `TestAPIOwnerPatch` / `TestAPIReadyCarriesOwnerUnfiltered` / `TestAPIBoardMarksHumanCards`
    — the API writes it, the frontier keeps it, the board marks the right card.
12. `TestMCPAgentCanHandTaskOver` — an agent may hand its own task to the human, and the
    resulting `doing` + human task is labelled `owner: human` in `in_progress`.
13. `TestMCPAgentCannotCloseHumanTask` — `status: done`, `status: cancelled` and
    `archive_task` are each refused with `human_confirmation_required`, and the task is
    unchanged.
14. `TestMCPHumanCloseGuardIsNarrow` — every non-closing write still succeeds, taking a task
    back restores the ability to close it, and the guard does not latch off.
15. `panel_test.js` — the owner control is defined, populated and wired; ownership is a node
    *shape*, not a reintroduced glyph; the human lens exists.
