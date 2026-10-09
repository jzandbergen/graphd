package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"graphd/internal/store"
)

func ptr[T any](v T) *T { return &v }

func newTestServer(t *testing.T) (*Server, *store.Store, *store.Project) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
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

// session drives the server over in-memory pipes, one request per line.
type session struct {
	t      *testing.T
	srv    *Server
	in     *strings.Reader
	out    *bytes.Buffer
	nextID int
}

func newSession(t *testing.T, srv *Server, lines ...string) *session {
	t.Helper()
	in := strings.NewReader(strings.Join(lines, "\n") + "\n")
	out := &bytes.Buffer{}
	if err := srv.Serve(context.Background(), in, out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	return &session{t: t, srv: srv, in: in, out: out}
}

// responses decodes every line of stdout as a JSON-RPC message. It fails the
// test if any line is not valid JSON — that is the §11.9 "stdout is pure"
// assertion.
func (s *session) responses() []response {
	s.t.Helper()
	var out []response
	for _, line := range strings.Split(strings.TrimSpace(s.out.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r response
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			s.t.Fatalf("stdout line is not JSON-RPC: %q (%v)", line, err)
		}
		if r.JSONRPC != "2.0" {
			s.t.Fatalf("stdout line is not JSON-RPC 2.0: %q", line)
		}
		out = append(out, r)
	}
	return out
}

// result decodes the Nth result into v.
func (s *session) result(n int, v any) {
	s.t.Helper()
	all := s.responses()
	if n >= len(all) {
		s.t.Fatalf("expected at least %d responses, got %d", n+1, len(all))
	}
	if all[n].Error != nil {
		s.t.Fatalf("response %d is an error: %+v", n, all[n].Error)
	}
	b, _ := json.Marshal(all[n].Result)
	if err := json.Unmarshal(b, v); err != nil {
		s.t.Fatalf("decode result %d: %v", n, err)
	}
}

func (s *session) response(n int) response {
	s.t.Helper()
	all := s.responses()
	if n >= len(all) {
		s.t.Fatalf("expected at least %d responses, got %d", n+1, len(all))
	}
	return all[n]
}

// §11.9 — initialize.
func TestMCPInitialize(t *testing.T) {
	srv, _, _ := newTestServer(t)
	s := newSession(t, srv, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)

	var res struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	s.result(0, &res)
	if res.ProtocolVersion != "2024-11-05" {
		t.Errorf("protocolVersion = %q, want 2024-11-05", res.ProtocolVersion)
	}
	if _, ok := res.Capabilities["tools"]; !ok {
		t.Errorf("capabilities has no tools key: %v", res.Capabilities)
	}
	if res.ServerInfo.Name != "graphd" {
		t.Errorf("serverInfo.name = %q", res.ServerInfo.Name)
	}
}

// §11.9 — tools/list returns exactly 13 tools, each with a valid inputSchema.
func TestMCPToolsList(t *testing.T) {
	srv, _, _ := newTestServer(t)
	s := newSession(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)

	var res struct {
		Tools []tool `json:"tools"`
	}
	s.result(0, &res)
	if len(res.Tools) != 13 {
		t.Fatalf("tools/list returned %d tools, want exactly 13", len(res.Tools))
	}
	want := []string{
		"list_projects", "create_project", "get_graph", "get_ready", "get_next_task",
		"create_task", "update_task", "archive_task", "restore_task", "add_edge",
		"remove_edge", "scaffold_plan", "export_json",
	}
	got := map[string]bool{}
	for _, tl := range res.Tools {
		got[tl.Name] = true
		if tl.Description == "" {
			t.Errorf("tool %q has no description", tl.Name)
		}
		if tl.InputSchema["type"] != "object" {
			t.Errorf("tool %q inputSchema.type = %v, want object", tl.Name, tl.InputSchema["type"])
		}
		if _, ok := tl.InputSchema["properties"]; !ok {
			t.Errorf("tool %q inputSchema has no properties", tl.Name)
		}
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("tools/list is missing %q", name)
		}
	}
	// Every handler must be reachable from the catalogue, and vice versa.
	if len(toolHandlers) != len(toolDefs) {
		t.Errorf("toolHandlers has %d entries, toolDefs has %d", len(toolHandlers), len(toolDefs))
	}
	for _, tl := range toolDefs {
		if _, ok := toolHandlers[tl.Name]; !ok {
			t.Errorf("tool %q has no handler", tl.Name)
		}
	}
}

// The handoff: get_next_task returns the work *and* the outputs it consumes.
// This is the feature's whole point — one call, no second lookup
// (docs/task-outputs.md §5).
func TestMCPGetNextTaskCarriesInputs(t *testing.T) {
	srv, st, p := newTestServer(t)
	ctx := context.Background()

	// Task 3 blocks 4, 5 and 17. Record an output on 3 and close it. The frontier
	// then ranks FIX-5 first (priority 1, unblocks 1), so FIX-5 is what
	// get_next_task returns and it should carry 3's output as its input.
	if _, err := st.UpdateTask(ctx, 3, store.TaskPatch{
		Output: ptr("ANALYSIS: counter key is rl:{tenant}:{window}"),
		Status: ptr(store.StatusDone),
	}); err != nil {
		t.Fatalf("set up task 3: %v", err)
	}

	s := newSession(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_next_task","arguments":{"project":"fixture"}}}`)
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	s.result(0, &res)
	if res.IsError {
		t.Fatalf("get_next_task isError: %s", res.Content[0].Text)
	}
	var payload struct {
		Next struct {
			Key    string `json:"key"`
			Inputs []struct {
				Key       string `json:"key"`
				Status    string `json:"status"`
				Output    string `json:"output"`
				Truncated bool   `json:"truncated"`
			} `json:"inputs"`
		} `json:"next"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].Text), &payload); err != nil {
		t.Fatalf("decode payload: %v\n%s", err, res.Content[0].Text)
	}
	// FIX-5 (priority 1, unblocks 1) outranks FIX-4 (priority 2, unblocks 0).
	if payload.Next.Key != "FIX-5" {
		t.Fatalf("next = %q, want FIX-5 (the fixture's top of frontier once 3 is done)", payload.Next.Key)
	}
	if len(payload.Next.Inputs) != 1 {
		t.Fatalf("next.inputs = %+v, want exactly the one blocker (FIX-3)", payload.Next.Inputs)
	}
	in := payload.Next.Inputs[0]
	if in.Key != "FIX-3" || in.Status != "done" {
		t.Errorf("input = {%s %s}, want {FIX-3 done}", in.Key, in.Status)
	}
	if in.Output != "ANALYSIS: counter key is rl:{tenant}:{window}" {
		t.Errorf("input output = %q", in.Output)
	}
	if in.Truncated {
		t.Errorf("a short output should not be marked truncated")
	}
	_ = p
}

// The frontier tools bound how much upstream text they inline; get_graph returns
// it whole. The marker is what tells a caller to go and fetch the rest.
func TestMCPInputTruncation(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()

	long := strings.Repeat("x", inputBudget*3) + "\nTAIL-MARKER\n"
	if _, err := st.UpdateTask(ctx, 3, store.TaskPatch{
		Output: ptr(long),
		Status: ptr(store.StatusDone),
	}); err != nil {
		t.Fatalf("set up task 3: %v", err)
	}

	s := newSession(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_ready","arguments":{"project":"fixture"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_graph","arguments":{"project":"fixture"}}}`)
	var ready struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	s.result(0, &ready)
	var rp struct {
		Ready []struct {
			Key    string `json:"key"`
			Inputs []struct {
				Output    string `json:"output"`
				Truncated bool   `json:"truncated"`
			} `json:"inputs"`
		} `json:"ready"`
	}
	if err := json.Unmarshal([]byte(ready.Content[0].Text), &rp); err != nil {
		t.Fatalf("decode get_ready: %v", err)
	}
	var checked bool
	for _, e := range rp.Ready {
		if e.Key != "FIX-4" {
			continue
		}
		checked = true
		if len(e.Inputs) != 1 {
			t.Fatalf("FIX-4 inputs = %+v", e.Inputs)
		}
		in := e.Inputs[0]
		if !in.Truncated {
			t.Errorf("a %d-byte output should be marked truncated at the frontier", len(long))
		}
		if len(in.Output) > inputBudget {
			t.Errorf("truncated output is %d bytes, want <= %d", len(in.Output), inputBudget)
		}
		if strings.Contains(in.Output, "TAIL-MARKER") {
			t.Errorf("truncated output still contains the tail")
		}
	}
	if !checked {
		t.Fatalf("FIX-4 not in the frontier: %+v", rp.Ready)
	}

	// get_graph returns the full text, untruncated.
	var graph struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	s.result(1, &graph)
	var gp struct {
		Tasks []struct {
			Key    string `json:"key"`
			Output string `json:"output"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(graph.Content[0].Text), &gp); err != nil {
		t.Fatalf("decode get_graph: %v", err)
	}
	var sawFull bool
	for _, tk := range gp.Tasks {
		if tk.Key == "FIX-3" {
			if tk.Output != long {
				t.Errorf("get_graph returned %d bytes of output, want the full %d", len(tk.Output), len(long))
			}
			sawFull = true
		}
	}
	if !sawFull {
		t.Errorf("get_graph did not return FIX-3")
	}
}

// create_task and update_task round-trip the output field.
func TestMCPOutputRoundTrip(t *testing.T) {
	srv, _, _ := newTestServer(t)
	s := newSession(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_task","arguments":{"project":"fixture","label":"probe","output":"first"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"update_task","arguments":{"task":"FIX-21","output":"second"}}}`)
	var created struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	s.result(0, &created)
	if created.IsError {
		t.Fatalf("create_task isError: %s", created.Content[0].Text)
	}
	var ct struct {
		Key    string `json:"key"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal([]byte(created.Content[0].Text), &ct); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if ct.Output != "first" {
		t.Errorf("created output = %q, want first", ct.Output)
	}

	var updated struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	s.result(1, &updated)
	if updated.IsError {
		t.Fatalf("update_task isError: %s", updated.Content[0].Text)
	}
	var ut struct {
		Key    string `json:"key"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal([]byte(updated.Content[0].Text), &ut); err != nil {
		t.Fatalf("decode update: %v", err)
	}
	if ut.Key != "FIX-21" || ut.Output != "second" {
		t.Errorf("updated = {%s %q}, want {FIX-21 second}", ut.Key, ut.Output)
	}
}

// §11.9 — get_next_task on the fixture returns FIX-3 with unblocks 3.
func TestMCPGetNextTask(t *testing.T) {
	srv, _, _ := newTestServer(t)
	s := newSession(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_next_task","arguments":{"project":"fixture"}}}`)

	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	s.result(0, &res)
	if res.IsError {
		t.Fatalf("get_next_task returned isError: %s", res.Content[0].Text)
	}
	if res.Content[0].Type != "text" {
		t.Errorf("content type = %q, want text", res.Content[0].Type)
	}
	var payload struct {
		Next struct {
			Key      string `json:"key"`
			Unblocks int    `json:"unblocks"`
		} `json:"next"`
		Reason     string `json:"reason"`
		InProgress []any  `json:"in_progress"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].Text), &payload); err != nil {
		t.Fatalf("tool result text is not JSON: %v\n%s", err, res.Content[0].Text)
	}
	if payload.Next.Key != "FIX-3" || payload.Next.Unblocks != 3 {
		t.Errorf("next = %+v, want FIX-3 with unblocks 3", payload.Next)
	}
	if payload.Reason != "ok" {
		t.Errorf("reason = %q, want ok", payload.Reason)
	}
}

// §11.9 — a cycle comes back as isError:true with the path in the text, NOT as
// a JSON-RPC error object.
func TestMCPCycleIsToolErrorNotProtocolError(t *testing.T) {
	srv, _, _ := newTestServer(t)
	s := newSession(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add_edge","arguments":{"project":"fixture","blocker":"FIX-7","blocked":"FIX-3"}}}`)

	resp := s.response(0)
	if resp.Error != nil {
		t.Fatalf("cycle was reported as a JSON-RPC error object: %+v", resp.Error)
	}
	b, _ := json.Marshal(resp.Result)
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	_ = json.Unmarshal(b, &res)
	if !res.IsError {
		t.Fatalf("cycle result isError = false, want true")
	}
	text := res.Content[0].Text
	if !strings.Contains(text, "cycle_detected") {
		t.Errorf("cycle error text lacks the code: %q", text)
	}
	for _, key := range []string{"FIX-3", "FIX-4", "FIX-7"} {
		if !strings.Contains(text, key) {
			t.Errorf("cycle error text lacks %s: %q", key, text)
		}
	}
}

// §11.9 — stdout contains nothing but JSON-RPC lines across a full session,
// including a notification, an unknown method and a parse error.
func TestMCPStdoutIsPure(t *testing.T) {
	srv, _, _ := newTestServer(t)
	s := newSession(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":4,"method":"no/such/method"}`,
		`this is not json`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"get_ready","arguments":{"project":"fixture"}}}`,
	)
	all := s.responses()
	// initialize, ping, tools/list, unknown method, parse error, tools/call = 6.
	// The notification produces nothing.
	if len(all) != 6 {
		t.Fatalf("got %d responses, want 6 (the notification must be silent)", len(all))
	}
	if all[3].Error == nil || all[3].Error.Code != codeMethodNotFound {
		t.Errorf("unknown method response = %+v, want method-not-found error", all[3])
	}
	if all[4].Error == nil || all[4].Error.Code != codeParseError {
		t.Errorf("parse error response = %+v, want parse error", all[4])
	}
}

// §11.10 — scaffold_plan atomicity through the MCP surface.
func TestMCPScaffoldPlanAtomicity(t *testing.T) {
	srv, st, p := newTestServer(t)
	ctx := context.Background()
	revBefore, _ := st.Revision(ctx)

	cycleCall := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"scaffold_plan","arguments":{` +
		`"project":"fixture",` +
		`"tasks":[{"ref":"a","label":"A"},{"ref":"b","label":"B"},{"ref":"c","label":"C"},{"ref":"d","label":"D"},{"ref":"e","label":"E"}],` +
		`"edges":[{"blocker":"a","blocked":"b"},{"blocker":"b","blocked":"c"},{"blocker":"c","blocked":"a"}]}}}`
	s := newSession(t, srv, cycleCall)
	resp := s.response(0)
	if resp.Error != nil {
		t.Fatalf("scaffold cycle was a protocol error: %+v", resp.Error)
	}
	b, _ := json.Marshal(resp.Result)
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	_ = json.Unmarshal(b, &res)
	if !res.IsError {
		t.Fatalf("scaffold cycle isError = false, want true")
	}
	if !strings.Contains(res.Content[0].Text, "cycle_detected") {
		t.Errorf("scaffold cycle text = %q", res.Content[0].Text)
	}

	if got, _ := st.Revision(ctx); got != revBefore {
		t.Errorf("revision changed on a rejected scaffold: %d -> %d", revBefore, got)
	}
	g, _ := st.LoadGraph(ctx, p.ID)
	if len(g.Tasks) != 20 || len(g.Edges) != 24 {
		t.Errorf("failed scaffold left %d tasks / %d edges, want 20 / 24", len(g.Tasks), len(g.Edges))
	}

	// Now a valid plan.
	validCall := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"scaffold_plan","arguments":{` +
		`"project":"fixture",` +
		`"tasks":[{"ref":"design","label":"Design"},{"ref":"mw","label":"Middleware"},{"ref":"store","label":"Store"},{"ref":"tests","label":"Tests"},{"ref":"ship","label":"Ship"}],` +
		`"edges":[{"blocker":"design","blocked":"mw"},{"blocker":"mw","blocked":"store"},{"blocker":"store","blocked":"tests"},{"blocker":"mw","blocked":"tests"},{"blocker":"tests","blocked":"ship"}]}}}`
	s2 := newSession(t, srv, validCall)
	resp2 := s2.response(0)
	if resp2.Error != nil {
		t.Fatalf("valid scaffold was a protocol error: %+v", resp2.Error)
	}
	b2, _ := json.Marshal(resp2.Result)
	var res2 struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	_ = json.Unmarshal(b2, &res2)
	if res2.IsError {
		t.Fatalf("valid scaffold failed: %s", res2.Content[0].Text)
	}
	var summary struct {
		Created int `json:"created"`
		Edges   int `json:"edges"`
	}
	if err := json.Unmarshal([]byte(res2.Content[0].Text), &summary); err != nil {
		t.Fatalf("scaffold summary: %v", err)
	}
	if summary.Created != 5 || summary.Edges != 5 {
		t.Errorf("scaffold summary = %+v, want 5 tasks / 5 edges", summary)
	}
	g2, _ := st.LoadGraph(ctx, p.ID)
	if len(g2.Tasks) != 25 || len(g2.Edges) != 29 {
		t.Errorf("after valid scaffold: %d tasks / %d edges, want 25 / 29", len(g2.Tasks), len(g2.Edges))
	}
}

// The build-contract guidance must reach the model on every call, so it lives
// in the `notes` field description of create_task, update_task and
// scaffold_plan — the field schemas are re-sent with every tools/list and every
// call, unlike a skill file the harness may or may not load.
func TestMCPNotesCarryBuildContractGuidance(t *testing.T) {
	srv, _, _ := newTestServer(t)
	s := newSession(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)

	var res struct {
		Tools []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	s.result(0, &res)

	// The headings a finished note is expected to carry.
	want := []string{"Problem", "Action Items", "Interfaces", "Pseudocode",
		"Validation contract", "Non-goals", "References", "Markdown"}

	checkNotes := func(toolName, where string, props map[string]any) {
		t.Helper()
		raw, ok := props["notes"]
		if !ok {
			t.Errorf("%s: no notes field at %s", toolName, where)
			return
		}
		field, ok := raw.(map[string]any)
		if !ok {
			t.Errorf("%s: notes at %s is not an object", toolName, where)
			return
		}
		desc, _ := field["description"].(string)
		for _, w := range want {
			if !strings.Contains(desc, w) {
				t.Errorf("%s: notes description at %s is missing %q", toolName, where, w)
			}
		}
	}

	byName := map[string]map[string]any{}
	for _, tl := range res.Tools {
		byName[tl.Name] = tl.InputSchema
	}
	for _, name := range []string{"create_task", "update_task"} {
		props, _ := byName[name]["properties"].(map[string]any)
		checkNotes(name, "properties", props)
	}
	// scaffold_plan nests its notes inside tasks.items.properties.
	sp, ok := byName["scaffold_plan"]["properties"].(map[string]any)
	if !ok {
		t.Fatalf("scaffold_plan has no properties")
	}
	tasks, _ := sp["tasks"].(map[string]any)
	items, _ := tasks["items"].(map[string]any)
	itemProps, _ := items["properties"].(map[string]any)
	checkNotes("scaffold_plan", "tasks.items.properties", itemProps)
}

// Project resolution order: id, then name, then key_prefix; and GRAPHD_PROJECT
// as the fallback when `project` is omitted (SPEC §8.3).
func TestMCPProjectResolution(t *testing.T) {
	srv, _, _ := newTestServer(t)
	t.Setenv("GRAPHD_PROJECT", "FIX")

	calls := []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_next_task","arguments":{"project":"fixture"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_next_task","arguments":{"project":"FIX"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_next_task","arguments":{"project":"1"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"get_next_task","arguments":{}}}`,
	}
	s := newSession(t, srv, calls...)
	for i := 0; i < 4; i++ {
		resp := s.response(i)
		b, _ := json.Marshal(resp.Result)
		var res struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		}
		_ = json.Unmarshal(b, &res)
		if res.IsError {
			t.Errorf("resolution case %d failed: %s", i, res.Content[0].Text)
			continue
		}
		if !strings.Contains(res.Content[0].Text, "FIX-3") {
			t.Errorf("resolution case %d did not resolve to the fixture: %s", i, res.Content[0].Text)
		}
	}

	// With no project and no env, the error must list the projects.
	t.Setenv("GRAPHD_PROJECT", "")
	s2 := newSession(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_next_task","arguments":{}}}`)
	resp := s2.response(0)
	b, _ := json.Marshal(resp.Result)
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	_ = json.Unmarshal(b, &res)
	if !res.IsError {
		t.Fatalf("missing project did not error")
	}
	if !strings.Contains(res.Content[0].Text, "fixture") {
		t.Errorf("no-project error does not list projects: %q", res.Content[0].Text)
	}
}

// The multi-agent coordination contract, which the README leans on heavily:
//
//   - the frontier is the queue, and it is ordered deterministically, so two
//     agents calling get_next_task get the same answer
//   - `doing` is the claim, and it is cooperative: a task handed out is NOT
//     reserved, so two simultaneous callers do get the same task. What
//     publishes the claim is the writer moving it to `doing`.
//   - a claimed task leaves the frontier and is reported under `in_progress`
//   - releasing it (back to todo) returns it to the frontier
//
// The lock-free design is deliberate (see docs/design.md), so this pins the
// behaviour rather than treating the race as a bug to fix.
func TestMCPFrontierClaimContract(t *testing.T) {
	srv, _, _ := newTestServer(t)
	call := func(id int, name string, args string) string {
		return `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"tools/call","params":{"name":"` +
			name + `","arguments":` + args + `}}`
	}
	type nextPayload struct {
		Next *struct {
			Key string `json:"key"`
		} `json:"next"`
		Reason     string `json:"reason"`
		InProgress []struct {
			Key string `json:"key"`
		} `json:"in_progress"`
	}
	// Tool results arrive as {content:[{type,text}],isError}; the payload is the
	// text, not the result object (SPEC §8.1).
	type toolResult struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	get := func(s *session, n int) nextPayload {
		t.Helper()
		var res toolResult
		s.result(n, &res)
		if res.IsError {
			t.Fatalf("get_next_task returned isError: %s", res.Content[0].Text)
		}
		var p nextPayload
		if err := json.Unmarshal([]byte(res.Content[0].Text), &p); err != nil {
			t.Fatalf("tool result text is not JSON: %v\n%s", err, res.Content[0].Text)
		}
		return p
	}

	// The fixture frontier is {3, 11, 14}; 3 leads on unblocks.
	s := newSession(t, srv, call(1, "get_next_task", `{"project":"fixture"}`))
	first := get(s, 0)
	if first.Next == nil || first.Next.Key != "FIX-3" {
		t.Fatalf("frontier head = %+v, want FIX-3", first.Next)
	}
	if first.Reason != "ok" {
		t.Errorf("reason = %q, want ok", first.Reason)
	}

	// A second agent asking at the same moment gets the same task: nothing is
	// reserved on read. This is the documented race, not a defect.
	s2 := newSession(t, srv, call(1, "get_next_task", `{"project":"fixture"}`))
	if second := get(s2, 0); second.Next == nil || second.Next.Key != "FIX-3" {
		t.Fatalf("concurrent read = %+v, want the same unreserved FIX-3", second.Next)
	}

	// The claim is published by the writer setting `doing`.
	s3 := newSession(t, srv, call(1, "update_task",
		`{"project":"fixture","task":"FIX-3","status":"doing"}`))
	s3.result(0, &map[string]any{})

	s4 := newSession(t, srv, call(1, "get_next_task", `{"project":"fixture"}`))
	claimed := get(s4, 0)
	if claimed.Next != nil && claimed.Next.Key == "FIX-3" {
		t.Errorf("a claimed task is still on the frontier: %+v", claimed.Next)
	}
	if len(claimed.InProgress) != 1 || claimed.InProgress[0].Key != "FIX-3" {
		t.Errorf("in_progress = %+v, want [FIX-3]", claimed.InProgress)
	}

	// Releasing it returns it to the frontier, and clears in_progress.
	s5 := newSession(t, srv, call(1, "update_task",
		`{"project":"fixture","task":"FIX-3","status":"todo"}`))
	s5.result(0, &map[string]any{})

	s6 := newSession(t, srv, call(1, "get_next_task", `{"project":"fixture"}`))
	released := get(s6, 0)
	if released.Next == nil || released.Next.Key != "FIX-3" {
		t.Errorf("released task did not return to the frontier: %+v", released.Next)
	}
	if len(released.InProgress) != 0 {
		t.Errorf("in_progress = %+v after release, want empty", released.InProgress)
	}
}

// get_graph must not expose x/y to agents (SPEC §8.2).
func TestMCPGetGraphOmitsPositions(t *testing.T) {
	srv, st, p := newTestServer(t)
	if err := st.SetPositions(context.Background(), p.ID, []store.Position{{ID: 3, X: 10, Y: 20}}); err != nil {
		t.Fatalf("SetPositions: %v", err)
	}
	s := newSession(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_graph","arguments":{"project":"fixture"}}}`)
	resp := s.response(0)
	b, _ := json.Marshal(resp.Result)
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	_ = json.Unmarshal(b, &res)
	if strings.Contains(res.Content[0].Text, "\"x\"") || strings.Contains(res.Content[0].Text, "\"y\"") {
		t.Errorf("get_graph leaks positions to agents:\n%s", res.Content[0].Text)
	}
}
