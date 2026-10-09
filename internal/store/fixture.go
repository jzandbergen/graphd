package store

import (
	"context"
	"database/sql"
	"fmt"
)

// The deterministic fixture from SPEC §11.1: a 20-task / 24-edge project named
// "fixture" with prefix "FIX". It is used by the tests and exposed as
// `graphd serve --seed-fixture` so a human can eyeball the UI against
// known-correct data on day one.
//
// The asserted frontier is {3, 11, 14} and unblocks(3) == 3. That is the
// correctness anchor for the whole program.

type fixtureTask struct {
	n        int
	label    string
	status   string
	priority int
}

// fixtureHuman names the fixture tasks owned by the human rather than an agent.
// Task 10, "Rollout behind flag", is a production change: exactly the kind of
// step an agent must hand to its user, so `--seed-fixture` shows a human-owned
// node on day one (docs/task-owners.md §6). Everything else defaults to agent.
var fixtureHuman = map[int]bool{10: true}

var fixtureTasks = []fixtureTask{
	{1, "Design schema", StatusDone, 3},
	{2, "Write migrations", StatusDone, 3},
	{3, "Token bucket middleware", StatusTodo, 1},
	{4, "Redis counter store", StatusTodo, 2},
	{5, "Config loader", StatusTodo, 1},
	{6, "Unit tests for limiter", StatusTodo, 2},
	{7, "Integration tests", StatusTodo, 2},
	{8, "Metrics export", StatusTodo, 3},
	{9, "Dashboard panel", StatusTodo, 4},
	{10, "Rollout behind flag", StatusTodo, 2},
	{11, "Docs", StatusTodo, 3},
	{12, "Load test", StatusTodo, 3},
	{13, "Per-tenant quotas", StatusCancelled, 4},
	{14, "Admin override API", StatusTodo, 2},
	{15, "Alerting", StatusTodo, 3},
	{16, "Review threat model", StatusDone, 2},
	{17, "Rate limit headers", StatusTodo, 2},
	{18, "Client SDK update", StatusTodo, 3},
	{19, "Backfill tenant configs", StatusTodo, 3},
	{20, "Deprecate old limiter", StatusTodo, 4},
}

// fixtureEdges are blocker → blocked, 24 of them.
var fixtureEdges = [][2]int{
	{1, 2}, {1, 3}, {2, 3}, {16, 3},
	{3, 4}, {3, 5}, {3, 17}, {3, 13},
	{4, 6}, {5, 6}, {4, 7}, {6, 7},
	{4, 12}, {6, 12}, {6, 8}, {8, 9},
	{8, 15}, {7, 10}, {5, 10}, {7, 20},
	{10, 20}, {17, 18}, {16, 14}, {5, 19},
}

// FixtureProjectName and FixturePrefix identify the fixture project.
const (
	FixtureProjectName = "fixture"
	FixturePrefix      = "FIX"
)

// SeedFixture creates (or resets) the fixture project and returns it.
//
// It is deterministic: the project is deleted and recreated so repeated calls
// produce identical state. Task ids are allocated by SQLite, so on a fresh
// database fixture task n has id n — which is what the acceptance tests assert.
func (s *Store) SeedFixture(ctx context.Context) (*Project, error) {
	var project *Project
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var existing int64
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM projects WHERE name = ?`, FixtureProjectName).Scan(&existing)
		switch {
		case err == nil:
			if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, existing); err != nil {
				return err
			}
		case err == sql.ErrNoRows:
			// nothing to do
		default:
			return err
		}

		now := nowUTC()
		res, err := tx.ExecContext(ctx,
			`INSERT INTO projects(name, key_prefix, next_task_number, created_at) VALUES (?, ?, 1, ?)`,
			FixtureProjectName, FixturePrefix, now)
		if err != nil {
			return err
		}
		pid, err := res.LastInsertId()
		if err != nil {
			return err
		}

		ids := make(map[int]int64, len(fixtureTasks))
		for _, ft := range fixtureTasks {
			owner := OwnerAgent
			if fixtureHuman[ft.n] {
				owner = OwnerHuman
			}
			res, err := tx.ExecContext(ctx, `
				INSERT INTO tasks(project_id, key, label, notes, status, priority, tags, owner,
				                  x, y, archived, created_at, updated_at)
				VALUES (?, ?, ?, '', ?, ?, '', ?, NULL, NULL, 0, ?, ?)`,
				pid, fmt.Sprintf("%s-%d", FixturePrefix, ft.n), ft.label, ft.status, ft.priority, owner, now, now)
			if err != nil {
				return err
			}
			id, err := res.LastInsertId()
			if err != nil {
				return err
			}
			ids[ft.n] = id
		}
		for _, fe := range fixtureEdges {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO edges(project_id, blocker_id, blocked_id, label, created_at)
				VALUES (?, ?, ?, '', ?)`, pid, ids[fe[0]], ids[fe[1]], now); err != nil {
				return err
			}
		}
		// Keep the key counter consistent with the seeded keys.
		if _, err := tx.ExecContext(ctx,
			`UPDATE projects SET next_task_number = ? WHERE id = ?`, len(fixtureTasks)+1, pid); err != nil {
			return err
		}
		project = &Project{ID: pid, Name: FixtureProjectName, KeyPrefix: FixturePrefix, CreatedAt: now}
		return bumpRevision(ctx, tx)
	})
	if err != nil {
		return nil, err
	}
	return project, nil
}
