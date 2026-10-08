package store

import (
	"context"
	"database/sql"
	"sort"
)

// Graph is the in-memory view of one project's tasks and edges. Every algorithm
// in this file is a pure function over it. The whole project is loaded per
// request — a project is hundreds of nodes, not millions — which is why there
// is not a single recursive CTE anywhere in this package (SPEC §5).
type Graph struct {
	Tasks map[int64]*Task
	Out   map[int64][]int64 // blocker -> blocked
	In    map[int64][]int64 // blocked -> blocker
	Edges []*Edge
}

// LoadGraph reads the whole project graph in two queries and builds adjacency.
// O(V+E).
func (s *Store) LoadGraph(ctx context.Context, projectID int64) (*Graph, error) {
	tasks, err := s.loadTasks(ctx, projectID, true)
	if err != nil {
		return nil, err
	}
	g := &Graph{
		Tasks: make(map[int64]*Task, len(tasks)),
		Out:   make(map[int64][]int64),
		In:    make(map[int64][]int64),
	}
	for _, t := range tasks {
		g.Tasks[t.ID] = t
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, blocker_id, blocked_id, label
		FROM edges WHERE project_id = ? ORDER BY id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		e := &Edge{}
		if err := rows.Scan(&e.ID, &e.BlockerID, &e.BlockedID, &e.Label); err != nil {
			return nil, err
		}
		e.Satisfied = edgeSatisfied(g.Tasks, e)
		g.Edges = append(g.Edges, e)
		g.Out[e.BlockerID] = append(g.Out[e.BlockerID], e.BlockedID)
		g.In[e.BlockedID] = append(g.In[e.BlockedID], e.BlockerID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Deterministic adjacency: sort every slice by id so that traversal order —
	// and therefore every path this file returns — is stable.
	for _, adj := range []map[int64][]int64{g.Out, g.In} {
		for k := range adj {
			sortIDs(adj[k])
		}
	}
	return g, nil
}

func edgeSatisfied(tasks map[int64]*Task, e *Edge) bool {
	b, ok := tasks[e.BlockerID]
	if !ok {
		return false
	}
	return isTerminal(b.Status)
}

func sortIDs(xs []int64) {
	sort.Slice(xs, func(i, j int) bool { return xs[i] < xs[j] })
}

// LoadGraphForTask loads the graph of whichever project owns the task.
func (s *Store) LoadGraphForTask(ctx context.Context, taskID int64) (*Graph, error) {
	pid, err := s.projectIDForTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return s.LoadGraph(ctx, pid)
}

func (s *Store) projectIDForTask(ctx context.Context, taskID int64) (int64, error) {
	var pid int64
	err := s.db.QueryRowContext(ctx, `SELECT project_id FROM tasks WHERE id = ?`, taskID).Scan(&pid)
	if err == sql.ErrNoRows {
		return 0, errf(CodeNotFound, "task %d not found", taskID)
	}
	if err != nil {
		return 0, err
	}
	return pid, nil
}

// ReadySet is the ready-frontier (SPEC §2.3):
//
//	ready(t) ⟺ t.status = 'todo' AND every blocker of t is terminal
//
// Archived tasks are excluded not by a special case but because archival sets
// status = 'cancelled', which is terminal (SPEC §2.5). Note that `doing` is NOT
// ready: ready means startable, not in flight.
func (g *Graph) ReadySet() map[int64]bool {
	ready := make(map[int64]bool)
	for id, t := range g.Tasks {
		if t.Archived || t.Status != StatusTodo {
			continue
		}
		ok := true
		for _, b := range g.In[id] {
			bt, exists := g.Tasks[b]
			if !exists || !isTerminal(bt.Status) {
				ok = false
				break
			}
		}
		if ok {
			ready[id] = true
		}
	}
	return ready
}

// ReadyIDs is ReadySet as a sorted slice. Nothing that returns a list may
// depend on Go's map iteration order (SPEC §5.6).
func (g *Graph) ReadyIDs() []int64 {
	return sortedKeys(g.ReadySet())
}

// Unblocks is the honest leverage metric (SPEC §2.4): how many tasks would
// become ready right now if t were closed, holding everything else fixed. It is
// deliberately NOT transitive — a task two hops downstream stays blocked
// because its intermediate blocker is still open. That is the number you can
// act on.
//
// t itself is never counted: forcing it to done removes it from the "after" set.
func (g *Graph) Unblocks(t *Task) int {
	if t == nil || isTerminal(t.Status) {
		return 0
	}
	before := g.ReadySet()
	saved := t.Status
	t.Status = StatusDone // mutate the in-memory copy, never the DB
	after := g.ReadySet()
	t.Status = saved
	n := 0
	for id := range after {
		if !before[id] {
			n++
		}
	}
	return n
}

// BlastRadius is the number of distinct transitive dependents of t, regardless
// of current readiness. Display only — it is never used for ranking (SPEC §2.4).
func (g *Graph) BlastRadius(t *Task) int {
	if t == nil {
		return 0
	}
	seen := map[int64]bool{t.ID: true}
	queue := []int64{t.ID}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range g.Out[cur] {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return len(seen) - 1 // do not count t
}

// WouldCycle reports whether adding blocker → blocked would close a cycle, and
// if so returns the full closed path (first element repeated at the end), e.g.
// [3 4 7 3]. Adding blocker → blocked closes a cycle iff blocked can already
// reach blocker (SPEC §5.5).
//
// The path goes straight into the API error body and the MCP error text so a
// model that gets it can fix its own plan without asking.
func (g *Graph) WouldCycle(blocker, blocked int64) ([]int64, bool) {
	if blocker == blocked {
		return []int64{blocker, blocker}, true
	}
	path, ok := g.path(blocked, blocker)
	if !ok {
		return nil, false
	}
	// Close the loop back to the node the new edge leaves from, so the returned
	// path is a genuinely closed cycle: blocked -> ... -> blocker -> blocked.
	// SPEC §5.5's pseudocode says `append(path, blocker)` but its own worked
	// example — and the §11.5 assertion — is [3,4,7,3], which is this.
	return append(path, blocked), true
}

// path is a BFS from `from` to `to` over Out edges, returning the node sequence
// inclusive of both endpoints. BFS over sorted adjacency keeps it deterministic.
func (g *Graph) path(from, to int64) ([]int64, bool) {
	if from == to {
		return []int64{from}, true
	}
	prev := map[int64]int64{}
	seen := map[int64]bool{from: true}
	queue := []int64{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range g.Out[cur] {
			if seen[next] {
				continue
			}
			seen[next] = true
			prev[next] = cur
			if next == to {
				return reconstruct(prev, from, to), true
			}
			queue = append(queue, next)
		}
	}
	return nil, false
}

func reconstruct(prev map[int64]int64, from, to int64) []int64 {
	var rev []int64
	for cur := to; cur != from; cur = prev[cur] {
		rev = append(rev, cur)
	}
	rev = append(rev, from)
	out := make([]int64, 0, len(rev))
	for i := len(rev) - 1; i >= 0; i-- {
		out = append(out, rev[i])
	}
	return out
}

// Derive populates every server-derived field on every task in the graph:
// ready, blocked_by, blocked_by_open, unblocks, blast_radius and inputs. Call it
// once per request before serialising.
func (g *Graph) Derive() {
	ready := g.ReadySet()
	ids := g.SortedTaskIDs()
	for _, id := range ids {
		t := g.Tasks[id]
		t.Ready = ready[id]
		blockers := append([]int64(nil), g.In[id]...)
		sortIDs(blockers)
		if blockers == nil {
			blockers = []int64{}
		}
		t.BlockedBy = blockers
		open := []int64{}
		for _, b := range blockers {
			if bt, ok := g.Tasks[b]; ok && !isTerminal(bt.Status) {
				open = append(open, b)
			}
		}
		t.BlockedByOpen = open
		t.Inputs = g.Inputs(t)
		t.Unblocks = g.Unblocks(t)
		t.BlastRadius = g.BlastRadius(t)
	}
}

// Inputs returns the outputs of t's blockers that have finished, ordered by
// blocker id. It is a pure derivation from the edges that already exist — there
// is no stored input, and no second relationship between producer and consumer
// (docs/task-outputs.md §2).
//
// Three deliberate rules:
//
//   - Only blockers in a terminal status. An input is material the consumer can
//     actually use, and an open blocker has not produced anything yet. Its
//     *promise* is already modelled by the edge — it is exactly what
//     blocked_by_open reports — so listing it here too would say the same thing
//     twice, and would fill the UI with entries reading "no output recorded" for
//     work nobody has started. When a task is ready every blocker is terminal,
//     so a ready task's inputs are the complete handoff.
//   - Archived blockers contribute nothing. An archived task is cancelled, so it
//     stops feeding its dependents exactly as it stops blocking them — no
//     special case, same as ReadySet.
//   - Direct blockers only, not the transitive closure. Same call as Unblocks,
//     and for the same reason: two hops down, the intermediate task's output is
//     where the synthesis belongs (docs/task-outputs.md §4.1).
//
// An empty output on a finished blocker is still reported. "Closed without
// recording findings" is a real state and the reader should see it.
func (g *Graph) Inputs(t *Task) []Input {
	out := []Input{}
	if t == nil {
		return out
	}
	blockers := append([]int64(nil), g.In[t.ID]...)
	sortIDs(blockers)
	for _, b := range blockers {
		bt, ok := g.Tasks[b]
		if !ok || bt.Archived || !isTerminal(bt.Status) {
			continue
		}
		out = append(out, Input{
			ID:     bt.ID,
			Key:    bt.Key,
			Label:  bt.Label,
			Status: bt.Status,
			Output: bt.Output,
		})
	}
	return out
}

// SortedTaskIDs returns every task id in the graph ascending.
func (g *Graph) SortedTaskIDs() []int64 {
	ids := make([]int64, 0, len(g.Tasks))
	for id := range g.Tasks {
		ids = append(ids, id)
	}
	sortIDs(ids)
	return ids
}

// Frontier returns the ready tasks in frontier order:
// unblocks DESC, priority ASC, id ASC (SPEC §2.4). The id tiebreak makes the
// ordering total and deterministic — do not omit it.
func (g *Graph) Frontier() []*Task {
	ready := g.ReadySet()
	out := make([]*Task, 0, len(ready))
	for id := range ready {
		t := g.Tasks[id]
		t.Unblocks = g.Unblocks(t)
		t.BlastRadius = g.BlastRadius(t)
		out = append(out, t)
	}
	SortFrontier(out)
	return out
}

// SortFrontier applies the canonical frontier ordering in place.
func SortFrontier(ts []*Task) {
	sort.Slice(ts, func(i, j int) bool {
		a, b := ts[i], ts[j]
		if a.Unblocks != b.Unblocks {
			return a.Unblocks > b.Unblocks
		}
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		return a.ID < b.ID
	})
}

func sortedKeys(m map[int64]bool) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortIDs(out)
	return out
}
