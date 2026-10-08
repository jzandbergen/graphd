package api

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"

	"graphd/internal/store"
)

// ---- UI shells ----

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/canvas", http.StatusFound)
}

func (s *Server) handleCanvas(w http.ResponseWriter, r *http.Request) {
	s.serveShell(w, r, "canvas")
}

func (s *Server) handleBoard(w http.ResponseWriter, r *http.Request) {
	s.serveShell(w, r, "board")
}

// ---- Projects ----

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.store.ListProjects(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"projects": projects})
}

type createProjectReq struct {
	Name      string `json:"name"`
	KeyPrefix string `json:"key_prefix"`
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var req createProjectReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	p, err := s.store.CreateProject(r.Context(), req.Name, req.KeyPrefix)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, p)
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	pid, err := pathID(r, "pid")
	if err != nil {
		writeError(w, r, err)
		return
	}
	p, err := s.store.GetProject(r.Context(), pid)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, p)
}

func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	pid, err := pathID(r, "pid")
	if err != nil {
		writeError(w, r, err)
		return
	}
	if err := s.store.DeleteProject(r.Context(), pid, r.URL.Query().Get("confirm")); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- Graph and frontier ----

func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	pid, err := s.projectID(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	include := r.URL.Query().Get("include_archived") == "1"
	g, err := s.store.GraphView(r.Context(), pid, include)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, g)
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	pid, err := s.projectID(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		limit, _ = strconv.Atoi(v)
	}
	// The frontier is where a caller learns what it can start, so it also sees
	// what it would be starting *with*. This endpoint returns the inputs whole;
	// the MCP tools apply their own size cap at their boundary.
	ready, err := s.store.GetReady(r.Context(), pid, limit)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, ready)
}

// ---- Export / import ----

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	pid, err := s.projectID(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	exp, err := s.store.ExportProject(r.Context(), pid)
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition",
		`attachment; filename="`+exp.Project.Name+`.graphd.json"`)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(exp)
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	pid, err := s.projectID(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeError(w, r, &store.Error{Code: store.CodeInvalidInput, Message: "could not read body"})
		return
	}
	exp, err := store.ImportJSON(body)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if err := s.store.Import(r.Context(), pid, exp); err != nil {
		writeError(w, r, err)
		return
	}
	g, err := s.store.GraphView(r.Context(), pid, false)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, g)
}

// ---- Tasks ----

type createTaskReq struct {
	Label    string `json:"label"`
	Notes    string `json:"notes"`
	Output   string `json:"output"`
	Status   string `json:"status"`
	Priority *int   `json:"priority"`
	Tags     string `json:"tags"`
	Key      string `json:"key"`
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	pid, err := s.projectID(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	var req createTaskReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	prio := 0
	if req.Priority != nil {
		prio = *req.Priority
	}
	t, err := s.store.CreateTask(r.Context(), pid, store.NewTask{
		Label: req.Label, Notes: req.Notes, Output: req.Output, Status: req.Status,
		Priority: prio, Tags: req.Tags, Key: req.Key,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	// Re-read with derived fields so the created task carries the same shape as
	// every other task the client sees.
	view, err := s.store.TaskView(r.Context(), t.ID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, view)
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	tid, err := pathID(r, "tid")
	if err != nil {
		writeError(w, r, err)
		return
	}
	t, err := s.store.TaskView(r.Context(), tid)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, t)
}

type patchTaskReq struct {
	Label    *string  `json:"label"`
	Notes    *string  `json:"notes"`
	Output   *string  `json:"output"`
	Status   *string  `json:"status"`
	Priority *int     `json:"priority"`
	Tags     *string  `json:"tags"`
	X        *float64 `json:"x"`
	Y        *float64 `json:"y"`
}

func (s *Server) handlePatchTask(w http.ResponseWriter, r *http.Request) {
	tid, err := pathID(r, "tid")
	if err != nil {
		writeError(w, r, err)
		return
	}
	var req patchTaskReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	t, err := s.store.UpdateTask(r.Context(), tid, store.TaskPatch{
		Label: req.Label, Notes: req.Notes, Output: req.Output, Status: req.Status,
		Priority: req.Priority, Tags: req.Tags, X: req.X, Y: req.Y,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	view, err := s.store.TaskView(r.Context(), t.ID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, view)
}

func (s *Server) handleArchiveTask(w http.ResponseWriter, r *http.Request) {
	tid, err := pathID(r, "tid")
	if err != nil {
		writeError(w, r, err)
		return
	}
	if _, err := s.store.ArchiveTask(r.Context(), tid); err != nil {
		writeError(w, r, err)
		return
	}
	view, err := s.store.TaskView(r.Context(), tid)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, view)
}

func (s *Server) handleRestoreTask(w http.ResponseWriter, r *http.Request) {
	tid, err := pathID(r, "tid")
	if err != nil {
		writeError(w, r, err)
		return
	}
	if _, err := s.store.RestoreTask(r.Context(), tid); err != nil {
		writeError(w, r, err)
		return
	}
	view, err := s.store.TaskView(r.Context(), tid)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, view)
}

// ---- Edges ----

type createEdgeReq struct {
	BlockerID int64  `json:"blocker_id"`
	BlockedID int64  `json:"blocked_id"`
	Label     string `json:"label"`
}

func (s *Server) handleCreateEdge(w http.ResponseWriter, r *http.Request) {
	pid, err := s.projectID(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	var req createEdgeReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	e, err := s.store.AddEdge(r.Context(), pid, req.BlockerID, req.BlockedID, req.Label)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, e)
}

func (s *Server) handleDeleteEdge(w http.ResponseWriter, r *http.Request) {
	eid, err := pathID(r, "eid")
	if err != nil {
		writeError(w, r, err)
		return
	}
	if err := s.store.RemoveEdgeByID(r.Context(), eid); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type patchEdgeReq struct {
	Label *string `json:"label"`
}

func (s *Server) handlePatchEdge(w http.ResponseWriter, r *http.Request) {
	eid, err := pathID(r, "eid")
	if err != nil {
		writeError(w, r, err)
		return
	}
	var req patchEdgeReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	label := ""
	if req.Label != nil {
		label = *req.Label
	}
	e, err := s.store.UpdateEdgeLabel(r.Context(), eid, label)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, e)
}

// ---- Positions ----

func (s *Server) handlePositions(w http.ResponseWriter, r *http.Request) {
	pid, err := s.projectID(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	var req []store.Position
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if err := s.store.SetPositions(r.Context(), pid, req); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRelayout returns the last saved positions. The server never computes
// layout (SPEC §7.3) — this is a server-side copy of what the browser saved.
func (s *Server) handleRelayout(w http.ResponseWriter, r *http.Request) {
	pid, err := s.projectID(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	pos, err := s.store.Positions(r.Context(), pid)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"positions": pos})
}

// ---- Board fragment ----

// boardColumn is one kanban column.
type boardColumn struct {
	Status string
	Title  string
	Tasks  []*store.Task
}

// handleBoardFragment renders the four columns as server-side HTML. The client
// swaps this wholesale on every SSE revision change; drag-and-drop is a few
// lines of HTML5 drag events (SPEC §7.4).
func (s *Server) handleBoardFragment(w http.ResponseWriter, r *http.Request) {
	pid, err := s.projectID(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	include := r.URL.Query().Get("include_archived") == "1"
	g, err := s.store.GraphView(r.Context(), pid, include)
	if err != nil {
		writeError(w, r, err)
		return
	}
	cols := buildColumns(g.Tasks)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := boardTmpl.Execute(w, cols); err != nil {
		writeError(w, r, err)
	}
}

// buildColumns groups tasks into the four fixed columns, each sorted ready
// first, then unblocks DESC, priority ASC, id ASC — the same ordering as the
// frontier, so the top of the Todo column is the answer to "what next".
func buildColumns(tasks []*store.Task) []boardColumn {
	titles := map[string]string{
		store.StatusTodo: "Todo", store.StatusDoing: "Doing",
		store.StatusDone: "Done", store.StatusCancelled: "Cancelled",
	}
	order := []string{store.StatusTodo, store.StatusDoing, store.StatusDone, store.StatusCancelled}
	cols := make([]boardColumn, 0, 4)
	for _, st := range order {
		cols = append(cols, boardColumn{Status: st, Title: titles[st], Tasks: []*store.Task{}})
	}
	idx := map[string]int{}
	for i, c := range cols {
		idx[c.Status] = i
	}
	for _, t := range tasks {
		i, ok := idx[t.Status]
		if !ok {
			continue
		}
		cols[i].Tasks = append(cols[i].Tasks, t)
	}
	for i := range cols {
		sort.SliceStable(cols[i].Tasks, func(a, b int) bool {
			x, y := cols[i].Tasks[a], cols[i].Tasks[b]
			if x.Ready != y.Ready {
				return x.Ready
			}
			if x.Unblocks != y.Unblocks {
				return x.Unblocks > y.Unblocks
			}
			if x.Priority != y.Priority {
				return x.Priority < y.Priority
			}
			return x.ID < y.ID
		})
	}
	return cols
}
