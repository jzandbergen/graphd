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
			Unblocks:      t.Unblocks,
			BlastRadius:   t.BlastRadius,
			BlockedByOpen: append([]int64{}, t.BlockedByOpen...),
		})
	}
	return out, nil
}

// NextTask is the payload of get_next_task (SPEC §8.2).
type NextTask struct {
	Next       *ReadyEntry  `json:"next"`
	Reason     string       `json:"reason"`
	InProgress []InProgress `json:"in_progress"`
}

// InProgress is one claimed (doing) task.
type InProgress struct {
	ID     int64  `json:"id"`
	Key    string `json:"key"`
	Label  string `json:"label"`
	Status string `json:"status"`
}

// GetNextTask returns the top of the frontier plus the tasks currently in
// flight. reason is "ok" or "no_ready_tasks".
func (s *Store) GetNextTask(ctx context.Context, projectID int64) (*NextTask, error) {
	ready, err := s.GetReady(ctx, projectID, 1)
	if err != nil {
		return nil, err
	}
	g, err := s.LoadGraph(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := &NextTask{Reason: "no_ready_tasks", InProgress: []InProgress{}}
	if len(ready.Ready) > 0 {
		next := ready.Ready[0]
		out.Next = &next
		out.Reason = "ok"
	}
	for _, id := range g.SortedTaskIDs() {
		t := g.Tasks[id]
		if t.Archived || t.Status != StatusDoing {
			continue
		}
		out.InProgress = append(out.InProgress, InProgress{
			ID: t.ID, Key: t.Key, Label: t.Label, Status: t.Status,
		})
	}
	return out, nil
}
