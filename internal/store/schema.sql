-- graphd schema. Applied with CREATE TABLE IF NOT EXISTS on every open, so it
-- is idempotent. schema_version lives in the meta table.

CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

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
  -- output is what the task produced or found, as opposed to notes, which is
  -- what it is meant to do. Plain text, markdown as a view. A dependent task's
  -- *inputs* are these values, derived at read time from the edges that already
  -- exist — never copied, never stored (docs/task-outputs.md §2).
  output     TEXT    NOT NULL DEFAULT '',
  status     TEXT    NOT NULL CHECK (status IN ('todo','doing','done','cancelled')),
  priority   INTEGER NOT NULL DEFAULT 3 CHECK (priority BETWEEN 1 AND 5),
  tags       TEXT    NOT NULL DEFAULT '',     -- comma-separated
  -- owner is who is expected to do the work: 'agent' or 'human'. It is an
  -- AUTHORED fact, like label or priority — there is nothing in the graph to
  -- derive it from, and nothing depends on it being true for the frontier math
  -- to be correct. It is therefore stored, unlike `blocked`/`ready`
  -- (docs/task-owners.md §2).
  owner      TEXT    NOT NULL DEFAULT 'agent' CHECK (owner IN ('agent','human')),
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
