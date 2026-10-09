package store

// The API/MCP-facing data shapes. JSON field names are exactly those in
// SPEC.md §6.1. Derived fields are computed server-side and never by the
// client.

// Project is a project row.
type Project struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	KeyPrefix string `json:"key_prefix"`
	CreatedAt string `json:"created_at"`
	// TaskCount is only populated by list_projects (MCP §8.2).
	TaskCount int `json:"task_count,omitempty"`
}

// Task is a task row plus every server-derived field.
type Task struct {
	ID       int64  `json:"id"`
	Key      string `json:"key"`
	Label    string `json:"label"`
	Notes    string `json:"notes"`
	Output   string `json:"output"` // what this task produced; see docs/task-outputs.md
	Status   string `json:"status"`
	Priority int    `json:"priority"`
	Tags     string `json:"tags"`
	// Owner is who is expected to do the work: "agent" or "human". Authored,
	// never derived — see docs/task-owners.md. It does not affect `ready`,
	// `unblocks` or the frontier ordering.
	Owner    string   `json:"owner"`
	X        *float64 `json:"x"` // null when unplaced
	Y        *float64 `json:"y"`
	Archived bool     `json:"archived"`

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`

	// ---- server-derived, client never computes these ----
	Ready         bool    `json:"ready"`
	BlockedBy     []int64 `json:"blocked_by"`      // all blockers, any status
	BlockedByOpen []int64 `json:"blocked_by_open"` // blockers that are not terminal
	Unblocks      int     `json:"unblocks"`
	BlastRadius   int     `json:"blast_radius"`
	// Inputs are the outputs of this task's immediate blockers, derived at read
	// time and never stored. Empty when the task has no blockers.
	Inputs []Input `json:"inputs"`
}

// Input is one blocker's output, as seen by a dependent task. It is derived,
// not stored: the edge that already exists is the dependency, and copying the
// producer's text into the consumer would rot the moment the producer is
// revised — the same failure mode as storing `blocked` (docs/task-outputs.md §2).
//
// Truncated is only ever true where a caller asked for a bounded payload (the
// MCP frontier tools). The store always returns the whole text.
type Input struct {
	ID        int64  `json:"id"`
	Key       string `json:"key"`
	Label     string `json:"label"`
	Status    string `json:"status"`
	Output    string `json:"output"`
	Truncated bool   `json:"truncated,omitempty"`
}

// Edge is a blocking edge. Direction is the direction work flows and the
// direction the arrow is drawn: blocker_id blocks blocked_id.
type Edge struct {
	ID        int64  `json:"id"`
	BlockerID int64  `json:"blocker_id"`
	BlockedID int64  `json:"blocked_id"`
	Label     string `json:"label"`
	Satisfied bool   `json:"satisfied"`
}

// GraphPayload is the full renderable state of one project, as serialised to
// the canvas. It is distinct from the algorithm-side Graph in graph.go: this
// one is a flat JSON view, that one is adjacency maps.
type GraphPayload struct {
	Project  Project `json:"project"`
	Revision int64   `json:"revision"`
	Tasks    []*Task `json:"tasks"`
	Edges    []*Edge `json:"edges"`
}

// ReadyEntry is one row of the ranked frontier.
type ReadyEntry struct {
	ID       int64  `json:"id"`
	Key      string `json:"key"`
	Label    string `json:"label"`
	Priority int    `json:"priority"`
	// Owner travels with a frontier entry so a consumer can tell at a glance
	// whether the entry is its own work. The frontier itself is unfiltered: a
	// human task is ready, it is simply not offered to an agent by
	// get_next_task (docs/task-owners.md §3).
	Owner         string  `json:"owner"`
	Unblocks      int     `json:"unblocks"`
	BlastRadius   int     `json:"blast_radius"`
	BlockedByOpen []int64 `json:"blocked_by_open"`
	// Inputs carries the outputs this task consumes, so "what do I do next" and
	// "with what" arrive in the same answer. Always present — an empty frontier
	// entry reports `[]`, never a missing field — so a client never has to
	// distinguish "no inputs" from "this shape does not carry inputs".
	Inputs []Input `json:"inputs"`
}

// Ready is the payload of GET /ready and the get_ready tool.
type Ready struct {
	ProjectID int64        `json:"project_id"`
	Revision  int64        `json:"revision"`
	Ready     []ReadyEntry `json:"ready"`
}
