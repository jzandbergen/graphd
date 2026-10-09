package store

import "context"

// GraphView returns the renderable graph for a project: the JSON shape with
// every derived field populated. When includeArchived is false, archived tasks
// and any edge touching them are omitted from the output — but the underlying
// algorithm still sees them, because an archived blocker is cancelled and
// therefore terminal (SPEC §2.5).
func (s *Store) GraphView(ctx context.Context, projectID int64, includeArchived bool) (*GraphPayload, error) {
	project, err := s.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	g, err := s.LoadGraph(ctx, projectID)
	if err != nil {
		return nil, err
	}
	g.Derive()
	rev, err := s.Revision(ctx)
	if err != nil {
		return nil, err
	}

	out := &GraphPayload{Project: *project, Revision: rev, Tasks: []*Task{}, Edges: []*Edge{}}
	for _, id := range g.SortedTaskIDs() {
		t := g.Tasks[id]
		if t.Archived && !includeArchived {
			continue
		}
		out.Tasks = append(out.Tasks, t)
	}
	for _, e := range g.Edges {
		if !includeArchived {
			if b, ok := g.Tasks[e.BlockerID]; ok && b.Archived {
				continue
			}
			if d, ok := g.Tasks[e.BlockedID]; ok && d.Archived {
				continue
			}
		}
		out.Edges = append(out.Edges, e)
	}
	return out, nil
}

// GetReady returns the ranked frontier for a project: ready tasks ordered by
// unblocks DESC, priority ASC, id ASC (SPEC §2.4). limit <= 0 means no limit.
//
// Each entry carries its derived inputs (the outputs of the task's blockers).
// There is deliberately no flag to suppress them: the frontier is where a caller
// learns what it can start, so it is also where it should learn what it would be
// starting with. Keeping that text out of a payload is a *presentation* decision
// and belongs at the boundary that cares — the MCP tools cap it
// (docs/task-outputs.md §4.2). The store returns what is in the column, whole.
func (s *Store) GetReady(ctx context.Context, projectID int64, limit int) (*Ready, error) {
	if _, err := s.GetProject(ctx, projectID); err != nil {
		return nil, err
	}
	g, err := s.LoadGraph(ctx, projectID)
	if err != nil {
		return nil, err
	}
	rev, err := s.Revision(ctx)
	if err != nil {
		return nil, err
	}
	frontier := g.Frontier()
	out := &Ready{ProjectID: projectID, Revision: rev, Ready: []ReadyEntry{}}
	for _, t := range frontier {
		if limit > 0 && len(out.Ready) >= limit {
			break
		}
		out.Ready = append(out.Ready, ReadyEntry{
			ID:            t.ID,
			Key:           t.Key,
			Label:         t.Label,
			Priority:      t.Priority,
			Owner:         t.Owner,
			Unblocks:      t.Unblocks,
			BlastRadius:   t.BlastRadius,
			BlockedByOpen: append([]int64{}, t.BlockedByOpen...),
			Inputs:        g.Inputs(t),
		})
	}
	return out, nil
}

// TaskView returns one task with every derived field populated — ready,
// blocked_by, blocked_by_open, unblocks, blast_radius and inputs. GetTask alone
// returns the raw row, which is what mutation paths want; a read path that
// promises derived fields must go through here.
func (s *Store) TaskView(ctx context.Context, id int64) (*Task, error) {
	// LoadGraphForTask resolves the owning project, so it also reports a task
	// that does not exist — no separate existence check needed.
	g, err := s.LoadGraphForTask(ctx, id)
	if err != nil {
		return nil, err
	}
	g.Derive()
	if t, ok := g.Tasks[id]; ok {
		return t, nil
	}
	return nil, errf(CodeNotFound, "task %d not found", id)
}

// NextTask is the payload of get_next_task (SPEC §8.2).
type NextTask struct {
	Next       *ReadyEntry  `json:"next"`
	Reason     string       `json:"reason"`
	InProgress []InProgress `json:"in_progress"`
	// AwaitingHuman lists ready tasks owned by the human. It is the third
	// bucket alongside Next and InProgress: work that is startable now but is
	// not the agent's to do (docs/task-owners.md §3).
	//
	// It exists because silently dropping human tasks would make `next: null`
	// read as "the project is finished" when it is only waiting on a person.
	// Always present — an empty list reports `[]`, never a missing field — for
	// the same reason `inputs` is.
	AwaitingHuman []ReadyEntry `json:"awaiting_human"`
}

// InProgress is one claimed (doing) task.
type InProgress struct {
	ID     int64  `json:"id"`
	Key    string `json:"key"`
	Label  string `json:"label"`
	Status string `json:"status"`
	// Owner is carried here too, because a human-owned task that is `doing` is a
	// real state — an agent handed a task over, or a person picked one up — and
	// it appears in no other bucket. `awaiting_human` is drawn from the frontier,
	// which requires `todo`, so without this field such a task would be listed
	// with no indication that it is the human's (docs/task-owners.md §3.1).
	Owner string `json:"owner"`
}

// GetNextTask returns the top of the agent frontier plus the tasks currently in
// flight and the ready tasks owned by the human.
//
// This is the one place the owner field changes what a caller is offered. The
// frontier itself (GetReady, the canvas, the board) stays unfiltered: a human
// task is ready, and hiding it from the human's own queue would defeat the
// point. Only "what should *I* — an agent — do next" partitions, and it
// partitions by *reporting*, not dropping: the human work comes back under
// awaiting_human so an agent can hand it to its user instead of attempting it
// (docs/task-owners.md §3).
//
// reason is "ok", "no_ready_tasks", or "awaiting_human" — the last when there
// is no agent work left but the project is not finished, only waiting.
func (s *Store) GetNextTask(ctx context.Context, projectID int64) (*NextTask, error) {
	// Load the whole frontier once: both the agent top and the human bucket are
	// slices of the same ranked list, so partitioning here costs one pass and
	// cannot disagree with itself.
	ready, err := s.GetReady(ctx, projectID, 0)
	if err != nil {
		return nil, err
	}
	g, err := s.LoadGraph(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := &NextTask{
		Reason:        "no_ready_tasks",
		InProgress:    []InProgress{},
		AwaitingHuman: []ReadyEntry{},
	}
	for _, e := range ready.Ready {
		if e.Owner == OwnerHuman {
			out.AwaitingHuman = append(out.AwaitingHuman, e)
			continue
		}
		if out.Next == nil {
			next := e
			out.Next = &next
			out.Reason = "ok"
		}
	}
	// Nothing for an agent, but the human has work: the project is not
	// finished, it is waiting. Saying "no_ready_tasks" here would be a lie of
	// omission, and this tool exists so an agent can tell the difference.
	if out.Next == nil && len(out.AwaitingHuman) > 0 {
		out.Reason = "awaiting_human"
	}
	for _, id := range g.SortedTaskIDs() {
		t := g.Tasks[id]
		if t.Archived || t.Status != StatusDoing {
			continue
		}
		out.InProgress = append(out.InProgress, InProgress{
			ID: t.ID, Key: t.Key, Label: t.Label, Status: t.Status, Owner: t.Owner,
		})
	}
	return out, nil
}
