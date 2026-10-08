package store

import (
	"context"
	"testing"
)

// §11.10 — scaffold_plan atomicity. A cycle in the edge set must create
// nothing at all and leave the revision untouched.
func TestScaffoldPlanAtomicity(t *testing.T) {
	ctx := context.Background()
	s, p := newTestStore(t)
	revBefore, _ := s.Revision(ctx)
	tasksBefore := countRows(t, s, "tasks", p.ID)
	edgesBefore := countRows(t, s, "edges", p.ID)

	// a -> b -> c -> a is a cycle.
	_, err := s.ScaffoldPlan(ctx, p.ID, []ScaffoldTask{
		{Ref: "a", Label: "A"},
		{Ref: "b", Label: "B"},
		{Ref: "c", Label: "C"},
		{Ref: "d", Label: "D"},
		{Ref: "e", Label: "E"},
	}, []ScaffoldEdge{
		{Blocker: "a", Blocked: "b"},
		{Blocker: "b", Blocked: "c"},
		{Blocker: "c", Blocked: "a"},
	})
	if got := AsError(err).Code; got != CodeCycleDetected {
		t.Fatalf("scaffold_plan cycle: code = %q, want %q (err=%v)", got, CodeCycleDetected, err)
	}
	if got := countRows(t, s, "tasks", p.ID); got != tasksBefore {
		t.Errorf("tasks after failed scaffold = %d, want %d", got, tasksBefore)
	}
	if got := countRows(t, s, "edges", p.ID); got != edgesBefore {
		t.Errorf("edges after failed scaffold = %d, want %d", got, edgesBefore)
	}
	if revAfter, _ := s.Revision(ctx); revAfter != revBefore {
		t.Errorf("revision after failed scaffold = %d, want %d", revAfter, revBefore)
	}

	// Now a valid plan: 5 tasks, a diamond plus a tail, refs resolved correctly.
	res, err := s.ScaffoldPlan(ctx, p.ID, []ScaffoldTask{
		{Ref: "design", Label: "Design"},
		{Ref: "mw", Label: "Middleware"},
		{Ref: "store", Label: "Store"},
		{Ref: "tests", Label: "Tests"},
		{Ref: "ship", Label: "Ship"},
	}, []ScaffoldEdge{
		{Blocker: "design", Blocked: "mw"},
		{Blocker: "mw", Blocked: "store"},
		{Blocker: "store", Blocked: "tests"},
		{Blocker: "mw", Blocked: "tests"},
		{Blocker: "tests", Blocked: "ship"},
	})
	if err != nil {
		t.Fatalf("valid scaffold_plan: %v", err)
	}
	if len(res.Tasks) != 5 {
		t.Fatalf("scaffold created %d tasks, want 5", len(res.Tasks))
	}
	if len(res.Edges) != 5 {
		t.Fatalf("scaffold created %d edges, want 5", len(res.Edges))
	}
	byLabel := map[string]*Task{}
	for _, tk := range res.Tasks {
		byLabel[tk.Label] = tk
	}
	// Ref resolution: the mw -> tests edge must connect the right two rows.
	g := mustGraph(t, s, p.ID)
	if !hasEdge(g, byLabel["Middleware"].ID, byLabel["Tests"].ID) {
		t.Errorf("edge mw -> tests was not created between the right tasks")
	}
	if !hasEdge(g, byLabel["Design"].ID, byLabel["Middleware"].ID) {
		t.Errorf("edge design -> mw was not created between the right tasks")
	}
}

// §9.3 — import replaces contents and rolls back entirely on failure.
func TestImportReplacesAndRollsBack(t *testing.T) {
	ctx := context.Background()
	s, p := newTestStore(t)

	// A valid import replaces the fixture: one project's worth of new tasks.
	good := &Export{
		SchemaVersion: 1,
		Project:       ExportProject{Name: "fixture", KeyPrefix: "FIX"},
		Tasks: []ExportTask{
			{Key: "FIX-1", Label: "Only task", Status: StatusTodo, Priority: 3},
		},
		Edges: []ExportEdge{},
	}
	if err := s.Import(ctx, p.ID, good); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if got := countRows(t, s, "tasks", p.ID); got != 1 {
		t.Fatalf("after import, tasks = %d, want 1 (import must replace, not merge)", got)
	}

	// A bad import — an edge referencing a missing key — must roll back and
	// leave the previous contents intact.
	bad := &Export{
		SchemaVersion: 1,
		Project:       ExportProject{Name: "fixture", KeyPrefix: "FIX"},
		Tasks: []ExportTask{
			{Key: "FIX-1", Label: "Should not survive", Status: StatusTodo, Priority: 3},
		},
		Edges: []ExportEdge{{Blocker: "FIX-1", Blocked: "FIX-999"}},
	}
	if err := s.Import(ctx, p.ID, bad); err == nil {
		t.Fatalf("Import with a dangling edge succeeded, want failure")
	}
	if got := countRows(t, s, "tasks", p.ID); got != 1 {
		t.Fatalf("after failed import, tasks = %d, want 1 (rollback)", got)
	}
	var label string
	if err := s.DB().QueryRow(`SELECT label FROM tasks WHERE project_id = ?`, p.ID).Scan(&label); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if label != "Only task" {
		t.Errorf("after failed import label = %q, want the pre-import value", label)
	}
}

// A cyclic import is rejected whole.
func TestImportRejectsCycle(t *testing.T) {
	ctx := context.Background()
	s, p := newTestStore(t)
	cyc := &Export{
		SchemaVersion: 1,
		Project:       ExportProject{Name: "fixture", KeyPrefix: "FIX"},
		Tasks: []ExportTask{
			{Key: "FIX-1", Label: "A", Status: StatusTodo, Priority: 3},
			{Key: "FIX-2", Label: "B", Status: StatusTodo, Priority: 3},
		},
		Edges: []ExportEdge{
			{Blocker: "FIX-1", Blocked: "FIX-2"},
			{Blocker: "FIX-2", Blocked: "FIX-1"},
		},
	}
	err := s.Import(ctx, p.ID, cyc)
	if got := AsError(err).Code; got != CodeCycleDetected {
		t.Fatalf("cyclic import code = %q, want %q", got, CodeCycleDetected)
	}
	if got := countRows(t, s, "tasks", p.ID); got != 20 {
		t.Errorf("after failed cyclic import, tasks = %d, want the fixture's 20", got)
	}
}

// Export must round-trip through import losslessly.
func TestExportImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, p := newTestStore(t)
	// Give a task a position and a tag so the round trip has something to lose.
	if _, err := s.UpdateTask(ctx, 5, TaskPatch{X: ptr(12.5), Y: ptr(88.0), Tags: ptr("ui, core")}); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	exp, err := s.ExportProject(ctx, p.ID)
	if err != nil {
		t.Fatalf("ExportProject: %v", err)
	}
	before := mustGraph(t, s, p.ID)
	readyBefore := before.ReadyIDs()

	if err := s.Import(ctx, p.ID, exp); err != nil {
		t.Fatalf("re-import: %v", err)
	}
	after := mustGraph(t, s, p.ID)
	if len(after.Tasks) != len(before.Tasks) {
		t.Fatalf("round trip changed task count: %d -> %d", len(before.Tasks), len(after.Tasks))
	}
	if len(after.Edges) != len(before.Edges) {
		t.Fatalf("round trip changed edge count: %d -> %d", len(before.Edges), len(after.Edges))
	}
	if !equalIDs(after.ReadyIDs(), readyBefore) {
		t.Errorf("round trip changed the frontier: %v -> %v", readyBefore, after.ReadyIDs())
	}
	// Position and tags survive.
	var x, y float64
	var tags string
	if err := s.DB().QueryRow(`SELECT x, y, tags FROM tasks WHERE project_id = ? AND key = 'FIX-5'`, p.ID).
		Scan(&x, &y, &tags); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if x != 12.5 || y != 88.0 || tags != "ui,core" {
		t.Errorf("round trip lost fields: x=%v y=%v tags=%q", x, y, tags)
	}
}

func countRows(t *testing.T, s *Store, table string, pid int64) int {
	t.Helper()
	var n int
	q := "SELECT COUNT(*) FROM " + table + " WHERE project_id = ?"
	if err := s.DB().QueryRow(q, pid).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func hasEdge(g *Graph, blocker, blocked int64) bool {
	for _, b := range g.Out[blocker] {
		if b == blocked {
			return true
		}
	}
	return false
}
