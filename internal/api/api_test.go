package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"graphd/internal/store"
)

func newTestServer(t *testing.T) (*Server, *store.Store, *store.Project) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	p, err := st.SeedFixture(context.Background())
	if err != nil {
		t.Fatalf("SeedFixture: %v", err)
	}
	return New(st), st, p
}

// do performs a request against the server's handler.
func do(t *testing.T, s *Server, method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	var decoded map[string]any
	if rec.Body.Len() > 0 && strings.Contains(rec.Header().Get("Content-Type"), "json") {
		_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
	}
	return rec, decoded
}

// PATCH /api/tasks/{tid} sets output, and the value comes back on the task.
// The output is also what a dependent task receives as its inputs.
func TestAPIOutputPatch(t *testing.T) {
	s, _, p := newTestServer(t)

	// Task 3 blocks 4. Record an output on 3.
	rec, body := do(t, s, "PATCH", "/api/tasks/3", map[string]any{
		"output": "ANALYSIS: token bucket holds 100/s",
	})
	if rec.Code != 200 {
		t.Fatalf("PATCH output = %d (%v)", rec.Code, body)
	}
	if body["output"] != "ANALYSIS: token bucket holds 100/s" {
		t.Errorf("patched output = %v", body["output"])
	}
	// The patch response is a full task view, so derived fields are present.
	if _, ok := body["inputs"]; !ok {
		t.Errorf("PATCH response has no inputs field: %v", body)
	}

	// Close 3 so 4 becomes ready, then read 4's derived inputs off the API.
	if rec, _ := do(t, s, "PATCH", "/api/tasks/3", map[string]any{"status": "done"}); rec.Code != 200 {
		t.Fatalf("close 3 = %d", rec.Code)
	}
	rec, t4 := do(t, s, "GET", "/api/tasks/4", nil)
	if rec.Code != 200 {
		t.Fatalf("GET task 4 = %d", rec.Code)
	}
	inputs, ok := t4["inputs"].([]any)
	if !ok || len(inputs) != 1 {
		t.Fatalf("task 4 inputs = %v, want one entry", t4["inputs"])
	}
	in := inputs[0].(map[string]any)
	if in["key"] != "FIX-3" {
		t.Errorf("input key = %v, want FIX-3", in["key"])
	}
	if in["output"] != "ANALYSIS: token bucket holds 100/s" {
		t.Errorf("input output = %v", in["output"])
	}
	// The HTTP surface returns inputs whole; no truncation marker.
	if _, has := in["truncated"]; has {
		t.Errorf("HTTP inputs should not be truncated: %v", in)
	}

	// The graph carries output and inputs on every task too.
	rec, g := do(t, s, "GET", "/api/projects/"+itoa(p.ID)+"/graph", nil)
	if rec.Code != 200 {
		t.Fatalf("GET graph = %d", rec.Code)
	}
	tasks := g["tasks"].([]any)
	var sawOutput bool
	for _, raw := range tasks {
		tk := raw.(map[string]any)
		if tk["key"] == "FIX-3" && tk["output"] == "ANALYSIS: token bucket holds 100/s" {
			sawOutput = true
		}
	}
	if !sawOutput {
		t.Errorf("graph did not carry task 3's output")
	}

	// The frontier carries inputs as well.
	rec, ready := do(t, s, "GET", "/api/projects/"+itoa(p.ID)+"/ready", nil)
	if rec.Code != 200 {
		t.Fatalf("GET ready = %d", rec.Code)
	}
	var sawReadyInputs bool
	for _, raw := range ready["ready"].([]any) {
		e := raw.(map[string]any)
		if e["key"] == "FIX-4" {
			ins, _ := e["inputs"].([]any)
			if len(ins) == 1 {
				sawReadyInputs = true
			}
		}
	}
	if !sawReadyInputs {
		t.Errorf("frontier did not carry FIX-4's inputs: %v", ready["ready"])
	}
}

// A created task comes back with the same shape as a read one: derived fields
// present, output honoured.
func TestAPICreateTaskCarriesOutputAndDerivedFields(t *testing.T) {
	s, _, p := newTestServer(t)
	rec, body := do(t, s, "POST", "/api/projects/"+itoa(p.ID)+"/tasks", map[string]any{
		"label":  "a fresh task",
		"output": "recorded at creation",
	})
	if rec.Code != 201 {
		t.Fatalf("POST task = %d (%v)", rec.Code, body)
	}
	if body["output"] != "recorded at creation" {
		t.Errorf("created output = %v", body["output"])
	}
	for _, field := range []string{"inputs", "blocked_by", "blocked_by_open", "unblocks", "blast_radius", "ready"} {
		if _, ok := body[field]; !ok {
			t.Errorf("created task is missing derived field %q", field)
		}
	}
	if ins, ok := body["inputs"].([]any); !ok || len(ins) != 0 {
		t.Errorf("a task with no blockers should have empty inputs, got %v", body["inputs"])
	}
}

func errCode(t *testing.T, body map[string]any) string {
	t.Helper()
	e, ok := body["error"].(map[string]any)
	if !ok {
		return ""
	}
	c, _ := e["code"].(string)
	return c
}

// Every route gets a handler test (M2 done-when).

func TestRouteProjects(t *testing.T) {
	s, _, p := newTestServer(t)

	rec, body := do(t, s, "GET", "/api/projects", nil)
	if rec.Code != 200 {
		t.Fatalf("GET /api/projects = %d", rec.Code)
	}
	if _, ok := body["projects"].([]any); !ok {
		t.Fatalf("projects missing: %v", body)
	}

	rec, body = do(t, s, "POST", "/api/projects", map[string]any{"name": "other", "key_prefix": "OTH"})
	if rec.Code != 201 {
		t.Fatalf("POST /api/projects = %d (%v)", rec.Code, body)
	}

	rec, _ = do(t, s, "GET", fmt.Sprintf("/api/projects/%d", p.ID), nil)
	if rec.Code != 200 {
		t.Fatalf("GET project = %d", rec.Code)
	}

	rec, _ = do(t, s, "GET", "/api/projects/99999", nil)
	if rec.Code != 404 {
		t.Fatalf("GET missing project = %d, want 404", rec.Code)
	}

	// Delete requires ?confirm=<name>.
	rec, body = do(t, s, "DELETE", fmt.Sprintf("/api/projects/%d", p.ID), nil)
	if rec.Code != 400 || errCode(t, body) != store.CodeConfirmRequired {
		t.Fatalf("DELETE without confirm = %d %v, want 400 confirm_required", rec.Code, body)
	}
	rec, _ = do(t, s, "DELETE", fmt.Sprintf("/api/projects/%d?confirm=%s", p.ID, p.Name), nil)
	if rec.Code != 204 {
		t.Fatalf("DELETE with confirm = %d, want 204", rec.Code)
	}
}

func TestRouteProjectValidation(t *testing.T) {
	s, _, _ := newTestServer(t)

	rec, body := do(t, s, "POST", "/api/projects", map[string]any{"name": "", "key_prefix": "AB"})
	if rec.Code != 400 || errCode(t, body) != store.CodeNameRequired {
		t.Fatalf("empty name = %d %v", rec.Code, body)
	}
	rec, body = do(t, s, "POST", "/api/projects", map[string]any{"name": "x", "key_prefix": "1bad"})
	if rec.Code != 400 || errCode(t, body) != store.CodeInvalidPrefix {
		t.Fatalf("bad prefix = %d %v", rec.Code, body)
	}
	rec, body = do(t, s, "POST", "/api/projects", map[string]any{"name": "fixture", "key_prefix": "FIX"})
	if rec.Code != 409 || errCode(t, body) != store.CodeDuplicateProject {
		t.Fatalf("duplicate project = %d %v", rec.Code, body)
	}
}

func TestRouteGraphAndReady(t *testing.T) {
	s, _, p := newTestServer(t)

	rec, body := do(t, s, "GET", fmt.Sprintf("/api/projects/%d/graph", p.ID), nil)
	if rec.Code != 200 {
		t.Fatalf("graph = %d", rec.Code)
	}
	tasks, _ := body["tasks"].([]any)
	if len(tasks) != 20 {
		t.Fatalf("graph tasks = %d, want 20", len(tasks))
	}
	// Derived fields must be present on every task.
	first := tasks[0].(map[string]any)
	for _, k := range []string{"ready", "blocked_by", "blocked_by_open", "unblocks", "blast_radius"} {
		if _, ok := first[k]; !ok {
			t.Errorf("graph task missing derived field %q", k)
		}
	}

	rec, body = do(t, s, "GET", fmt.Sprintf("/api/projects/%d/ready", p.ID), nil)
	if rec.Code != 200 {
		t.Fatalf("ready = %d", rec.Code)
	}
	ready, _ := body["ready"].([]any)
	if len(ready) != 3 {
		t.Fatalf("ready = %d entries, want 3", len(ready))
	}
	top := ready[0].(map[string]any)
	if top["key"] != "FIX-3" || top["unblocks"].(float64) != 3 {
		t.Errorf("frontier top = %v, want FIX-3 with unblocks 3", top)
	}

	rec, body = do(t, s, "GET", fmt.Sprintf("/api/projects/%d/ready?limit=1", p.ID), nil)
	ready, _ = body["ready"].([]any)
	if len(ready) != 1 {
		t.Fatalf("ready?limit=1 = %d entries", len(ready))
	}
}

func TestRouteTasks(t *testing.T) {
	s, _, p := newTestServer(t)

	rec, body := do(t, s, "POST", fmt.Sprintf("/api/projects/%d/tasks", p.ID),
		map[string]any{"label": "A new task", "priority": 2})
	if rec.Code != 201 {
		t.Fatalf("create task = %d (%v)", rec.Code, body)
	}
	if body["key"] != "FIX-21" {
		t.Errorf("new task key = %v, want FIX-21", body["key"])
	}
	id := int64(body["id"].(float64))

	rec, _ = do(t, s, "GET", fmt.Sprintf("/api/tasks/%d", id), nil)
	if rec.Code != 200 {
		t.Fatalf("get task = %d", rec.Code)
	}

	rec, body = do(t, s, "PATCH", fmt.Sprintf("/api/tasks/%d", id), map[string]any{"status": "doing"})
	if rec.Code != 200 || body["status"] != "doing" {
		t.Fatalf("patch status = %d %v", rec.Code, body)
	}

	rec, body = do(t, s, "PATCH", fmt.Sprintf("/api/tasks/%d", id), map[string]any{"status": "nope"})
	if rec.Code != 400 || errCode(t, body) != store.CodeInvalidStatus {
		t.Fatalf("bad status = %d %v", rec.Code, body)
	}
	rec, body = do(t, s, "PATCH", fmt.Sprintf("/api/tasks/%d", id), map[string]any{"priority": 9})
	if rec.Code != 400 || errCode(t, body) != store.CodeInvalidPriority {
		t.Fatalf("bad priority = %d %v", rec.Code, body)
	}

	// Positions are accepted on PATCH (the drag-persist path).
	rec, body = do(t, s, "PATCH", fmt.Sprintf("/api/tasks/%d", id), map[string]any{"x": 12.5, "y": 34.5})
	if rec.Code != 200 || body["x"].(float64) != 12.5 {
		t.Fatalf("patch position = %d %v", rec.Code, body)
	}

	rec, body = do(t, s, "POST", fmt.Sprintf("/api/tasks/%d/archive", id), nil)
	if rec.Code != 200 || body["archived"] != true || body["status"] != "cancelled" {
		t.Fatalf("archive = %d %v", rec.Code, body)
	}
	rec, body = do(t, s, "POST", fmt.Sprintf("/api/tasks/%d/restore", id), nil)
	if rec.Code != 200 || body["archived"] != false || body["status"] != "todo" {
		t.Fatalf("restore = %d %v", rec.Code, body)
	}

	rec, _ = do(t, s, "GET", "/api/tasks/999999", nil)
	if rec.Code != 404 {
		t.Fatalf("get missing task = %d", rec.Code)
	}
}

func TestRouteEdges(t *testing.T) {
	s, st, p := newTestServer(t)

	// A valid new edge: 11 -> 18 (11 is ready, 18 has no other blockers).
	rec, body := do(t, s, "POST", fmt.Sprintf("/api/projects/%d/edges", p.ID),
		map[string]any{"blocker_id": 11, "blocked_id": 18})
	if rec.Code != 201 {
		t.Fatalf("create edge = %d (%v)", rec.Code, body)
	}
	eid := int64(body["id"].(float64))

	rec, body = do(t, s, "PATCH", fmt.Sprintf("/api/edges/%d", eid), map[string]any{"label": "docs first"})
	if rec.Code != 200 || body["label"] != "docs first" {
		t.Fatalf("patch edge = %d %v", rec.Code, body)
	}

	// Cycle: 7 -> 3.
	rec, body = do(t, s, "POST", fmt.Sprintf("/api/projects/%d/edges", p.ID),
		map[string]any{"blocker_id": 7, "blocked_id": 3})
	if rec.Code != 409 || errCode(t, body) != store.CodeCycleDetected {
		t.Fatalf("cycle edge = %d %v", rec.Code, body)
	}
	cycle := body["error"].(map[string]any)["cycle"].([]any)
	if len(cycle) != 4 {
		t.Errorf("cycle path = %v, want 4 nodes", cycle)
	}

	rec, body = do(t, s, "POST", fmt.Sprintf("/api/projects/%d/edges", p.ID),
		map[string]any{"blocker_id": 3, "blocked_id": 3})
	if rec.Code != 400 || errCode(t, body) != store.CodeSelfEdge {
		t.Fatalf("self edge = %d %v", rec.Code, body)
	}
	rec, body = do(t, s, "POST", fmt.Sprintf("/api/projects/%d/edges", p.ID),
		map[string]any{"blocker_id": 3, "blocked_id": 4})
	if rec.Code != 409 || errCode(t, body) != store.CodeDuplicateEdge {
		t.Fatalf("duplicate edge = %d %v", rec.Code, body)
	}

	// Cross-project: create a second project with a task, then try to join them.
	other, err := st.CreateProject(context.Background(), "other", "OTH")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	ot, err := st.CreateTask(context.Background(), other.ID, store.NewTask{Label: "foreign"})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	rec, body = do(t, s, "POST", fmt.Sprintf("/api/projects/%d/edges", p.ID),
		map[string]any{"blocker_id": 11, "blocked_id": ot.ID})
	if rec.Code != 400 || errCode(t, body) != store.CodeCrossProjectEdge {
		t.Fatalf("cross-project edge = %d %v", rec.Code, body)
	}

	rec, _ = do(t, s, "DELETE", fmt.Sprintf("/api/edges/%d", eid), nil)
	if rec.Code != 204 {
		t.Fatalf("delete edge = %d", rec.Code)
	}
	rec, _ = do(t, s, "DELETE", fmt.Sprintf("/api/edges/%d", eid), nil)
	if rec.Code != 404 {
		t.Fatalf("delete missing edge = %d", rec.Code)
	}
}

func TestRoutePositionsAndRelayout(t *testing.T) {
	s, _, p := newTestServer(t)

	rec, _ := do(t, s, "POST", fmt.Sprintf("/api/projects/%d/positions", p.ID), []map[string]any{
		{"id": 3, "x": 100.0, "y": 200.0},
		{"id": 4, "x": 300.0, "y": 400.0},
	})
	if rec.Code != 204 {
		t.Fatalf("positions = %d", rec.Code)
	}

	rec, body := do(t, s, "POST", fmt.Sprintf("/api/projects/%d/relayout", p.ID), nil)
	if rec.Code != 200 {
		t.Fatalf("relayout = %d", rec.Code)
	}
	pos, _ := body["positions"].([]any)
	if len(pos) != 2 {
		t.Fatalf("relayout positions = %d, want 2", len(pos))
	}
	// Ordered by id, so the first is task 3.
	first := pos[0].(map[string]any)
	if first["x"].(float64) != 100.0 || first["y"].(float64) != 200.0 {
		t.Errorf("relayout first = %v, want {100,200}", first)
	}
}

func TestRouteExportImport(t *testing.T) {
	s, _, p := newTestServer(t)

	rec, _ := do(t, s, "GET", fmt.Sprintf("/api/projects/%d/export", p.ID), nil)
	if rec.Code != 200 {
		t.Fatalf("export = %d", rec.Code)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("export Content-Disposition = %q, want attachment", cd)
	}
	var exp store.Export
	if err := json.Unmarshal(rec.Body.Bytes(), &exp); err != nil {
		t.Fatalf("export body: %v", err)
	}
	if len(exp.Tasks) != 20 || len(exp.Edges) != 24 {
		t.Fatalf("export = %d tasks / %d edges", len(exp.Tasks), len(exp.Edges))
	}

	rec, body := do(t, s, "POST", fmt.Sprintf("/api/projects/%d/import", p.ID), exp)
	if rec.Code != 200 {
		t.Fatalf("import = %d (%v)", rec.Code, body)
	}
	tasks, _ := body["tasks"].([]any)
	if len(tasks) != 20 {
		t.Fatalf("after import, graph has %d tasks", len(tasks))
	}
}

func TestRouteBoardFragment(t *testing.T) {
	s, _, p := newTestServer(t)

	rec, _ := do(t, s, "GET", fmt.Sprintf("/api/projects/%d/board", p.ID), nil)
	if rec.Code != 200 {
		t.Fatalf("board fragment = %d", rec.Code)
	}
	html := rec.Body.String()
	for _, want := range []string{"data-status=\"todo\"", "data-status=\"doing\"",
		"data-status=\"done\"", "data-status=\"cancelled\"", "FIX-3", "Token bucket middleware"} {
		if !strings.Contains(html, want) {
			t.Errorf("board fragment missing %q", want)
		}
	}
	// Todo column must come first and the top card must be FIX-3 (the frontier).
	todo := strings.Index(html, "data-status=\"todo\"")
	if todo < 0 || strings.Index(html, "FIX-3") < todo {
		t.Errorf("FIX-3 is not the first todo card")
	}
}

func TestShellRoutes(t *testing.T) {
	s, _, _ := newTestServer(t)

	rec, _ := do(t, s, "GET", "/", nil)
	if rec.Code != 302 {
		t.Fatalf("GET / = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/canvas" {
		t.Errorf("GET / Location = %q, want /canvas", loc)
	}
	for _, view := range []string{"canvas", "board"} {
		rec, _ := do(t, s, "GET", "/"+view, nil)
		if rec.Code != 200 {
			t.Fatalf("GET /%s = %d", view, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "graphd") {
			t.Errorf("GET /%s body does not look like the shell", view)
		}
	}
	rec, _ = do(t, s, "GET", "/assets/app.js", nil)
	if rec.Code != 200 {
		t.Fatalf("GET /assets/app.js = %d", rec.Code)
	}
	rec, _ = do(t, s, "GET", "/assets/vendor/cytoscape.min.js", nil)
	if rec.Code != 200 {
		t.Fatalf("GET vendored cytoscape = %d", rec.Code)
	}
}

// §11.12 — archival semantics through the API.
func TestArchivalSemanticsAPI(t *testing.T) {
	s, st, p := newTestServer(t)

	// 6 is blocked by 4 and 5; 4 is blocked by 3. Close 3 first so 4 is ready,
	// then archive 4 and confirm 6 loses one open blocker.
	rec, _ := do(t, s, "PATCH", "/api/tasks/3", map[string]any{"status": "done"})
	if rec.Code != 200 {
		t.Fatalf("close 3 = %d", rec.Code)
	}
	rec, _ = do(t, s, "POST", "/api/tasks/4/archive", nil)
	if rec.Code != 200 {
		t.Fatalf("archive 4 = %d", rec.Code)
	}
	g, err := st.LoadGraph(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("LoadGraph: %v", err)
	}
	// The edge 4 -> 6 survives archival on purpose (history survives), but task
	// 4 is now cancelled, which is terminal, so it no longer blocks 6. That is
	// the whole point of the design: no special case is needed anywhere.
	for _, b := range g.In[6] {
		if b == 4 {
			if st := g.Tasks[4].Status; st != "cancelled" {
				t.Errorf("task 4 still blocks 6: status = %q, want cancelled", st)
			}
		}
	}
	// Archived tasks are hidden by default.
	rec, body := do(t, s, "GET", fmt.Sprintf("/api/projects/%d/graph", p.ID), nil)
	for _, raw := range body["tasks"].([]any) {
		if int64(raw.(map[string]any)["id"].(float64)) == 4 {
			t.Errorf("archived task 4 present in default graph")
		}
	}
	rec, body = do(t, s, "GET", fmt.Sprintf("/api/projects/%d/graph?include_archived=1", p.ID), nil)
	found := false
	for _, raw := range body["tasks"].([]any) {
		if int64(raw.(map[string]any)["id"].(float64)) == 4 {
			found = true
		}
	}
	if !found {
		t.Errorf("archived task 4 missing with include_archived=1")
	}
}

// §11.7 — persistence across a restart. This one uses a real server process so
// that "restart" means restart.
func TestPersistenceAcrossRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	bin := buildBinary(t)
	dir := t.TempDir()
	db := filepath.Join(dir, "graphd.db")

	// Seed via the binary so the fixture ids match the spec.
	seedFixture(t, bin, db)

	// Start a server, write positions, stop it.
	srv1, addr1 := startServe(t, bin, db)
	post(t, addr1+"/api/projects/1/positions", []map[string]any{
		{"id": 3, "x": 111.0, "y": 222.0},
		{"id": 5, "x": 333.0, "y": 444.0},
	})
	stop(t, srv1)

	// Restart and read them back.
	srv2, addr2 := startServe(t, bin, db)
	defer stop(t, srv2)
	var graph struct {
		Tasks []struct {
			ID int64    `json:"id"`
			X  *float64 `json:"x"`
			Y  *float64 `json:"y"`
		} `json:"tasks"`
	}
	getJSON(t, addr2+"/api/projects/1/graph", &graph)
	got := map[int64][2]float64{}
	for _, tk := range graph.Tasks {
		if tk.X != nil && tk.Y != nil {
			got[tk.ID] = [2]float64{*tk.X, *tk.Y}
		}
	}
	if got[3] != [2]float64{111, 222} || got[5] != [2]float64{333, 444} {
		t.Fatalf("positions after restart = %v, want 3:{111,222} 5:{333,444}", got)
	}
}

// §11.8 — live update across processes: an MCP write must reach an SSE client
// within 2 seconds.
func TestLiveUpdateAcrossProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	bin := buildBinary(t)
	dir := t.TempDir()
	db := filepath.Join(dir, "graphd.db")
	seedFixture(t, bin, db)

	srv, addr := startServe(t, bin, db)
	defer stop(t, srv)

	// Open the SSE stream.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", addr+"/api/projects/1/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE connect: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("SSE content type = %q", ct)
	}
	reader := bufio.NewReader(resp.Body)

	// Drain the initial revision event.
	rev0 := readRevision(t, reader)

	// Write through a separate mcp process.
	mcp := exec.Command(bin, "mcp", "--db", db)
	stdin, _ := mcp.StdinPipe()
	stdout, _ := mcp.StdoutPipe()
	mcp.Stderr = os.Stderr
	if err := mcp.Start(); err != nil {
		t.Fatalf("start mcp: %v", err)
	}
	defer func() { stdin.Close(); mcp.Wait() }()

	send := func(line string) {
		if _, err := io.WriteString(stdin, line+"\n"); err != nil {
			t.Fatalf("mcp stdin: %v", err)
		}
	}
	mcpReader := bufio.NewReader(stdout)
	readMCPLine := func() string {
		line, err := mcpReader.ReadString('\n')
		if err != nil {
			t.Fatalf("mcp stdout: %v", err)
		}
		return line
	}
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	readMCPLine()
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"create_task","arguments":{"project":"fixture","label":"filed by an agent"}}}`)
	created := readMCPLine()
	if !strings.Contains(created, "FIX-21") {
		t.Fatalf("mcp create_task response = %s", created)
	}

	// The SSE client must see a bumped revision within 2s.
	deadline := time.Now().Add(2 * time.Second)
	rev1 := rev0
	for time.Now().Before(deadline) {
		rev1 = readRevision(t, reader)
		if rev1 > rev0 {
			break
		}
	}
	if rev1 <= rev0 {
		t.Fatalf("no SSE revision bump within 2s (stuck at %d)", rev0)
	}
}

// §11.11 — the bind guard.
func TestBindGuard(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	bin := buildBinary(t)
	db := filepath.Join(t.TempDir(), "graphd.db")

	// 0.0.0.0 is not loopback: refuse, non-zero exit, do not bind.
	out, err := exec.Command(bin, "serve", "--db", db, "--listen", "0.0.0.0:7399").CombinedOutput()
	if err == nil {
		t.Fatalf("serve --listen 0.0.0.0:7399 succeeded; want a refusal. output:\n%s", out)
	}
	if !strings.Contains(string(out), "refusing to bind") {
		t.Errorf("refusal message missing from output:\n%s", out)
	}
	// And it must not have bound.
	if conn, err := net.DialTimeout("tcp", "127.0.0.1:7399", 200*time.Millisecond); err == nil {
		conn.Close()
		t.Fatalf("serve bound 0.0.0.0:7399 despite refusing")
	}

	// --allow-remote permits it.
	cmd := exec.Command(bin, "serve", "--db", db, "--listen", "127.0.0.1:0", "--allow-remote")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start with --allow-remote: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

func TestIsLoopback(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:7331": true,
		"localhost:7331": true,
		"[::1]:7331":     true,
		"0.0.0.0:7331":   false,
		":7331":          false,
		"192.168.1.5:80": false,
	}
	for addr, want := range cases {
		if got := IsLoopback(addr); got != want {
			t.Errorf("IsLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}

// ---- integration helpers ----

var (
	binOnce sync.Once
	binPath string
	binErr  error
)

// buildBinary compiles graphd once per test run.
func buildBinary(t *testing.T) string {
	t.Helper()
	binOnce.Do(func() {
		dir, err := os.MkdirTemp("", "graphd-bin")
		if err != nil {
			binErr = err
			return
		}
		binPath = filepath.Join(dir, "graphd")
		cmd := exec.Command("go", "build", "-o", binPath, ".")
		cmd.Dir = repoRoot()
		out, err := cmd.CombinedOutput()
		if err != nil {
			binErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if binErr != nil {
		t.Fatalf("%v", binErr)
	}
	return binPath
}

func repoRoot() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "."
		}
		dir = parent
	}
}

func run(t *testing.T, bin string, args ...string) {
	t.Helper()
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", bin, args, err, out)
	}
}

// seedFixture runs the binary in serve mode just long enough to seed the
// fixture, then stops it. There is no separate seed subcommand on purpose —
// --seed-fixture is a serve flag (SPEC §11.1).
func seedFixture(t *testing.T, bin, db string) {
	t.Helper()
	cmd, _ := startServe(t, bin, db, "--seed-fixture")
	stop(t, cmd)
}

// startServe starts `graphd serve --listen 127.0.0.1:0` and returns the process
// and the resolved base URL.
func startServe(t *testing.T, bin, db string, extra ...string) (*exec.Cmd, string) {
	t.Helper()
	args := append([]string{"serve", "--db", db, "--listen", "127.0.0.1:0"}, extra...)
	cmd := exec.Command(bin, args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start serve: %v", err)
	}
	addrCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			line := sc.Text()
			if i := strings.Index(line, "http://"); i >= 0 {
				addr := line[i:]
				if j := strings.IndexAny(addr, " \t"); j >= 0 {
					addr = addr[:j]
				}
				select {
				case addrCh <- addr:
				default:
				}
			}
		}
	}()
	select {
	case addr := <-addrCh:
		return cmd, addr
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("serve did not report an address in time")
		return nil, ""
	}
}

func stop(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}

func post(t *testing.T, url string, body any) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST %s = %d: %s", url, resp.StatusCode, body)
	}
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s = %d: %s", url, resp.StatusCode, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

// readRevision reads one SSE `data: {"revision":N}` line.
func readRevision(t *testing.T, r *bufio.Reader) int64 {
	t.Helper()
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("SSE read: %v", err)
		}
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var payload struct {
			Revision int64 `json:"revision"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &payload); err != nil {
			t.Fatalf("SSE data line %q: %v", line, err)
		}
		return payload.Revision
	}
}
