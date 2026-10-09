package mcp

import (
	"context"
	"encoding/json"
	"strconv"
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
