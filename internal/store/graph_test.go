package store

import (
	"context"
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
