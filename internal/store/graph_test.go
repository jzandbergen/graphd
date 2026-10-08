package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// newTestStore opens a fresh database in a temp dir and seeds the fixture.
func newTestStore(t *testing.T) (*Store, *Project) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	p, err := s.SeedFixture(context.Background())
	if err != nil {
		t.Fatalf("SeedFixture: %v", err)
	}
	return s, p
}

func mustGraph(t *testing.T, s *Store, pid int64) *Graph {
	t.Helper()
	g, err := s.LoadGraph(context.Background(), pid)
	if err != nil {
		t.Fatalf("LoadGraph: %v", err)
	}
	return g
}

// §11.2 — the crux. ReadySet() == {3, 11, 14}. 13 is cancelled so it is not
// ready; everything else has at least one open blocker.
func TestReadySetFixture(t *testing.T) {
	s, p := newTestStore(t)
	g := mustGraph(t, s, p.ID)

	got := g.ReadyIDs()
	want := []int64{3, 11, 14}
	if !equalIDs(got, want) {
		t.Fatalf("ReadySet = %v, want %v", got, want)
	}
}

// §11.3 — leverage correctness, then close task 3 and re-assert.
func TestLeverageFixture(t *testing.T) {
	ctx := context.Background()
	s, p := newTestStore(t)
	g := mustGraph(t, s, p.ID)

	assertUnblocks := func(id int64, want int) {
		t.Helper()
		if got := g.Unblocks(g.Tasks[id]); got != want {
			t.Errorf("unblocks(%d) = %d, want %d", id, got, want)
		}
	}
	assertUnblocks(3, 3) // closing 3 readies 4, 5 and 17
	assertUnblocks(11, 0)
	assertUnblocks(14, 0)
	assertUnblocks(1, 0) // already terminal

	if got := g.BlastRadius(g.Tasks[3]); got != 14 {
		t.Errorf("blast_radius(3) = %d, want 14", got)
	}

	// Close task 3 and re-assert.
	if _, err := s.UpdateTask(ctx, 3, TaskPatch{Status: ptr(StatusDone)}); err != nil {
		t.Fatalf("close 3: %v", err)
	}
	g = mustGraph(t, s, p.ID)

	got := g.ReadyIDs()
	want := []int64{4, 5, 11, 14, 17}
	if !equalIDs(got, want) {
		t.Fatalf("after closing 3, ReadySet = %v, want %v", got, want)
	}
	if got := g.Unblocks(g.Tasks[5]); got != 1 { // closing 5 readies 19
		t.Errorf("unblocks(5) = %d, want 1", got)
	}
	if got := g.Unblocks(g.Tasks[17]); got != 1 { // closing 17 readies 18
		t.Errorf("unblocks(17) = %d, want 1", got)
	}
	// The important one: leverage is not naively transitive. 6 still needs 5.
	if got := g.Unblocks(g.Tasks[4]); got != 0 {
		t.Errorf("unblocks(4) = %d, want 0 (6 still needs 5)", got)
	}
}

// §11.4 — frontier ordering. Ties on unblocks break by priority, not by id.
func TestFrontierOrdering(t *testing.T) {
	s, p := newTestStore(t)
	ready, err := s.GetReady(context.Background(), p.ID, 0)
	if err != nil {
		t.Fatalf("GetReady: %v", err)
	}
	if len(ready.Ready) != 3 {
		t.Fatalf("len(ready) = %d, want 3", len(ready.Ready))
	}
	want := []struct {
		key      string
		unblocks int
		prio     int
	}{
		{"FIX-3", 3, 1},
		{"FIX-14", 0, 2},
		{"FIX-11", 0, 3},
	}
	for i, w := range want {
		got := ready.Ready[i]
		if got.Key != w.key || got.Unblocks != w.unblocks || got.Priority != w.prio {
			t.Errorf("ready[%d] = {%s, unblocks %d, prio %d}, want {%s, unblocks %d, prio %d}",
				i, got.Key, got.Unblocks, got.Priority, w.key, w.unblocks, w.prio)
		}
	}
}

// §11.5 — cycle rejection. The graph is unchanged afterwards.
func TestCycleRejection(t *testing.T) {
	ctx := context.Background()
	s, p := newTestStore(t)
	g := mustGraph(t, s, p.ID)
	revBefore, _ := s.Revision(ctx)

	_, err := s.AddEdge(ctx, p.ID, 7, 3, "")
	e := AsError(err)
	if e.Code != CodeCycleDetected {
		t.Fatalf("code = %q, want %q (err=%v)", e.Code, CodeCycleDetected, err)
	}
	wantPath := []int64{3, 4, 7, 3}
	if !equalIDs(e.Cycle, wantPath) {
		t.Errorf("cycle = %v, want %v", e.Cycle, wantPath)
	}

	// Graph unchanged: edge count still 24, revision unchanged.
	g2 := mustGraph(t, s, p.ID)
	if len(g2.Edges) != 24 {
		t.Errorf("edge count = %d, want 24", len(g2.Edges))
	}
	revAfter, _ := s.Revision(ctx)
	if revBefore != revAfter {
		t.Errorf("revision changed %d -> %d on a rejected write", revBefore, revAfter)
	}
	if !equalIDs(g.ReadyIDs(), []int64{3, 11, 14}) {
		t.Errorf("ready set changed after a rejected write")
	}

	cases := []struct {
		blocker, blocked int64
		wantCode         string
	}{
		{3, 3, CodeSelfEdge},
		{3, 4, CodeDuplicateEdge},
		{1, 2, CodeDuplicateEdge},
	}
	for _, c := range cases {
		_, err := s.AddEdge(ctx, p.ID, c.blocker, c.blocked, "")
		if got := AsError(err).Code; got != c.wantCode {
			t.Errorf("add_edge(%d,%d) code = %q, want %q", c.blocker, c.blocked, got, c.wantCode)
		}
	}
}

// §11.12 — archival semantics. Because cancelled is terminal, a task blocked by
// an archived task becomes ready, with no special case anywhere.
func TestArchivalSemantics(t *testing.T) {
	ctx := context.Background()
	s, p := newTestStore(t)

	// Task 6 is blocked by 4 and 5. Task 4 is not ready initially.
	if _, err := s.ArchiveTask(ctx, 4); err != nil {
		t.Fatalf("ArchiveTask: %v", err)
	}

	t4, err := s.GetTask(ctx, 4)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if !t4.Archived || t4.Status != StatusCancelled {
		t.Errorf("archived task = {archived:%v status:%q}, want {true cancelled}", t4.Archived, t4.Status)
	}

	// Default graph hides it.
	view, err := s.GraphView(ctx, p.ID, false)
	if err != nil {
		t.Fatalf("GraphView: %v", err)
	}
	for _, tk := range view.Tasks {
		if tk.ID == 4 {
			t.Errorf("archived task 4 still present in default graph")
		}
	}
	full, _ := s.GraphView(ctx, p.ID, true)
	found := false
	for _, tk := range full.Tasks {
		if tk.ID == 4 {
			found = true
		}
	}
	if !found {
		t.Errorf("archived task 4 missing from graph with include_archived=1")
	}

	// Task 4 becoming terminal unblocks 6 (which still needs 5) — but 7 needs 6,
	// so nothing downstream of 6 becomes ready. What must hold: task 4 is no
	// longer an open blocker of 6.
	g := mustGraph(t, s, p.ID)
	for _, b := range g.In[6] {
		if b == 4 && !isTerminal(g.Tasks[4].Status) {
			t.Errorf("task 4 is still an open blocker of 6")
		}
	}
}

// Inputs are the outputs of a task's *finished* immediate blockers, derived at
// read time from the edges that already exist (docs/task-outputs.md §4).
func TestInputsFinishedBlockersOnly(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	// Task 6 is blocked by 4 and 5. Before either finishes, 6 has no usable
	// inputs: an open blocker has not produced anything yet, and its promise is
	// already reported by blocked_by_open.
	if _, err := s.UpdateTask(ctx, 4, TaskPatch{Output: ptr("counter key scheme: rl:{tenant}:{window}")}); err != nil {
		t.Fatalf("set output 4: %v", err)
	}
	if _, err := s.UpdateTask(ctx, 5, TaskPatch{Output: ptr("config precedence: flag > env > file")}); err != nil {
		t.Fatalf("set output 5: %v", err)
	}
	open, err := s.TaskView(ctx, 6)
	if err != nil {
		t.Fatalf("TaskView(6): %v", err)
	}
	if len(open.Inputs) != 0 {
		t.Errorf("task 6 has %d inputs while both blockers are open, want 0", len(open.Inputs))
	}

	// Close both so 6 is ready, then read its derived inputs.
	for _, id := range []int64{4, 5} {
		if _, err := s.UpdateTask(ctx, id, TaskPatch{Status: ptr(StatusDone)}); err != nil {
			t.Fatalf("close %d: %v", id, err)
		}
	}
	t6, err := s.TaskView(ctx, 6)
	if err != nil {
		t.Fatalf("TaskView(6): %v", err)
	}
	if !t6.Ready {
		t.Fatalf("task 6 should be ready once 4 and 5 are done")
	}
	if len(t6.Inputs) != 2 {
		t.Fatalf("task 6 has %d inputs, want 2 (its blockers 4 and 5)", len(t6.Inputs))
	}
	// Ordered by blocker id.
	if t6.Inputs[0].ID != 4 || t6.Inputs[1].ID != 5 {
		t.Errorf("inputs out of order: got %d,%d want 4,5", t6.Inputs[0].ID, t6.Inputs[1].ID)
	}
	if t6.Inputs[0].Key != "FIX-4" || t6.Inputs[0].Status != StatusDone {
		t.Errorf("input[0] = %+v, want key FIX-4 status done", t6.Inputs[0])
	}
	if t6.Inputs[0].Output != "counter key scheme: rl:{tenant}:{window}" {
		t.Errorf("input[0].Output = %q", t6.Inputs[0].Output)
	}
	if t6.Inputs[1].Output != "config precedence: flag > env > file" {
		t.Errorf("input[1].Output = %q", t6.Inputs[1].Output)
	}

	// A blocker with an empty output still yields an entry: "closed without
	// recording findings" is a real state, not a reason to hide the edge.
	// Task 7 is blocked by 4 (done, has output) and 6 (open). Its inputs are 4
	// alone; 6 contributes nothing until it finishes.
	t7, err := s.TaskView(ctx, 7)
	if err != nil {
		t.Fatalf("TaskView(7): %v", err)
	}
	if len(t7.Inputs) != 1 || t7.Inputs[0].ID != 4 {
		t.Fatalf("task 7 inputs = %+v, want exactly the finished blocker 4", t7.Inputs)
	}

	// Now close 6, which has no output: the entry appears anyway, reporting the
	// empty result. "Closed without recording findings" is a real state.
	if _, err := s.UpdateTask(ctx, 6, TaskPatch{Status: ptr(StatusDone)}); err != nil {
		t.Fatalf("close 6: %v", err)
	}
	t7, err = s.TaskView(ctx, 7)
	if err != nil {
		t.Fatalf("TaskView(7) after closing 6: %v", err)
	}
	if len(t7.Inputs) != 2 {
		t.Fatalf("task 7 inputs = %+v, want 2 once both blockers are terminal", t7.Inputs)
	}
	found6 := false
	for _, in := range t7.Inputs {
		if in.ID == 6 {
			found6 = true
			if in.Output != "" {
				t.Errorf("task 6 has no output, but input reports %q", in.Output)
			}
		}
	}
	if !found6 {
		t.Errorf("task 6 is a finished blocker of 7 but is missing from its inputs: %+v", t7.Inputs)
	}
}

// Inputs are DIRECT only. Task 8 is blocked by 6, which is blocked by 4. Closing
// 4 must not leak 4's output into 8's inputs — the same non-transitivity the
// leverage metric is tested for. This is the test that catches a naive
// transitive implementation (docs/task-outputs.md §9.2).
func TestInputsDirectOnly(t *testing.T) {
	ctx := context.Background()
	s, p := newTestStore(t)

	// 8's blockers: 6 only. 6's blockers: 4 and 5. Close 4 and 6, and give both
	// an output, so 8 has exactly one usable input — 6's.
	if _, err := s.UpdateTask(ctx, 4, TaskPatch{
		Output: ptr("DEEP-SECRET"), Status: ptr(StatusDone),
	}); err != nil {
		t.Fatalf("set up 4: %v", err)
	}
	if _, err := s.UpdateTask(ctx, 6, TaskPatch{
		Output: ptr("MIDDLE"), Status: ptr(StatusDone),
	}); err != nil {
		t.Fatalf("set up 6: %v", err)
	}

	g := mustGraph(t, s, p.ID)
	t8 := g.Tasks[8]
	if len(g.In[8]) != 1 || g.In[8][0] != 6 {
		t.Fatalf("fixture changed: In[8] = %v, want [6]", g.In[8])
	}
	inputs := g.Inputs(t8)
	if len(inputs) != 1 || inputs[0].ID != 6 {
		t.Fatalf("task 8 inputs = %+v, want exactly its direct blocker 6", inputs)
	}
	if inputs[0].Output != "MIDDLE" {
		t.Errorf("task 8's input = %q, want MIDDLE (its direct blocker's output)", inputs[0].Output)
	}
	for _, in := range inputs {
		if in.Output == "DEEP-SECRET" {
			t.Errorf("task 4's output leaked into task 8's inputs; inputs must be direct blockers only")
		}
	}
}

// Archiving a blocker removes its output from a dependent's inputs, with no
// special case — exactly as it stops blocking (docs/task-outputs.md §4.1).
func TestInputsExcludeArchived(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	// Give 4 and 5 outputs and close them, so both are usable inputs of 6.
	if _, err := s.UpdateTask(ctx, 4, TaskPatch{
		Output: ptr("output from 4"), Status: ptr(StatusDone),
	}); err != nil {
		t.Fatalf("set up task 4: %v", err)
	}
	if _, err := s.UpdateTask(ctx, 5, TaskPatch{
		Output: ptr("output from 5"), Status: ptr(StatusDone),
	}); err != nil {
		t.Fatalf("set up task 5: %v", err)
	}

	before, err := s.TaskView(ctx, 6)
	if err != nil {
		t.Fatalf("TaskView(6): %v", err)
	}
	if len(before.Inputs) != 2 {
		t.Fatalf("precondition: task 6 should have 2 inputs, got %d", len(before.Inputs))
	}

	if _, err := s.ArchiveTask(ctx, 4); err != nil {
		t.Fatalf("ArchiveTask(4): %v", err)
	}
	after, err := s.TaskView(ctx, 6)
	if err != nil {
		t.Fatalf("TaskView(6) after archive: %v", err)
	}
	if len(after.Inputs) != 1 {
		t.Fatalf("after archiving 4, task 6 has %d inputs, want 1: %+v", len(after.Inputs), after.Inputs)
	}
	if after.Inputs[0].ID != 5 {
		t.Errorf("remaining input = %d, want 5", after.Inputs[0].ID)
	}
}

// The frontier always carries derived inputs (docs/task-outputs.md §4.2).
func TestReadyCarriesInputs(t *testing.T) {
	ctx := context.Background()
	s, p := newTestStore(t)

	if _, err := s.UpdateTask(ctx, 3, TaskPatch{Output: ptr("ANALYSIS")}); err != nil {
		t.Fatalf("set output 3: %v", err)
	}
	if _, err := s.UpdateTask(ctx, 3, TaskPatch{Status: ptr(StatusDone)}); err != nil {
		t.Fatalf("close 3: %v", err)
	}

	ready, err := s.GetReady(ctx, p.ID, 0)
	if err != nil {
		t.Fatalf("GetReady: %v", err)
	}
	// Task 4 is blocked by 3 alone, and 3 is now done, so 4 is ready and its
	// single input is 3's output.
	var found bool
	for _, e := range ready.Ready {
		if e.Key != "FIX-4" {
			continue
		}
		found = true
		if len(e.Inputs) != 1 || e.Inputs[0].ID != 3 {
			t.Fatalf("FIX-4 inputs = %+v, want one entry for task 3", e.Inputs)
		}
		if e.Inputs[0].Output != "ANALYSIS" {
			t.Errorf("FIX-4 input output = %q, want ANALYSIS", e.Inputs[0].Output)
		}
	}
	if !found {
		t.Fatalf("FIX-4 not in frontier after closing 3: %+v", ready.Ready)
	}

	// Every entry reports inputs as a list, never a missing field: a task with no
	// blockers has an empty (non-nil) slice, so the JSON shape is stable.
	for _, e := range ready.Ready {
		if e.Inputs == nil {
			t.Errorf("%s: inputs is nil, want an empty slice", e.Key)
		}
	}
}

// A database created before tasks.output existed must gain the column on open,
// keep its rows, and report the new schema version (docs/task-outputs.md §7).
func TestOutputMigration(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "v1.db")

	// Build a v1 database by hand: the tasks table without output, plus the meta
	// rows a v1 build would have written.
	v1, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open v1: %v", err)
	}
	v1Schema := `
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE projects (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, key_prefix TEXT NOT NULL UNIQUE,
  next_task_number INTEGER NOT NULL DEFAULT 1, created_at TEXT NOT NULL);
CREATE TABLE tasks (
  id INTEGER PRIMARY KEY, project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  key TEXT NOT NULL, label TEXT NOT NULL, notes TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK (status IN ('todo','doing','done','cancelled')),
  priority INTEGER NOT NULL DEFAULT 3 CHECK (priority BETWEEN 1 AND 5),
  tags TEXT NOT NULL DEFAULT '', x REAL, y REAL, archived INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL, UNIQUE (project_id, key));
CREATE TABLE edges (
  id INTEGER PRIMARY KEY, project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  blocker_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  blocked_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  label TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL,
  UNIQUE (project_id, blocker_id, blocked_id), CHECK (blocker_id <> blocked_id));
INSERT INTO meta(key,value) VALUES ('revision','7');
INSERT INTO meta(key,value) VALUES ('schema_version','1');
INSERT INTO projects(id,name,key_prefix,next_task_number,created_at)
  VALUES (1,'legacy','LEG',3,'2026-01-01T00:00:00Z');
INSERT INTO tasks(project_id,key,label,notes,status,priority,tags,archived,created_at,updated_at)
  VALUES (1,'LEG-1','old task','a note','done',2,'',0,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z');
`
	if _, err := v1.Exec(v1Schema); err != nil {
		v1.Close()
		t.Fatalf("build v1 schema: %v", err)
	}
	if err := v1.Close(); err != nil {
		t.Fatalf("close v1: %v", err)
	}

	// Open with the current build: migrate() must add the column.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open v1 database: %v", err)
	}
	defer s.Close()

	// The pre-existing row survives, with output defaulting to empty.
	tk, err := s.GetTask(ctx, 1)
	if err != nil {
		t.Fatalf("GetTask on migrated row: %v", err)
	}
	if tk.Label != "old task" || tk.Notes != "a note" {
		t.Errorf("migrated row lost data: %+v", tk)
	}
	if tk.Output != "" {
		t.Errorf("migrated row output = %q, want empty", tk.Output)
	}

	// And the column is writable now.
	if _, err := s.UpdateTask(ctx, 1, TaskPatch{Output: ptr("written after migration")}); err != nil {
		t.Fatalf("write output after migration: %v", err)
	}
	again, err := s.GetTask(ctx, 1)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if again.Output != "written after migration" {
		t.Errorf("output after migration = %q", again.Output)
	}

	// The revision counter is untouched by migration; schema_version moves.
	rev, err := s.Revision(ctx)
	if err != nil {
		t.Fatalf("Revision: %v", err)
	}
	if rev != 8 { // 7 from the v1 file, +1 for the output write above
		t.Errorf("revision = %d, want 8", rev)
	}
	var sv string
	if err := s.DB().QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&sv); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if sv != fmt.Sprint(SchemaVersion) {
		t.Errorf("schema_version = %q, want %q", sv, fmt.Sprint(SchemaVersion))
	}
	if SchemaVersion != 2 {
		t.Errorf("SchemaVersion = %d, want 2", SchemaVersion)
	}
}

// Migration must be idempotent: opening twice must not fail on a duplicate
// column, and a fresh database must already have the column.
func TestMigrationIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "twice.db")
	for i := 0; i < 3; i++ {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("Open #%d: %v", i+1, err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i+1, err)
		}
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("final Open: %v", err)
	}
	defer s.Close()
	if _, err := s.SeedFixture(context.Background()); err != nil {
		t.Fatalf("SeedFixture on migrated db: %v", err)
	}
}

// The fixture must be a DAG (§11.1).
func TestFixtureIsDAG(t *testing.T) {
	s, p := newTestStore(t)
	g := mustGraph(t, s, p.ID)
	if cycle, ok := g.findCycle(); ok {
		t.Fatalf("fixture contains a cycle: %v", g.cycleString(cycle))
	}
	if len(g.Tasks) != 20 {
		t.Errorf("fixture has %d tasks, want 20", len(g.Tasks))
	}
	if len(g.Edges) != 24 {
		t.Errorf("fixture has %d edges, want 24", len(g.Edges))
	}
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func ptr[T any](v T) *T { return &v }
