package store

import (
	"context"
	"testing"
)

// Ownership is orthogonal to readiness. Marking a task human must not move the
// frontier, change its ordering, or alter the leverage arithmetic — the whole
// reason this feature is cheap is that the graph math never learns it exists
// (docs/task-owners.md §2).
func TestOwnerDoesNotChangeFrontier(t *testing.T) {
	ctx := context.Background()
	s, p := newTestStore(t)

	// Task 3 is the top of the frontier: unblocks 3, priority 1.
	before := mustGraph(t, s, p.ID)
	readyBefore := before.ReadyIDs()
	unblocksBefore := before.Unblocks(before.Tasks[3])

	if _, err := s.UpdateTask(ctx, 3, TaskPatch{Owner: ptr(OwnerHuman)}); err != nil {
		t.Fatalf("mark task 3 human: %v", err)
	}

	g := mustGraph(t, s, p.ID)
	if !equalIDs(g.ReadyIDs(), readyBefore) {
		t.Errorf("marking a task human changed the frontier: %v -> %v", readyBefore, g.ReadyIDs())
	}
	if got := g.Unblocks(g.Tasks[3]); got != unblocksBefore {
		t.Errorf("unblocks(3) = %d after marking human, want %d", got, unblocksBefore)
	}

	// And the ranked frontier still lists it first, still carrying the owner so a
	// client can tell whose it is.
	ready, err := s.GetReady(ctx, p.ID, 0)
	if err != nil {
		t.Fatalf("GetReady: %v", err)
	}
	if len(ready.Ready) != 3 || ready.Ready[0].Key != "FIX-3" {
		t.Fatalf("frontier = %v, want FIX-3 first", keysOf(ready.Ready))
	}
	if ready.Ready[0].Owner != OwnerHuman {
		t.Errorf("FIX-3 owner = %q, want %q", ready.Ready[0].Owner, OwnerHuman)
	}
	if ready.Ready[1].Owner != OwnerAgent {
		t.Errorf("FIX-14 owner = %q, want %q", ready.Ready[1].Owner, OwnerAgent)
	}
}

// get_next_task skips human-owned work for the agent's `next`, but reports it
// under awaiting_human rather than dropping it — silently dropping it would make
// "no next task" indistinguishable from "the project is finished"
// (docs/task-owners.md §3).
func TestGetNextTaskPartitionsHuman(t *testing.T) {
	ctx := context.Background()
	s, p := newTestStore(t)

	// Without the marking, the agent's next task is FIX-3.
	nt, err := s.GetNextTask(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetNextTask: %v", err)
	}
	if nt.Next == nil || nt.Next.Key != "FIX-3" {
		t.Fatalf("baseline next = %v, want FIX-3", nt.Next)
	}
	if len(nt.AwaitingHuman) != 0 {
		t.Fatalf("baseline awaiting_human = %v, want empty", nt.AwaitingHuman)
	}

	// Mark the top of the frontier human. The agent is offered the next
	// agent-owned task; the human task is reported alongside it.
	if _, err := s.UpdateTask(ctx, 3, TaskPatch{Owner: ptr(OwnerHuman)}); err != nil {
		t.Fatalf("mark task 3 human: %v", err)
	}
	nt, err = s.GetNextTask(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetNextTask after marking: %v", err)
	}
	if nt.Reason != "ok" {
		t.Errorf("reason = %q, want ok (there is still agent work)", nt.Reason)
	}
	if nt.Next == nil || nt.Next.Key != "FIX-14" {
		t.Errorf("next = %v, want FIX-14 (the next agent-owned task)", nt.Next)
	}
	if len(nt.AwaitingHuman) != 1 || nt.AwaitingHuman[0].Key != "FIX-3" {
		t.Errorf("awaiting_human = %v, want [FIX-3]", keysOf(nt.AwaitingHuman))
	}
	if nt.AwaitingHuman[0].Owner != OwnerHuman {
		t.Errorf("awaiting_human[0].owner = %q, want %q", nt.AwaitingHuman[0].Owner, OwnerHuman)
	}
}

// When every ready task is human, `next` is null — and reason says so. This is
// the assertion that keeps the tool from lying: a bare null would read as
// "nothing left to do".
func TestGetNextTaskAwaitingHumanReason(t *testing.T) {
	ctx := context.Background()
	s, p := newTestStore(t)

	// The fixture's whole frontier is {3, 11, 14}; make all of it the human's.
	for _, id := range []int64{3, 11, 14} {
		if _, err := s.UpdateTask(ctx, id, TaskPatch{Owner: ptr(OwnerHuman)}); err != nil {
			t.Fatalf("mark task %d human: %v", id, err)
		}
	}
	nt, err := s.GetNextTask(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetNextTask: %v", err)
	}
	if nt.Next != nil {
		t.Errorf("next = %v, want nil (no agent work is ready)", nt.Next)
	}
	if nt.Reason != "awaiting_human" {
		t.Errorf("reason = %q, want awaiting_human", nt.Reason)
	}
	if len(nt.AwaitingHuman) != 3 {
		t.Errorf("awaiting_human has %d entries, want 3", len(nt.AwaitingHuman))
	}
	// Ranked exactly like the frontier: unblocks DESC, priority ASC, id ASC.
	if got := keysOf(nt.AwaitingHuman); got[0] != "FIX-3" || got[1] != "FIX-14" || got[2] != "FIX-11" {
		t.Errorf("awaiting_human order = %v, want [FIX-3 FIX-14 FIX-11]", got)
	}
}

// A genuinely finished project still reports no_ready_tasks with an empty
// awaiting_human — the pre-existing behaviour, unchanged.
func TestGetNextTaskNoReadyTasksUnchanged(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	// A fresh project whose only task is already in flight: nothing is todo, so
	// nothing is ready, and nothing is human.
	pr, err := s.CreateProject(ctx, "busy-project", "BUSY")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err := s.CreateTask(ctx, pr.ID, NewTask{Label: "in flight", Status: StatusDoing}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	nt, err := s.GetNextTask(ctx, pr.ID)
	if err != nil {
		t.Fatalf("GetNextTask: %v", err)
	}
	if nt.Next != nil {
		t.Errorf("next = %v, want nil", nt.Next)
	}
	if nt.Reason != "no_ready_tasks" {
		t.Errorf("reason = %q, want no_ready_tasks", nt.Reason)
	}
	if nt.AwaitingHuman == nil {
		t.Errorf("awaiting_human is nil, want an empty slice (stable JSON shape)")
	}
	if len(nt.AwaitingHuman) != 0 {
		t.Errorf("awaiting_human = %v, want empty", keysOf(nt.AwaitingHuman))
	}
}

// The owner is validated on every write path, and defaults to agent when
// omitted so a caller that never heard of the field keeps working.
func TestOwnerValidationAndDefault(t *testing.T) {
	ctx := context.Background()
	s, p := newTestStore(t)

	// Omitted -> agent.
	tk, err := s.CreateTask(ctx, p.ID, NewTask{Label: "no owner given"})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if tk.Owner != OwnerAgent {
		t.Errorf("default owner = %q, want %q", tk.Owner, OwnerAgent)
	}

	// Explicit human survives the round trip through the row.
	tk2, err := s.CreateTask(ctx, p.ID, NewTask{Label: "for a person", Owner: OwnerHuman})
	if err != nil {
		t.Fatalf("CreateTask human: %v", err)
	}
	if tk2.Owner != OwnerHuman {
		t.Errorf("created owner = %q, want %q", tk2.Owner, OwnerHuman)
	}
	reread, err := s.GetTask(ctx, tk2.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if reread.Owner != OwnerHuman {
		t.Errorf("re-read owner = %q, want %q", reread.Owner, OwnerHuman)
	}

	// Garbage is refused, with a stable code.
	_, err = s.CreateTask(ctx, p.ID, NewTask{Label: "bad", Owner: "robot"})
	if got := AsError(err).Code; got != CodeInvalidOwner {
		t.Errorf("create with bad owner: code = %q, want %q", got, CodeInvalidOwner)
	}
	if _, err := s.UpdateTask(ctx, tk.ID, TaskPatch{Owner: ptr("robot")}); AsError(err).Code != CodeInvalidOwner {
		t.Errorf("patch with bad owner: code = %q, want %q", AsError(err).Code, CodeInvalidOwner)
	}

	// And the invalid value never reached the database.
	if again, err := s.GetTask(ctx, tk.ID); err != nil || again.Owner != OwnerAgent {
		t.Errorf("a refused patch changed the row: owner=%v err=%v", again.Owner, err)
	}
}

func keysOf(es []ReadyEntry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Key)
	}
	return out
}
