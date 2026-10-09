package mcp

import (
	"context"
	"fmt"
	"os"

	"graphd/internal/store"
)

// envProject is the GRAPHD_PROJECT fallback for tools that take `project`
// (SPEC §8.3). Read from the environment rather than baked in at start so a
// long-lived MCP process picks up changes; it is cheap.
func envProject() string { return os.Getenv("GRAPHD_PROJECT") }

// toolScaffoldPlan creates a whole plan transactionally (SPEC §8.2). This is
// the tool that matters most for agents: a plan in one call instead of thirty
// round-trips.
//
// tasks[].ref is a local string; edges[].blocker/blocked reference those refs
// and are resolved to real ids inside the transaction. If any edge would create
// a cycle the whole call rolls back and the error names the refs involved.
func toolScaffoldPlan(ctx context.Context, st *store.Store, args map[string]any) (any, error) {
	p, err := resolveProject(ctx, st, args)
	if err != nil {
		return nil, err
	}

	rawTasks, ok := args["tasks"]
	if !ok {
		return nil, &store.Error{Code: store.CodeInvalidInput, Message: "scaffold_plan: `tasks` is required"}
	}
	tasks, err := decodeScaffoldTasks(rawTasks)
	if err != nil {
		return nil, err
	}
	edges, err := decodeScaffoldEdges(args["edges"])
	if err != nil {
		return nil, err
	}

	res, err := st.ScaffoldPlan(ctx, p.ID, tasks, edges)
	if err != nil {
		return nil, err
	}

	// Summary: the created tasks (with derived fields) and the edge set.
	g, err := st.LoadGraph(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	created := map[int64]bool{}
	for _, t := range res.Tasks {
		created[t.ID] = true
	}
	outTasks := make([]mcpTask, 0, len(res.Tasks))
	for _, id := range g.SortedTaskIDs() {
		if !created[id] {
			continue
		}
		outTasks = append(outTasks, toMCPTask(g.Tasks[id]))
	}
	outEdges := make([]map[string]any, 0, len(res.Edges))
	for _, e := range res.Edges {
		outEdges = append(outEdges, map[string]any{
			"id":         e.ID,
			"blocker_id": e.BlockerID,
			"blocked_id": e.BlockedID,
			"label":      e.Label,
			"satisfied":  e.Satisfied,
		})
	}
	return map[string]any{
		"project_id": p.ID,
		"created":    len(outTasks),
		"edges":      len(outEdges),
		"tasks":      outTasks,
		"edge_list":  outEdges,
	}, nil
}

func decodeScaffoldTasks(raw any) ([]store.ScaffoldTask, error) {
	list, ok := raw.([]any)
	if !ok {
		return nil, &store.Error{Code: store.CodeInvalidInput, Message: "scaffold_plan: `tasks` must be an array"}
	}
	if len(list) == 0 {
		return nil, &store.Error{Code: store.CodeInvalidInput, Message: "scaffold_plan: `tasks` is empty"}
	}
	out := make([]store.ScaffoldTask, 0, len(list))
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, &store.Error{Code: store.CodeInvalidInput,
				Message: fmt.Sprintf("scaffold_plan: tasks[%d] is not an object", i)}
		}
		st := store.ScaffoldTask{}
		st.Ref, _ = m["ref"].(string)
		st.Label, _ = m["label"].(string)
		st.Notes, _ = m["notes"].(string)
		st.Output, _ = m["output"].(string)
		st.Status, _ = m["status"].(string)
		st.Tags, _ = m["tags"].(string)
		st.Owner, _ = m["owner"].(string)
		if v, ok := m["priority"].(float64); ok {
			st.Priority = int(v)
		}
		if st.Label == "" {
			return nil, &store.Error{Code: store.CodeInvalidInput,
				Message: fmt.Sprintf("scaffold_plan: tasks[%d] has no label", i)}
		}
		out = append(out, st)
	}
	return out, nil
}

func decodeScaffoldEdges(raw any) ([]store.ScaffoldEdge, error) {
	if raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, &store.Error{Code: store.CodeInvalidInput, Message: "scaffold_plan: `edges` must be an array"}
	}
	out := make([]store.ScaffoldEdge, 0, len(list))
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, &store.Error{Code: store.CodeInvalidInput,
				Message: fmt.Sprintf("scaffold_plan: edges[%d] is not an object", i)}
		}
		e := store.ScaffoldEdge{}
		e.Blocker, _ = m["blocker"].(string)
		e.Blocked, _ = m["blocked"].(string)
		e.Label, _ = m["label"].(string)
		if e.Blocker == "" || e.Blocked == "" {
			return nil, &store.Error{Code: store.CodeInvalidInput,
				Message: fmt.Sprintf("scaffold_plan: edges[%d] needs both blocker and blocked refs", i)}
		}
		out = append(out, e)
	}
	return out, nil
}
