package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// Export is the lossless JSON payload (SPEC §9.3). Tasks and edges are keyed by
// their human key rather than integer id so an export is portable between
// databases. Positions and archived flags are included.
type Export struct {
	SchemaVersion int           `json:"schema_version"`
	ExportedAt    string        `json:"exported_at"`
	Project       ExportProject `json:"project"`
	Tasks         []ExportTask  `json:"tasks"`
	Edges         []ExportEdge  `json:"edges"`
}

// ExportProject is the project header inside an export.
type ExportProject struct {
	Name      string `json:"name"`
	KeyPrefix string `json:"key_prefix"`
	CreatedAt string `json:"created_at,omitempty"`
}

// ExportTask is one task inside an export.
type ExportTask struct {
	Key       string   `json:"key"`
	Label     string   `json:"label"`
	Notes     string   `json:"notes"`
	Output    string   `json:"output"`
	Status    string   `json:"status"`
	Priority  int      `json:"priority"`
	Tags      string   `json:"tags"`
	X         *float64 `json:"x"`
	Y         *float64 `json:"y"`
	Archived  bool     `json:"archived"`
	CreatedAt string   `json:"created_at,omitempty"`
	UpdatedAt string   `json:"updated_at,omitempty"`
}

// ExportEdge is one edge inside an export, endpoints named by task key.
type ExportEdge struct {
	Blocker string `json:"blocker"`
	Blocked string `json:"blocked"`
	Label   string `json:"label"`
}

// ExportProject builds the lossless export payload for a project.
func (s *Store) ExportProject(ctx context.Context, projectID int64) (*Export, error) {
	project, err := s.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	tasks, err := s.loadTasks(ctx, projectID, true)
	if err != nil {
		return nil, err
	}
	g, err := s.LoadGraph(ctx, projectID)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]string, len(tasks))
	exp := &Export{
		SchemaVersion: SchemaVersion,
		ExportedAt:    nowUTC(),
		Project: ExportProject{
			Name:      project.Name,
			KeyPrefix: project.KeyPrefix,
			CreatedAt: project.CreatedAt,
		},
		Tasks: []ExportTask{},
		Edges: []ExportEdge{},
	}
	for _, t := range tasks {
		byID[t.ID] = t.Key
		exp.Tasks = append(exp.Tasks, ExportTask{
			Key: t.Key, Label: t.Label, Notes: t.Notes, Output: t.Output, Status: t.Status,
			Priority: t.Priority, Tags: t.Tags, X: t.X, Y: t.Y, Archived: t.Archived,
			CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
		})
	}
	for _, e := range g.Edges {
		exp.Edges = append(exp.Edges, ExportEdge{
			Blocker: byID[e.BlockerID],
			Blocked: byID[e.BlockedID],
			Label:   e.Label,
		})
	}
	return exp, nil
}

// ImportJSON parses and validates an export payload without touching the DB.
func ImportJSON(data []byte) (*Export, error) {
	var exp Export
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&exp); err != nil {
		return nil, errf(CodeInvalidInput, "import payload is not valid JSON: %v", err)
	}
	return &exp, nil
}

// Import replaces the contents of the project with the payload, transactionally
// (SPEC §9.3). It never merges. Validation happens inside the transaction so a
// failure rolls the whole thing back and reports the first failure with its
// code.
//
// Order of validation: duplicate keys, edge endpoints exist, then cycles.
func (s *Store) Import(ctx context.Context, projectID int64, exp *Export) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := s.getProjectTx(ctx, tx, projectID); err != nil {
			return err
		}
		// Wipe existing contents. Edges first so the FK graph stays clean even
		// though the cascade would handle it.
		if _, err := tx.ExecContext(ctx, `DELETE FROM edges WHERE project_id = ?`, projectID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM tasks WHERE project_id = ?`, projectID); err != nil {
			return err
		}

		seen := map[string]bool{}
		ids := map[string]int64{}
		now := nowUTC()
		for _, et := range exp.Tasks {
			if et.Key == "" {
				return errf(CodeInvalidInput, "import: task with empty key")
			}
			if seen[et.Key] {
				return errf(CodeDuplicateKey, "import: duplicate task key %q", et.Key)
			}
			seen[et.Key] = true
			if !ValidStatus(et.Status) {
				return errf(CodeInvalidStatus, "import: task %s has invalid status %q", et.Key, et.Status)
			}
			if et.Priority < 1 || et.Priority > 5 {
				return errf(CodeInvalidPriority, "import: task %s priority %d outside 1..5", et.Key, et.Priority)
			}
			archived := 0
			if et.Archived {
				archived = 1
			}
			created := et.CreatedAt
			if created == "" {
				created = now
			}
			updated := et.UpdatedAt
			if updated == "" {
				updated = created
			}
			res, err := tx.ExecContext(ctx, `
				INSERT INTO tasks(project_id, key, label, notes, output, status, priority, tags,
				                  x, y, archived, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				projectID, et.Key, et.Label, et.Notes, et.Output, et.Status, et.Priority, normalizeTags(et.Tags),
				et.X, et.Y, archived, created, updated)
			if err != nil {
				return err
			}
			id, err := res.LastInsertId()
			if err != nil {
				return err
			}
			ids[et.Key] = id
		}

		for _, ee := range exp.Edges {
			bid, ok := ids[ee.Blocker]
			if !ok {
				return errf(CodeNotFound, "import: edge blocker %q does not exist", ee.Blocker)
			}
			did, ok := ids[ee.Blocked]
			if !ok {
				return errf(CodeNotFound, "import: edge blocked %q does not exist", ee.Blocked)
			}
			if bid == did {
				return errf(CodeSelfEdge, "import: self-edge on %q", ee.Blocker)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO edges(project_id, blocker_id, blocked_id, label, created_at)
				VALUES (?, ?, ?, ?, ?)`, projectID, bid, did, ee.Label, now); err != nil {
				if isUniqueViolation(err) {
					return errf(CodeDuplicateEdge, "import: duplicate edge %s -> %s", ee.Blocker, ee.Blocked)
				}
				return err
			}
		}

		// Cycle check over the freshly written graph; any cycle rolls it all back.
		g, err := s.loadGraphTx(ctx, tx, projectID)
		if err != nil {
			return err
		}
		if cycle, ok := g.findCycle(); ok {
			return &Error{
				Code:    CodeCycleDetected,
				Message: "import: graph contains a cycle: " + g.cycleString(cycle),
				Cycle:   cycle,
			}
		}

		// Keep the key counter past every imported key so future keys do not
		// collide.
		maxN := int64(0)
		for k := range ids {
			if n, ok := keyNumber(k); ok && n > maxN {
				maxN = n
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE projects SET next_task_number = ? WHERE id = ?`, maxN+1, projectID); err != nil {
			return err
		}
		return bumpRevision(ctx, tx)
	})
}

func (s *Store) getProjectTx(ctx context.Context, tx *sql.Tx, id int64) (*Project, error) {
	p := &Project{}
	err := tx.QueryRowContext(ctx,
		`SELECT id, name, key_prefix, created_at FROM projects WHERE id = ?`, id).
		Scan(&p.ID, &p.Name, &p.KeyPrefix, &p.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, errf(CodeNotFound, "project %d not found", id)
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// findCycle detects a cycle in the graph using iterative DFS with colouring and
// returns a closed path. Used only by import, where a whole graph arrives at
// once; interactive writes use WouldCycle instead.
func (g *Graph) findCycle() ([]int64, bool) {
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := map[int64]int{}
	parent := map[int64]int64{}
	ids := g.SortedTaskIDs()

	var visit func(start int64) ([]int64, bool)
	visit = func(start int64) ([]int64, bool) {
		type frame struct {
			id int64
			ix int
		}
		stack := []frame{{start, 0}}
		color[start] = grey
		for len(stack) > 0 {
			f := &stack[len(stack)-1]
			out := g.Out[f.id]
			if f.ix < len(out) {
				next := out[f.ix]
				f.ix++
				switch color[next] {
				case grey:
					// Found a back edge: walk parents from f.id back to next.
					var rev []int64
					rev = append(rev, f.id)
					for cur := f.id; cur != next; {
						cur = parent[cur]
						rev = append(rev, cur)
					}
					path := make([]int64, 0, len(rev)+1)
					for i := len(rev) - 1; i >= 0; i-- {
						path = append(path, rev[i])
					}
					path = append(path, next)
					return path, true
				case white:
					color[next] = grey
					parent[next] = f.id
					stack = append(stack, frame{next, 0})
				}
			} else {
				color[f.id] = black
				stack = stack[:len(stack)-1]
			}
		}
		return nil, false
	}

	for _, id := range ids {
		if color[id] == white {
			if path, ok := visit(id); ok {
				return path, true
			}
		}
	}
	return nil, false
}

// keyNumber extracts the numeric suffix from a key like "RATE-7".
func keyNumber(key string) (int64, bool) {
	i := len(key)
	for i > 0 && key[i-1] >= '0' && key[i-1] <= '9' {
		i--
	}
	if i == len(key) {
		return 0, false
	}
	var n int64
	for _, r := range key[i:] {
		n = n*10 + int64(r-'0')
	}
	return n, true
}

// ScaffoldTask is one task in a scaffold_plan call.
type ScaffoldTask struct {
	Ref      string `json:"ref"`
	Label    string `json:"label"`
	Notes    string `json:"notes"`
	Output   string `json:"output"`
	Status   string `json:"status"`
	Priority int    `json:"priority"`
	Tags     string `json:"tags"`
}

// ScaffoldEdge references tasks by their local ref.
type ScaffoldEdge struct {
	Blocker string `json:"blocker"`
	Blocked string `json:"blocked"`
	Label   string `json:"label"`
}

// ScaffoldResult summarises a scaffold_plan call.
type ScaffoldResult struct {
	ProjectID int64   `json:"project_id"`
	Tasks     []*Task `json:"tasks"`
	Edges     []*Edge `json:"edges"`
}

// ScaffoldPlan creates a whole plan — tasks and edges — in one transaction.
// All of it lands or none of it does (SPEC §8.2). Task refs are resolved to
// real ids inside the transaction; if any edge would create a cycle the entire
// call rolls back and the error names the refs involved.
func (s *Store) ScaffoldPlan(ctx context.Context, projectID int64, tasks []ScaffoldTask, edges []ScaffoldEdge) (*ScaffoldResult, error) {
	var createdTaskIDs []int64
	var createdEdgeIDs []int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := s.getProjectTx(ctx, tx, projectID); err != nil {
			return err
		}
		byRef := map[string]int64{}
		refOf := map[int64]string{}
		now := nowUTC()
		for i, st := range tasks {
			if st.Label == "" {
				return errf(CodeInvalidInput, "scaffold_plan: task %d has an empty label", i)
			}
			status := st.Status
			if status == "" {
				status = StatusTodo
			}
			if !ValidStatus(status) {
				return errf(CodeInvalidStatus, "scaffold_plan: task %q has invalid status %q", st.Label, status)
			}
			priority := st.Priority
			if priority == 0 {
				priority = 3
			}
			if priority < 1 || priority > 5 {
				return errf(CodeInvalidPriority, "scaffold_plan: task %q priority %d outside 1..5", st.Label, priority)
			}
			key, err := nextKey(ctx, tx, projectID)
			if err != nil {
				return err
			}
			r, err := tx.ExecContext(ctx, `
				INSERT INTO tasks(project_id, key, label, notes, output, status, priority, tags,
				                  x, y, archived, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, 0, ?, ?)`,
				projectID, key, st.Label, st.Notes, st.Output, status, priority, normalizeTags(st.Tags), now, now)
			if err != nil {
				return err
			}
			id, err := r.LastInsertId()
			if err != nil {
				return err
			}
			if st.Ref != "" {
				if _, dup := byRef[st.Ref]; dup {
					return errf(CodeInvalidInput, "scaffold_plan: duplicate task ref %q", st.Ref)
				}
				byRef[st.Ref] = id
			}
			refOf[id] = st.Ref
			createdTaskIDs = append(createdTaskIDs, id)
		}

		g, err := s.loadGraphTx(ctx, tx, projectID)
		if err != nil {
			return err
		}
		// Sort edges so ref resolution is deterministic.
		for _, se := range edges {
			bid, ok := byRef[se.Blocker]
			if !ok {
				return errf(CodeNotFound, "scaffold_plan: edge blocker ref %q not found", se.Blocker)
			}
			did, ok := byRef[se.Blocked]
			if !ok {
				return errf(CodeNotFound, "scaffold_plan: edge blocked ref %q not found", se.Blocked)
			}
			if bid == did {
				return errf(CodeSelfEdge, "scaffold_plan: edge %q -> %q is a self-edge", se.Blocker, se.Blocked)
			}
			if cycle, ok := g.WouldCycle(bid, did); ok {
				return &Error{
					Code: CodeCycleDetected,
					Message: fmt.Sprintf("scaffold_plan: adding %s -> %s creates a cycle: %s",
						refLabel(refOf, g, bid), refLabel(refOf, g, did), refCycleString(refOf, g, cycle)),
					Cycle: cycle,
				}
			}
			er, err := tx.ExecContext(ctx, `
				INSERT INTO edges(project_id, blocker_id, blocked_id, label, created_at)
				VALUES (?, ?, ?, ?, ?)`, projectID, bid, did, se.Label, now)
			if err != nil {
				if isUniqueViolation(err) {
					return errf(CodeDuplicateEdge, "scaffold_plan: duplicate edge %s -> %s", se.Blocker, se.Blocked)
				}
				return err
			}
			if eid, err := er.LastInsertId(); err == nil {
				createdEdgeIDs = append(createdEdgeIDs, eid)
			}
			// Record it in the in-memory graph so later edges in the same call
			// see it when checking for cycles.
			g.Out[bid] = append(g.Out[bid], did)
			g.In[did] = append(g.In[did], bid)
			sortIDs(g.Out[bid])
			sortIDs(g.In[did])
		}
		return bumpRevision(ctx, tx)
	})
	if err != nil {
		return nil, err
	}
	// Re-read the created rows so the caller gets complete task and edge shapes.
	res := &ScaffoldResult{ProjectID: projectID, Tasks: []*Task{}, Edges: []*Edge{}}
	for _, id := range createdTaskIDs {
		t, err := s.GetTask(ctx, id)
		if err != nil {
			return nil, err
		}
		res.Tasks = append(res.Tasks, t)
	}
	for _, id := range createdEdgeIDs {
		e, err := s.GetEdge(ctx, id)
		if err != nil {
			return nil, err
		}
		res.Edges = append(res.Edges, e)
	}
	return res, nil
}

func refLabel(refOf map[int64]string, g *Graph, id int64) string {
	if r, ok := refOf[id]; ok && r != "" {
		return r
	}
	return g.name(id)
}

func refCycleString(refOf map[int64]string, g *Graph, path []int64) string {
	parts := make([]string, 0, len(path))
	for _, id := range path {
		parts = append(parts, refLabel(refOf, g, id))
	}
	return joinArrow(parts)
}

func joinArrow(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " -> "
		}
		out += p
	}
	return out
}
