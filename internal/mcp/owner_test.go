package mcp

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"graphd/internal/store"
)

// call builds one tools/call line.
func call(id int, name, args string) string {
	return `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"tools/call","params":{"name":"` +
		name + `","arguments":` + args + `}}`
}

// toolText runs one tools/call and returns the text content of the result.
func toolText(t *testing.T, s *session, n int) string {
	t.Helper()
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	s.result(n, &res)
	if res.IsError {
		t.Fatalf("tool returned isError: %s", res.Content[0].Text)
	}
	return res.Content[0].Text
}

type nextPayload struct {
	Next *struct {
		Key   string `json:"key"`
		Owner string `json:"owner"`
	} `json:"next"`
	Reason        string `json:"reason"`
	AwaitingHuman []struct {
		Key   string `json:"key"`
		Owner string `json:"owner"`
	} `json:"awaiting_human"`
}

func getNext(t *testing.T, s *session) nextPayload {
	t.Helper()
	var p nextPayload
	if err := json.Unmarshal([]byte(toolText(t, s, 0)), &p); err != nil {
		t.Fatalf("get_next_task text is not JSON: %v", err)
	}
	return p
}

// The agent's next task skips human-owned work and reports it separately, so a
// harness can hand it to the user instead of attempting it
// (docs/task-owners.md §3).
func TestMCPGetNextTaskPartitionsHuman(t *testing.T) {
	srv, _, _ := newTestServer(t)

	// Mark the top of the frontier human, over MCP — the field must be writable
	// through the same tool an agent already uses.
	mark := newSession(t, srv, call(1, "update_task", `{"task":"FIX-3","owner":"human"}`))
	var marked struct {
		Key   string `json:"key"`
		Owner string `json:"owner"`
	}
	if err := json.Unmarshal([]byte(toolText(t, mark, 0)), &marked); err != nil {
		t.Fatalf("update_task text is not JSON: %v", err)
	}
	if marked.Owner != "human" {
		t.Fatalf("update_task owner = %q, want human", marked.Owner)
	}

	s := newSession(t, srv, call(1, "get_next_task", `{"project":"fixture"}`))
	p := getNext(t, s)
	if p.Reason != "ok" {
		t.Errorf("reason = %q, want ok (agent work remains)", p.Reason)
	}
	if p.Next == nil || p.Next.Key != "FIX-14" {
		t.Errorf("next = %+v, want FIX-14", p.Next)
	}
	if p.Next != nil && p.Next.Owner != "agent" {
		t.Errorf("next.owner = %q, want agent", p.Next.Owner)
	}
	if len(p.AwaitingHuman) != 1 || p.AwaitingHuman[0].Key != "FIX-3" {
		t.Fatalf("awaiting_human = %+v, want [FIX-3]", p.AwaitingHuman)
	}
	if p.AwaitingHuman[0].Owner != "human" {
		t.Errorf("awaiting_human[0].owner = %q, want human", p.AwaitingHuman[0].Owner)
	}
}

// When all the ready work is human, the agent is told so explicitly. `next` is
// null but the reason is not "no_ready_tasks" — the project is waiting, not
// finished.
func TestMCPGetNextTaskAwaitingHumanReason(t *testing.T) {
	srv, st, p := newTestServer(t)
	ctx := context.Background()

	// Fixture frontier is {3, 11, 14}; hand all of it to the human.
	for _, key := range []string{"FIX-3", "FIX-11", "FIX-14"} {
		tk, err := st.ResolveTask(ctx, key)
		if err != nil {
			t.Fatalf("ResolveTask %s: %v", key, err)
		}
		if _, err := st.UpdateTask(ctx, tk.ID, store.TaskPatch{Owner: ptr(store.OwnerHuman)}); err != nil {
			t.Fatalf("mark %s human: %v", key, err)
		}
	}
	_ = p

	s := newSession(t, srv, call(1, "get_next_task", `{"project":"fixture"}`))
	got := getNext(t, s)
	if got.Next != nil {
		t.Errorf("next = %+v, want nil", got.Next)
	}
	if got.Reason != "awaiting_human" {
		t.Errorf("reason = %q, want awaiting_human", got.Reason)
	}
	if len(got.AwaitingHuman) != 3 {
		t.Errorf("awaiting_human has %d entries, want 3", len(got.AwaitingHuman))
	}
	// Ranked like the frontier: unblocks DESC, priority ASC, id ASC.
	if len(got.AwaitingHuman) == 3 {
		want := []string{"FIX-3", "FIX-14", "FIX-11"}
		for i, w := range want {
			if got.AwaitingHuman[i].Key != w {
				t.Errorf("awaiting_human[%d] = %s, want %s", i, got.AwaitingHuman[i].Key, w)
			}
		}
	}
}

// `awaiting_human` is always present, so a client never has to tell "no human
// work" apart from "this shape does not carry the field".
func TestMCPNextTaskAwaitingHumanAlwaysPresent(t *testing.T) {
	srv, _, _ := newTestServer(t)
	s := newSession(t, srv, call(1, "get_next_task", `{"project":"fixture"}`))
	text := toolText(t, s, 0)
	if !jsonHasField(t, text, "awaiting_human") {
		t.Errorf("get_next_task payload has no awaiting_human field:\n%s", text)
	}
	p := getNext(t, s)
	if p.AwaitingHuman == nil {
		t.Errorf("awaiting_human decoded to nil; want an empty array")
	}
	if len(p.AwaitingHuman) != 0 {
		t.Errorf("awaiting_human = %+v, want empty", p.AwaitingHuman)
	}
}

// get_graph carries the owner so an agent can see a plan's human steps without
// calling get_next_task.
func TestMCPGetGraphCarriesOwner(t *testing.T) {
	srv, st, p := newTestServer(t)
	ctx := context.Background()
	if _, err := st.UpdateTask(ctx, 3, store.TaskPatch{Owner: ptr(store.OwnerHuman)}); err != nil {
		t.Fatalf("mark human: %v", err)
	}
	_ = p

	s := newSession(t, srv, call(1, "get_graph", `{"project":"fixture"}`))
	var payload struct {
		Tasks []struct {
			Key   string `json:"key"`
			Owner string `json:"owner"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(toolText(t, s, 0)), &payload); err != nil {
		t.Fatalf("get_graph text is not JSON: %v", err)
	}
	byKey := map[string]string{}
	for _, tk := range payload.Tasks {
		byKey[tk.Key] = tk.Owner
	}
	if byKey["FIX-3"] != "human" {
		t.Errorf("FIX-3 owner in get_graph = %q, want human", byKey["FIX-3"])
	}
	if byKey["FIX-14"] != "agent" {
		t.Errorf("FIX-14 owner in get_graph = %q, want agent", byKey["FIX-14"])
	}
}

// scaffold_plan is where a planner marks the human steps, so the field must ride
// through it.
func TestMCPScaffoldPlanOwner(t *testing.T) {
	srv, _, _ := newTestServer(t)
	args := `{"project":"fixture","tasks":[` +
		`{"ref":"prep","label":"Provision VM"},` +
		`{"ref":"cutover","label":"App switchover","owner":"human"}` +
		`],"edges":[{"blocker":"prep","blocked":"cutover"}]}`
	s := newSession(t, srv, call(1, "scaffold_plan", args))

	var out struct {
		Tasks []struct {
			Label string `json:"label"`
			Owner string `json:"owner"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(toolText(t, s, 0)), &out); err != nil {
		t.Fatalf("scaffold_plan text is not JSON: %v", err)
	}
	owners := map[string]string{}
	for _, tk := range out.Tasks {
		owners[tk.Label] = tk.Owner
	}
	if owners["App switchover"] != "human" {
		t.Errorf("scaffolded human task owner = %q, want human", owners["App switchover"])
	}
	if owners["Provision VM"] != "agent" {
		t.Errorf("scaffolded default task owner = %q, want agent", owners["Provision VM"])
	}
}

// The owner guidance must reach the model through the tool schema, which is
// re-sent on every call — not only through documentation the harness may not
// load.
func TestMCPOwnerGuidanceInSchema(t *testing.T) {
	srv, _, _ := newTestServer(t)
	s := newSession(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	var res struct {
		Tools []tool `json:"tools"`
	}
	s.result(0, &res)

	var createSchema, nextDesc string
	for _, tl := range res.Tools {
		switch tl.Name {
		case "create_task":
			props, _ := tl.InputSchema["properties"].(map[string]any)
			if props != nil {
				if p, ok := props["owner"].(map[string]any); ok {
					createSchema, _ = p["description"].(string)
				}
			}
		case "get_next_task":
			nextDesc = tl.Description
		}
	}
	if createSchema == "" {
		t.Errorf("create_task has no owner property description")
	}
	if !contains(nextDesc, "awaiting_human") {
		t.Errorf("get_next_task description does not explain awaiting_human:\n%s", nextDesc)
	}
	if !contains(nextDesc, "do not attempt them") {
		t.Errorf("get_next_task description does not tell the agent to hand off:\n%s", nextDesc)
	}
}

// An agent may hand its own task over, and the task is then reported as the
// human's. This is the mid-flight hand-off (docs/task-owners.md §3.1).
func TestMCPAgentCanHandTaskOver(t *testing.T) {
	srv, _, _ := newTestServer(t)

	// Claim FIX-3, then realise it is not the agent's to do.
	claim := newSession(t, srv, call(1, "update_task", `{"task":"FIX-3","status":"doing"}`))
	if err := json.Unmarshal([]byte(toolText(t, claim, 0)), &map[string]any{}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	hand := newSession(t, srv, call(1, "update_task", `{"task":"FIX-3","owner":"human"}`))
	var handed struct {
		Key   string `json:"key"`
		Owner string `json:"owner"`
	}
	if err := json.Unmarshal([]byte(toolText(t, hand, 0)), &handed); err != nil {
		t.Fatalf("hand over: %v", err)
	}
	if handed.Owner != "human" {
		t.Fatalf("owner after hand-over = %q, want human", handed.Owner)
	}

	// A `doing` human task is in no other bucket, so in_progress must say whose
	// it is — otherwise the hand-off is invisible in every direction.
	s := newSession(t, srv, call(1, "get_next_task", `{"project":"fixture"}`))
	var p struct {
		Next       *struct{ Key string } `json:"next"`
		Reason     string                `json:"reason"`
		InProgress []struct {
			Key   string `json:"key"`
			Owner string `json:"owner"`
		} `json:"in_progress"`
		AwaitingHuman []struct {
			Key string `json:"key"`
		} `json:"awaiting_human"`
	}
	if err := json.Unmarshal([]byte(toolText(t, s, 0)), &p); err != nil {
		t.Fatalf("get_next_task: %v", err)
	}
	if len(p.InProgress) != 1 || p.InProgress[0].Key != "FIX-3" {
		t.Fatalf("in_progress = %+v, want [FIX-3]", p.InProgress)
	}
	if p.InProgress[0].Owner != "human" {
		t.Errorf("in_progress[0].owner = %q, want human (a doing human task is in no other bucket)",
			p.InProgress[0].Owner)
	}
	// And it is NOT offered to the agent as next.
	if p.Next != nil && p.Next.Key == "FIX-3" {
		t.Errorf("a human-owned task was offered as next: %+v", p.Next)
	}
}

// The agent must not close the human's work. This is the one write the field
// exists to prevent (docs/task-owners.md §3.2).
func TestMCPAgentCannotCloseHumanTask(t *testing.T) {
	srv, _, _ := newTestServer(t)

	// Mark FIX-3 human, then try each way of closing it from MCP.
	if err := json.Unmarshal([]byte(toolText(t, newSession(t, srv,
		call(1, "update_task", `{"task":"FIX-3","owner":"human"}`)), 0)), &map[string]any{}); err != nil {
		t.Fatalf("mark human: %v", err)
	}

	for _, tc := range []struct{ name, line string }{
		{"status done", call(1, "update_task", `{"task":"FIX-3","status":"done"}`)},
		{"status cancelled", call(1, "update_task", `{"task":"FIX-3","status":"cancelled"}`)},
		{"archive", call(1, "archive_task", `{"task":"FIX-3"}`)},
	} {
		s := newSession(t, srv, tc.line)
		resp := s.response(0)
		b, _ := json.Marshal(resp.Result)
		var res struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		}
		if err := json.Unmarshal(b, &res); err != nil {
			t.Fatalf("%s: decode: %v", tc.name, err)
		}
		if !res.IsError {
			t.Errorf("%s: the close was allowed, want isError", tc.name)
			continue
		}
		if !strings.Contains(res.Content[0].Text, "human_confirmation_required") {
			t.Errorf("%s: error text = %q, want the human_confirmation_required code", tc.name, res.Content[0].Text)
		}
	}

	// The task is untouched.
	s := newSession(t, srv, call(1, "get_graph", `{"project":"fixture"}`))
	var g struct {
		Tasks []struct {
			Key      string `json:"key"`
			Status   string `json:"status"`
			Archived bool   `json:"archived"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(toolText(t, s, 0)), &g); err != nil {
		t.Fatalf("get_graph: %v", err)
	}
	for _, tk := range g.Tasks {
		if tk.Key != "FIX-3" {
			continue
		}
		if tk.Status != "todo" || tk.Archived {
			t.Errorf("a refused close changed FIX-3: status=%q archived=%v", tk.Status, tk.Archived)
		}
	}
}

// The guard is narrow, not a permission system: every other write to a human
// task still works, including taking it back, and an agent may close its own.
func TestMCPHumanCloseGuardIsNarrow(t *testing.T) {
	srv, _, _ := newTestServer(t)

	// A human task accepts non-closing writes.
	for _, line := range []string{
		call(1, "update_task", `{"task":"FIX-3","owner":"human"}`),
		call(1, "update_task", `{"task":"FIX-3","status":"doing"}`),
		call(1, "update_task", `{"task":"FIX-3","notes":"the human's brief"}`),
		call(1, "update_task", `{"task":"FIX-3","priority":1}`),
	} {
		if got := toolText(t, newSession(t, srv, line), 0); got == "" {
			t.Fatalf("a non-closing write was refused: %s", line)
		}
	}

	// Taking it back restores the ability to close it — the guard follows the
	// owner, it does not latch.
	takeBack := newSession(t, srv, call(1, "update_task", `{"task":"FIX-3","owner":"agent"}`))
	if err := json.Unmarshal([]byte(toolText(t, takeBack, 0)), &map[string]any{}); err != nil {
		t.Fatalf("take back: %v", err)
	}
	done := newSession(t, srv, call(1, "update_task", `{"task":"FIX-3","status":"done"}`))
	var closed struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(toolText(t, done, 0)), &closed); err != nil {
		t.Fatalf("close after taking back: %v", err)
	}
	if closed.Status != "done" {
		t.Errorf("status = %q, want done (the guard must follow the owner)", closed.Status)
	}

	// And a different human task still cannot be closed, so the guard did not
	// latch off after one success.
	mark := newSession(t, srv, call(1, "update_task", `{"task":"FIX-11","owner":"human"}`))
	if err := json.Unmarshal([]byte(toolText(t, mark, 0)), &map[string]any{}); err != nil {
		t.Fatalf("mark FIX-11 human: %v", err)
	}
	s := newSession(t, srv, call(1, "update_task", `{"task":"FIX-11","status":"done"}`))
	b, _ := json.Marshal(s.response(0).Result)
	var res struct {
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.IsError {
		t.Errorf("closing a human task succeeded after another was taken back")
	}
}

func jsonHasField(t *testing.T, text, field string) bool {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		t.Fatalf("not a JSON object: %v", err)
	}
	_, ok := m[field]
	return ok
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
