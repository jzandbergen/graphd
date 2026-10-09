# graphd

**graphd is a local work graph: tasks are nodes, blockers are arrows.** It shows
what you can start now, and passes each task its blockers' results.

![the canvas](docs/img/canvas.png)

One binary. One SQLite file. No server, no account, no sync, no cloud.

---

## Feature set

**The graph**

- Tasks as nodes, blockers as labelled arrows, drawn `blocker → blocked`.
- A computed **ready frontier**: every task whose blockers are all closed.
- **`unblocks`** — how many tasks closing this one would make ready, *right now*.
- **`blast radius`** — everything downstream of a task, transitively.
- **Lenses** — narrow the canvas to the ready frontier, one task's blast radius,
  or its connected component.
- Auto-layout, with a horizontal / vertical toggle and a grid fallback.
- Search by key or label; show or hide archived tasks.

**Two views of the same state**

- **Canvas** — the whole graph, arranged so work flows left to right.
- **Board** — a kanban of the same tasks; drag a card to change its status.

**Per task**

- `notes` — what the task must do. Markdown, rendered.
- `output` — what it produced or found. Markdown, rendered.
- `inputs` — read-only: the recorded outputs of the tasks that block it.
  Derived from the edges, never copied, so it cannot go stale.
- Status, priority (1–5), tags, archive / restore.

**Live**

- The UI repaints itself when an agent writes to the database. No refresh button
  needed, and it never overwrites the field you are typing in.

**For agents**

- `graphd mcp` — an MCP server with thirteen tools, so an agent can read the
  frontier and file work while the UI is closed.

---

## Install

```sh
go build -o graphd .        # or: go install .
```

That is the whole build. No npm, no `node_modules`, no bundler — the web UI is
embedded in the binary.

## Quick start

```sh
# a demo project, so there is something to look at on day one
graphd serve --seed-fixture --open

# your own data (default: ~/.local/share/graphd/graphd.db)
graphd serve --open
```

`--open` launches your browser at the canvas. Then:

1. Press **+ new project** in the header and give it a name and a key prefix
   (e.g. `RATE`, which makes tasks `RATE-1`, `RATE-2`, …).
2. Have your agent file work into it — see *Where tasks come from* below.
3. Press **Layout** to arrange the graph, or drag nodes where you want them.

---

## Using it

### The canvas

Every task is a box. The box's outline tells you its state:

| state | looks like | means |
|---|---|---|
| `todo` | neutral box | not started |
| `doing` | amber box | in flight |
| `done` | green, dimmed | finished |
| `cancelled` | grey, dashed, very dim | abandoned or archived |
| **ready** | accent ring and a soft glow | startable *now* |
| **blocked** | red outline | waiting on an open blocker |

`ready` and `blocked` are computed for you from the graph — there is no "blocked"
checkbox anywhere, so they can never drift out of date. Closing a blocker is all
it takes for its dependents to light up.

### Making the graph

- **Drag from a node's edge** onto another node to make it a blocker.
- Click a node to open it; add or remove blockers under the **links** tab.
- A cycle is refused, and the error tells you the path it would have made.
- Click an edge to select it, then `Delete` to remove it.

### Writing things down

A task has two text fields, and the difference between them is the whole point:

- **`notes` is what the task must do** — the plan, the interfaces, the acceptance
  check. Written before the work.
- **`output` is what the task produced or found** — the decision, the measured
  numbers, the names you settled on. Written when it is done.

You never copy an output into the task that consumes it. When a task's blockers
finish, their outputs show up as that task's **inputs** automatically. Open a
task, go to **inputs**, and the handoff is right there — which is what makes a
dependency a handoff rather than just a gate.

Both fields are plain text stored as-is; markdown is only how they are displayed.
Use the **edit** / **preview** toggle, or `⤢` to read one full width.

### Filtering and lenses

- The **search** box dims everything that does not match a key or label.
- The **lens** picker narrows the canvas to one thing: the ready frontier, a
  task's blast radius, or its component.
- The **archived** checkbox shows archived tasks; otherwise they stay hidden.

### Keys

| key | does |
|---|---|
| `L` | run the layout |
| `F` | fit the graph to the window |
| `O` | toggle horizontal / vertical layout |
| `Ctrl+Z` | undo the last canvas change |
| `[` `]` | previous / next panel tab |
| `Esc` | close the full-width pane, then the panel |
| `Delete` | archive the selected task |

Dragging a node moves it and the position is saved. Scrolling or pinching zooms;
drag the background to marquee-select.

### The board

![the board](docs/img/board.png)

The same tasks, grouped by status. Drag a card to another column to change its
status — it moves immediately and reconciles with the server. Ready tasks sort
first, then by how much they unblock, then by priority.

### A task in the panel

![the detail panel](docs/img/panel.png)

A task opens as a sidebar with the fields you change constantly pinned at the
top (label, status, priority, tags, and the derived readouts), and the long
content in tabs below: **notes · output · inputs · links**. Drag the left edge to
resize it.

---

## Where tasks come from

Agents. graphd is built for a model to file and work a plan, and `graphd mcp`
exposes the graph to it over MCP:

```sh
graphd mcp        # newline-delimited JSON-RPC on stdin/stdout
```

Thirteen tools — read the frontier (`get_ready`, `get_next_task`), write work
(`create_task`, `update_task`, `add_edge`), and file a whole plan in one
transaction (`scaffold_plan`). `get_next_task` returns the work *and* its inputs,
so the agent does not need a second lookup.

The MCP process talks to the same SQLite file directly, so it does not need the
server running: an agent can file work while the UI is closed, and the UI picks
it up live when you open it.

> **Note:** today the UI creates **projects**, not tasks. Tasks come from an agent
> or the HTTP API; the UI is where you read, shape, and arrange the graph.
> `--seed-fixture` gives you a populated project to explore immediately.

---

## Configuration

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

Environment: `GRAPHD_DB`, `GRAPHD_LISTEN`, `GRAPHD_PROJECT`, `GRAPHD_LOG_LEVEL`.
Flags override the environment. There is no config file.

**There is no authentication.** `serve` refuses to bind a non-loopback address
unless you pass `--allow-remote`, and you should not: anyone who can reach the
port can read and change everything. graphd is a single-user tool for your own
machine.

---

## Documentation

- **[docs/design.md](docs/design.md)** — how it works: the semantics, the
  architecture, the layout engine, the MCP surface, and the decisions behind them.
- **[docs/task-outputs.md](docs/task-outputs.md)** — the design of `output` and
  derived `inputs`.
- **[SPEC.md](SPEC.md)** — the original build specification.
