package mcp

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"graphd/internal/store"
)

// Exactly thirteen tools. Do not invent more; do not split tools "for clarity".
// A small, sharp surface is the point (SPEC §8.2).

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// toolHandler is one tool implementation.
type toolHandler func(ctx context.Context, st *store.Store, args map[string]any) (any, error)

// buildContractGuidance is attached to the `notes` field of create_task,
// update_task and scaffold_plan. Field descriptions are part of the tool schema
// and are re-sent to the model on every call, so this guidance reaches the
// model whether or not the harness loads any skill file — which is the whole
// point of putting it here rather than in a document.
//
// It is deliberately a *shape*, not a procedure: it says what a finished note
// contains. Multi-step method belongs in a prompt, not a field description.
const buildContractGuidance = "Markdown. For anything non-trivial, write a build contract rather than a one-liner, " +
	"so a worker can execute the task without re-reading any parent document. Use these headings, " +
	"omitting only the ones that genuinely do not apply: " +
	"**Problem** (what must change and why — name the files, types and functions, never line numbers); " +
	"**Action Items** (specific, independently completable steps); " +
	"**Interfaces** (the concrete signatures, endpoint shapes, CLI flags or config keys this touches, named exactly); " +
	"**Pseudocode** (control flow for anything not obvious from the interfaces; skip for a single-path edit); " +
	"**Validation contract** (the command or observable outcome that proves it done — a named test, not \"tests pass\"); " +
	"**Non-goals** (the adjacent work this explicitly does not do, and where it lands instead); " +
	"**References** (related task keys). " +
	"Markdown is rendered in the UI. Keep the text self-contained: do not cite external ticket ids inline."

// outputGuidance is attached to the `output` field. It exists to draw the line
// between the two text fields, because the distinction is the whole point of the
// feature and is not obvious from the names alone: notes is the instruction,
// output is the result.
//
// It also tells the model to write the output when it finishes, which is the
// half of the contract that would otherwise be forgotten — an output nobody
// writes is not a handoff.
const outputGuidance = "Markdown. What this task PRODUCED or FOUND — the result, as opposed to `notes`, " +
	"which is what the task was asked to do. Write it when you finish the task: the tasks that depend on this " +
	"one receive it as their `inputs` (see get_next_task), so it is the handoff. Record decisions, findings, " +
	"measured numbers and resulting file/type/function names — whatever the next worker needs and cannot " +
	"rediscover cheaply. Do not restate the task; do not paste the diff. Leave empty only if the task genuinely " +
	"produced nothing a dependent would need."

// toolDefs is the ordered tool catalogue; toolList() derives tools/list from it.
var toolDefs = []tool{
	{
		Name:        "list_projects",
		Description: "List every project with its task count.",
		InputSchema: obj(nil),
	},
	{
		Name:        "create_project",
		Description: "Create a project. key_prefix is uppercase, 1-8 chars, first char a letter (e.g. RATE).",
		InputSchema: obj(map[string]any{
			"name":       strProp("project name, unique"),
			"key_prefix": strProp("task key prefix, e.g. RATE"),
		}, "name", "key_prefix"),
	},
	{
		Name: "get_graph",
		Description: "Return the project's tasks and edges with derived fields. " +
			"Positions are deliberately omitted: agents never read or write x/y.",
		InputSchema: obj(map[string]any{
			"project":          strProp("project id, name or key_prefix"),
			"include_archived": boolProp("include archived (soft-deleted) tasks"),
		}, "project"),
	},
	{
		Name: "get_ready",
		Description: "The ready-frontier: todo tasks whose blockers are all terminal, ranked by unblocks DESC, priority ASC, id ASC. " +
			"Each entry carries `inputs`: the outputs of the tasks it depends on, so you have what you need to start.",
		InputSchema: obj(map[string]any{
			"project": strProp("project id, name or key_prefix"),
			"limit":   intProp("maximum number of entries to return"),
		}, "project"),
	},
	{
		Name: "get_next_task",
		Description: "The single best task to start now, with the outputs of its blockers in `next.inputs` — the handoff from " +
			"whatever it was waiting on — plus the tasks currently in progress. Read next.inputs before starting.",
		InputSchema: obj(map[string]any{"project": strProp("project id, name or key_prefix")}, "project"),
	},
	{
		Name:        "create_task",
		Description: "Create a task. status defaults to todo, priority to 3 (1 is highest).",
		InputSchema: obj(map[string]any{
			"project":  strProp("project id, name or key_prefix"),
			"label":    strProp("task label"),
			"notes":    strProp(buildContractGuidance),
			"output":   strProp(outputGuidance),
			"status":   enumProp("todo", "doing", "done", "cancelled"),
			"priority": intProp("1 (highest) to 5 (lowest)"),
			"tags":     strProp("comma-separated tags"),
		}, "project", "label"),
	},
	{
		Name:        "update_task",
		Description: "Update a task's label, notes, output, status, priority or tags. Omitted fields are left unchanged.",
		InputSchema: obj(map[string]any{
			"task":     strProp("task id or key, e.g. RATE-7"),
			"label":    strProp("new label"),
			"notes":    strProp(buildContractGuidance),
			"output":   strProp(outputGuidance),
			"status":   enumProp("todo", "doing", "done", "cancelled"),
			"priority": intProp("1 (highest) to 5 (lowest)"),
			"tags":     strProp("comma-separated tags"),
		}, "task"),
	},
	{
		Name:        "archive_task",
		Description: "Soft-delete a task: sets archived=1 and status=cancelled. It stops blocking whatever it blocked.",
		InputSchema: obj(map[string]any{"task": strProp("task id or key")}, "task"),
	},
	{
		Name:        "restore_task",
		Description: "Restore an archived task: sets archived=0 and status=todo.",
		InputSchema: obj(map[string]any{"task": strProp("task id or key")}, "task"),
	},
	{
		Name: "add_edge",
		Description: "Add a blocking edge: blocker cannot be started until... rather, blocker blocks blocked. " +
			"Cycles, self-edges and duplicates are rejected with the offending path.",
		InputSchema: obj(map[string]any{
			"project": strProp("project id, name or key_prefix"),
			"blocker": strProp("task id or key that must finish first"),
			"blocked": strProp("task id or key that cannot start until the blocker is terminal"),
			"label":   strProp("optional decorative label"),
		}, "project", "blocker", "blocked"),
	},
	{
		Name:        "remove_edge",
		Description: "Remove the edge between two tasks. Returns {removed:true}.",
		InputSchema: obj(map[string]any{
			"project": strProp("project id, name or key_prefix"),
			"blocker": strProp("blocker task id or key"),
			"blocked": strProp("blocked task id or key"),
		}, "project", "blocker", "blocked"),
	},
	{
		Name: "scaffold_plan",
		Description: "Create a whole plan — tasks and edges — in one transaction. " +
			"Tasks may carry a local ref; edges reference those refs. All of it lands or none of it does.",
		InputSchema: obj(map[string]any{
			"project": strProp("project id, name or key_prefix"),
			"tasks": map[string]any{
				"type":        "array",
				"description": "tasks to create",
				"items": obj(map[string]any{
					"ref":      strProp("local reference used by edges in this same call"),
					"label":    strProp("task label"),
					"notes":    strProp(buildContractGuidance),
					"output":   strProp(outputGuidance),
					"status":   enumProp("todo", "doing", "done", "cancelled"),
					"priority": intProp("1 (highest) to 5 (lowest)"),
					"tags":     strProp("comma-separated tags"),
				}, "label"),
			},
			"edges": map[string]any{
				"type":        "array",
				"description": "edges between the refs above",
				"items": obj(map[string]any{
					"blocker": strProp("ref of the blocking task"),
					"blocked": strProp("ref of the blocked task"),
					"label":   strProp("optional decorative label"),
				}, "blocker", "blocked"),
			},
		}, "project", "tasks"),
	},
	{
		Name:        "export_json",
		Description: "Return the project's full lossless export (tasks, edges, positions, archived flags).",
		InputSchema: obj(map[string]any{"project": strProp("project id, name or key_prefix")}, "project"),
	},
}

// toolList returns the catalogue for tools/list.
func toolList() []tool { return toolDefs }

// toolHandlers maps each tool name to its implementation. It must stay in sync
// with toolDefs; a test asserts the two have identical key sets.
var toolHandlers = map[string]toolHandler{
	"list_projects":  toolListProjects,
	"create_project": toolCreateProject,
	"get_graph":      toolGetGraph,
	"get_ready":      toolGetReady,
	"get_next_task":  toolGetNextTask,
	"create_task":    toolCreateTask,
	"update_task":    toolUpdateTask,
	"archive_task":   toolArchiveTask,
	"restore_task":   toolRestoreTask,
	"add_edge":       toolAddEdge,
	"remove_edge":    toolRemoveEdge,
	"scaffold_plan":  toolScaffoldPlan,
	"export_json":    toolExportJSON,
}

// ---- schema helpers ----

func obj(props map[string]any, required ...string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	m := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func boolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func enumProp(vals ...string) map[string]any {
	return map[string]any{"type": "string", "enum": vals}
}

// ---- argument helpers ----

func argString(args map[string]any, key string) (string, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func argInt(args map[string]any, key string) (int, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}

func argBool(args map[string]any, key string) (bool, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

// resolveProject applies the project-resolution order (SPEC §8.3): the
// `project` argument, else GRAPHD_PROJECT, else an error listing the projects.
// It never infers from the working directory — this tool is explicitly not
// repo-bound.
func resolveProject(ctx context.Context, st *store.Store, args map[string]any) (*store.Project, error) {
	ref, _ := argString(args, "project")
	if ref == "" {
		ref = envProject()
	}
	if ref == "" {
		return nil, noProjectError(ctx, st)
	}
	p, err := st.ResolveProject(ctx, ref)
	if err != nil {
		return nil, noProjectError(ctx, st)
	}
	return p, nil
}

func noProjectError(ctx context.Context, st *store.Store) error {
	projects, _ := st.ListProjects(ctx)
	msg := "no project specified: pass `project`, or set GRAPHD_PROJECT"
	if len(projects) > 0 {
		msg += "\navailable projects:"
		for _, p := range projects {
			msg += fmt.Sprintf("\n  - %s (id %d, prefix %s)", p.Name, p.ID, p.KeyPrefix)
		}
	} else {
		msg += "\nno projects exist yet; create one with create_project"
	}
	return &store.Error{Code: store.CodeNotFound, Message: msg}
}

// resolveTaskArg resolves a task reference from an argument, converting a
// resolution failure into the spec's not_found code.
func resolveTaskArg(ctx context.Context, st *store.Store, args map[string]any, key string) (*store.Task, error) {
	ref, ok := argString(args, key)
	if !ok || ref == "" {
		return nil, &store.Error{Code: store.CodeInvalidInput, Message: "missing required argument: " + key}
	}
	return st.ResolveTask(ctx, ref)
}

// ---- implementations ----

func toolListProjects(ctx context.Context, st *store.Store, args map[string]any) (any, error) {
	projects, err := st.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(projects))
	for _, p := range projects {
		out = append(out, map[string]any{
			"id":         p.ID,
			"name":       p.Name,
			"key_prefix": p.KeyPrefix,
			"task_count": p.TaskCount,
		})
	}
	return out, nil
}

func toolCreateProject(ctx context.Context, st *store.Store, args map[string]any) (any, error) {
	name, _ := argString(args, "name")
	prefix, _ := argString(args, "key_prefix")
	p, err := st.CreateProject(ctx, name, prefix)
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": p.ID, "name": p.Name, "key_prefix": p.KeyPrefix}, nil
}

// mcpTask is a task as exposed to agents: no x/y (SPEC §8.2).
//
// It carries the task's own `output` in full but deliberately not its derived
// `inputs`. inputs are a *join* — a blocker's output repeated once per
// dependent — and get_graph already returns every output and every edge, so
// inlining them here would duplicate text for no information. The two tools
// where the join is the point, get_ready and get_next_task, return a shape that
// does carry inputs (see readyEntry).
type mcpTask struct {
	ID            int64   `json:"id"`
	Key           string  `json:"key"`
	Label         string  `json:"label"`
	Notes         string  `json:"notes,omitempty"`
	Output        string  `json:"output,omitempty"`
	Status        string  `json:"status"`
	Priority      int     `json:"priority"`
	Tags          string  `json:"tags,omitempty"`
	Archived      bool    `json:"archived"`
	Ready         bool    `json:"ready"`
	BlockedBy     []int64 `json:"blocked_by"`
	BlockedByOpen []int64 `json:"blocked_by_open"`
	Unblocks      int     `json:"unblocks"`
	BlastRadius   int     `json:"blast_radius"`
}

// Input is one blocker's output as seen by a dependent task.
type Input struct {
	ID        int64  `json:"id"`
	Key       string `json:"key"`
	Label     string `json:"label"`
	Status    string `json:"status"`
	Output    string `json:"output"`
	Truncated bool   `json:"truncated,omitempty"`
}

// readyEntry is one frontier row for agents: the ranked task plus the inputs it
// consumes, so "what do I do next" and "with what" arrive together
// (docs/task-outputs.md §5).
type readyEntry struct {
	ID            int64   `json:"id"`
	Key           string  `json:"key"`
	Label         string  `json:"label"`
	Priority      int     `json:"priority"`
	Unblocks      int     `json:"unblocks"`
	BlastRadius   int     `json:"blast_radius"`
	BlockedByOpen []int64 `json:"blocked_by_open"`
	Inputs        []Input `json:"inputs"`
}

func toReadyEntry(e store.ReadyEntry) readyEntry {
	return readyEntry{
		ID: e.ID, Key: e.Key, Label: e.Label, Priority: e.Priority,
		Unblocks: e.Unblocks, BlastRadius: e.BlastRadius,
		BlockedByOpen: e.BlockedByOpen, Inputs: capInputs(e.Inputs),
	}
}

// readyView is the frontier payload for agents.
type readyView struct {
	ProjectID int64        `json:"project_id"`
	Revision  int64        `json:"revision"`
	Ready     []readyEntry `json:"ready"`
}

// nextTaskView is the get_next_task payload for agents (SPEC §8.2).
type nextTaskView struct {
	Next       *readyEntry        `json:"next"`
	Reason     string             `json:"reason"`
	InProgress []store.InProgress `json:"in_progress"`
}

// inputBudget caps how much of a single input the frontier tools inline.
//
// The frontier is meant to be cheap and an analysis is not, so get_ready and
// get_next_task bound what they return rather than dumping every upstream
// document into a call that runs on a loop. The full text is always one
// get_graph (or one task read) away, and truncated:true says so. Truncation is
// applied here, at the tool boundary, and never in the store — GET /ready and
// the MCP tool do not have to agree on a number, and the store's job is to
// return what is actually in the column.
const inputBudget = 512

// capInputs copies inputs, truncating each output to the budget.
func capInputs(in []store.Input) []Input {
	out := make([]Input, 0, len(in))
	for _, x := range in {
		out = append(out, Input{
			ID: x.ID, Key: x.Key, Label: x.Label, Status: x.Status,
			Output:    truncateOutput(x.Output, inputBudget),
			Truncated: len(x.Output) > inputBudget,
		})
	}
	return out
}

// truncateOutput cuts s to at most budget bytes, preferring to end on the last
// line boundary within the budget so a list item or a pseudocode line is not
// severed mid-token. When no line break is available it cuts on a rune boundary,
// so the result is always valid UTF-8.
func truncateOutput(s string, budget int) string {
	if len(s) <= budget {
		return s
	}
	cut := s[:budget]
	if i := strings.LastIndexByte(cut, '\n'); i > budget/2 {
		return cut[:i+1]
	}
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

func toMCPTask(t *store.Task) mcpTask {
	return mcpTask{
		ID: t.ID, Key: t.Key, Label: t.Label, Notes: t.Notes, Output: t.Output,
		Status: t.Status, Priority: t.Priority, Tags: t.Tags, Archived: t.Archived,
		Ready: t.Ready, BlockedBy: t.BlockedBy, BlockedByOpen: t.BlockedByOpen,
		Unblocks: t.Unblocks, BlastRadius: t.BlastRadius,
	}
}

func toolGetGraph(ctx context.Context, st *store.Store, args map[string]any) (any, error) {
	p, err := resolveProject(ctx, st, args)
	if err != nil {
		return nil, err
	}
	include, _ := argBool(args, "include_archived")
	g, err := st.GraphView(ctx, p.ID, include)
	if err != nil {
		return nil, err
	}
	tasks := make([]mcpTask, 0, len(g.Tasks))
	for _, t := range g.Tasks {
		tasks = append(tasks, toMCPTask(t))
	}
	edges := make([]map[string]any, 0, len(g.Edges))
	for _, e := range g.Edges {
		edges = append(edges, map[string]any{
			"blocker_id": e.BlockerID,
			"blocked_id": e.BlockedID,
			"label":      e.Label,
			"satisfied":  e.Satisfied,
		})
	}
	return map[string]any{
		"project":  map[string]any{"id": p.ID, "name": p.Name, "key_prefix": p.KeyPrefix},
		"revision": g.Revision,
		"tasks":    tasks,
		"edges":    edges,
	}, nil
}

func toolGetReady(ctx context.Context, st *store.Store, args map[string]any) (any, error) {
	p, err := resolveProject(ctx, st, args)
	if err != nil {
		return nil, err
	}
	limit, _ := argInt(args, "limit")
	ready, err := st.GetReady(ctx, p.ID, limit)
	if err != nil {
		return nil, err
	}
	out := &readyView{ProjectID: ready.ProjectID, Revision: ready.Revision,
		Ready: make([]readyEntry, 0, len(ready.Ready))}
	for _, e := range ready.Ready {
		out.Ready = append(out.Ready, toReadyEntry(e))
	}
	return out, nil
}

func toolGetNextTask(ctx context.Context, st *store.Store, args map[string]any) (any, error) {
	p, err := resolveProject(ctx, st, args)
	if err != nil {
		return nil, err
	}
	nt, err := st.GetNextTask(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	out := &nextTaskView{Reason: nt.Reason, InProgress: nt.InProgress}
	if out.InProgress == nil {
		out.InProgress = []store.InProgress{}
	}
	if nt.Next != nil {
		e := toReadyEntry(*nt.Next)
		out.Next = &e
	}
	return out, nil
}

func toolCreateTask(ctx context.Context, st *store.Store, args map[string]any) (any, error) {
	p, err := resolveProject(ctx, st, args)
	if err != nil {
		return nil, err
	}
	label, _ := argString(args, "label")
	notes, _ := argString(args, "notes")
	output, _ := argString(args, "output")
	status, _ := argString(args, "status")
	tags, _ := argString(args, "tags")
	prio, _ := argInt(args, "priority")
	t, err := st.CreateTask(ctx, p.ID, store.NewTask{
		Label: label, Notes: notes, Output: output, Status: status, Priority: prio, Tags: tags,
	})
	if err != nil {
		return nil, err
	}
	view, err := st.TaskView(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	return toMCPTask(view), nil
}

func toolUpdateTask(ctx context.Context, st *store.Store, args map[string]any) (any, error) {
	t, err := resolveTaskArg(ctx, st, args, "task")
	if err != nil {
		return nil, err
	}
	var patch store.TaskPatch
	if v, ok := argString(args, "label"); ok {
		patch.Label = &v
	}
	if v, ok := argString(args, "notes"); ok {
		patch.Notes = &v
	}
	if v, ok := argString(args, "output"); ok {
		patch.Output = &v
	}
	if v, ok := argString(args, "status"); ok {
		patch.Status = &v
	}
	if v, ok := argInt(args, "priority"); ok {
		patch.Priority = &v
	}
	if v, ok := argString(args, "tags"); ok {
		patch.Tags = &v
	}
	updated, err := st.UpdateTask(ctx, t.ID, patch)
	if err != nil {
		return nil, err
	}
	view, err := st.TaskView(ctx, updated.ID)
	if err != nil {
		return nil, err
	}
	return toMCPTask(view), nil
}

func toolArchiveTask(ctx context.Context, st *store.Store, args map[string]any) (any, error) {
	t, err := resolveTaskArg(ctx, st, args, "task")
	if err != nil {
		return nil, err
	}
	updated, err := st.ArchiveTask(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	return toMCPTask(updated), nil
}

func toolRestoreTask(ctx context.Context, st *store.Store, args map[string]any) (any, error) {
	t, err := resolveTaskArg(ctx, st, args, "task")
	if err != nil {
		return nil, err
	}
	updated, err := st.RestoreTask(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	return toMCPTask(updated), nil
}

func toolAddEdge(ctx context.Context, st *store.Store, args map[string]any) (any, error) {
	p, err := resolveProject(ctx, st, args)
	if err != nil {
		return nil, err
	}
	blocker, err := resolveTaskArg(ctx, st, args, "blocker")
	if err != nil {
		return nil, err
	}
	blocked, err := resolveTaskArg(ctx, st, args, "blocked")
	if err != nil {
		return nil, err
	}
	label, _ := argString(args, "label")
	e, err := st.AddEdge(ctx, p.ID, blocker.ID, blocked.ID, label)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"id":         e.ID,
		"blocker_id": e.BlockerID,
		"blocked_id": e.BlockedID,
		"label":      e.Label,
		"satisfied":  e.Satisfied,
	}, nil
}

func toolRemoveEdge(ctx context.Context, st *store.Store, args map[string]any) (any, error) {
	p, err := resolveProject(ctx, st, args)
	if err != nil {
		return nil, err
	}
	blocker, err := resolveTaskArg(ctx, st, args, "blocker")
	if err != nil {
		return nil, err
	}
	blocked, err := resolveTaskArg(ctx, st, args, "blocked")
	if err != nil {
		return nil, err
	}
	if err := st.RemoveEdge(ctx, p.ID, blocker.ID, blocked.ID); err != nil {
		return nil, err
	}
	return map[string]any{"removed": true}, nil
}

func toolExportJSON(ctx context.Context, st *store.Store, args map[string]any) (any, error) {
	p, err := resolveProject(ctx, st, args)
	if err != nil {
		return nil, err
	}
	return st.ExportProject(ctx, p.ID)
}
