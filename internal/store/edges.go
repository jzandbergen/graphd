package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// AddEdge creates a blocking edge blocker → blocked. Direction is the direction
// work flows and the direction the arrow is drawn; there is no inversion
// anywhere in the system (SPEC §2.2).
//
// Rejections, in order: cross-project endpoints, self-edge, duplicate, and
// cycle. A broken graph is never persisted — the cycle check runs before the
// INSERT and the whole thing is one transaction.
func (s *Store) AddEdge(ctx context.Context, projectID, blockerID, blockedID int64, label string) (*Edge, error) {
	if blockerID == blockedID {
		return nil, errf(CodeSelfEdge, "a task cannot block itself (task %d)", blockerID)
	}
	var out *Edge
	err := s.tx(ctx, func(tx *sql.Tx) error {
		// Both endpoints must exist and belong to the same project. The
		// denormalised edges.project_id is validated here, never assumed.
		for _, id := range []int64{blockerID, blockedID} {
			var pid int64
			err := tx.QueryRowContext(ctx, `SELECT project_id FROM tasks WHERE id = ?`, id).Scan(&pid)
			if err == sql.ErrNoRows {
				return errf(CodeNotFound, "task %d not found", id)
			}
			if err != nil {
				return err
			}
			if pid != projectID {
				return errf(CodeCrossProjectEdge,
					"task %d belongs to project %d, not project %d", id, pid, projectID)
			}
		}
		g, err := s.loadGraphTx(ctx, tx, projectID)
		if err != nil {
			return err
		}
		if cycle, ok := g.WouldCycle(blockerID, blockedID); ok {
			return &Error{
				Code:    CodeCycleDetected,
				Message: fmt.Sprintf("adding %s -> %s creates a cycle: %s", g.name(blockerID), g.name(blockedID), g.cycleString(cycle)),
				Cycle:   cycle,
			}
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO edges(project_id, blocker_id, blocked_id, label, created_at)
			VALUES (?, ?, ?, ?, ?)`,
			projectID, blockerID, blockedID, label, nowUTC())
		if err != nil {
			if isUniqueViolation(err) {
				return errf(CodeDuplicateEdge,
					"edge %s -> %s already exists", g.name(blockerID), g.name(blockedID))
			}
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		out = &Edge{ID: id, BlockerID: blockerID, BlockedID: blockedID, Label: label}
		// satisfied is derived from the blocker's status.
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, blockerID).Scan(&status); err != nil {
			return err
		}
		out.Satisfied = isTerminal(status)
		return bumpRevision(ctx, tx)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// loadGraphTx builds a Graph from inside an open transaction (used by the
// cycle check so the read and the write are atomic).
func (s *Store) loadGraphTx(ctx context.Context, tx *sql.Tx, projectID int64) (*Graph, error) {
	g := &Graph{
		Tasks: map[int64]*Task{},
		Out:   map[int64][]int64{},
		In:    map[int64][]int64{},
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE project_id = ?`, projectID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		g.Tasks[t.ID] = t
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	erows, err := tx.QueryContext(ctx,
		`SELECT id, blocker_id, blocked_id, label FROM edges WHERE project_id = ?`, projectID)
	if err != nil {
		return nil, err
	}
	defer erows.Close()
	for erows.Next() {
		e := &Edge{}
		if err := erows.Scan(&e.ID, &e.BlockerID, &e.BlockedID, &e.Label); err != nil {
			return nil, err
		}
		g.Edges = append(g.Edges, e)
		g.Out[e.BlockerID] = append(g.Out[e.BlockerID], e.BlockedID)
		g.In[e.BlockedID] = append(g.In[e.BlockedID], e.BlockerID)
	}
	for _, adj := range []map[int64][]int64{g.Out, g.In} {
		for k := range adj {
			sortIDs(adj[k])
		}
	}
	return g, erows.Err()
}

// name renders a task id as its key, falling back to the id.
func (g *Graph) name(id int64) string {
	if t, ok := g.Tasks[id]; ok {
		return t.Key
	}
	return fmt.Sprint(id)
}

// cycleString renders a closed cycle path as "A -> B -> C -> A".
func (g *Graph) cycleString(path []int64) string {
	parts := make([]string, 0, len(path))
	for _, id := range path {
		parts = append(parts, g.name(id))
	}
	return strings.Join(parts, " -> ")
}

// RemoveEdge deletes an edge by its endpoint pair (the MCP remove_edge shape).
func (s *Store) RemoveEdge(ctx context.Context, projectID, blockerID, blockedID int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM edges WHERE project_id = ? AND blocker_id = ? AND blocked_id = ?`,
			projectID, blockerID, blockedID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return errf(CodeNotFound, "edge not found in project %d", projectID)
		}
		return bumpRevision(ctx, tx)
	})
}

// RemoveEdgeByID deletes an edge by id (the HTTP DELETE /api/edges/{eid} shape).
func (s *Store) RemoveEdgeByID(ctx context.Context, id int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM edges WHERE id = ?`, id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return errf(CodeNotFound, "edge %d not found", id)
		}
		return bumpRevision(ctx, tx)
	})
}

// GetEdge fetches an edge by id.
func (s *Store) GetEdge(ctx context.Context, id int64) (*Edge, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, blocker_id, blocked_id, label FROM edges WHERE id = ?`, id)
	e := &Edge{}
	err := row.Scan(&e.ID, &e.BlockerID, &e.BlockedID, &e.Label)
	if err == sql.ErrNoRows {
		return nil, errf(CodeNotFound, "edge %d not found", id)
	}
	if err != nil {
		return nil, err
	}
	var status string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, e.BlockerID).Scan(&status); err == nil {
		e.Satisfied = isTerminal(status)
	}
	return e, nil
}

// UpdateEdgeLabel sets an edge's label. The label is decoration only; no logic
// branches on its contents (SPEC §2.2).
func (s *Store) UpdateEdgeLabel(ctx context.Context, id int64, label string) (*Edge, error) {
	var out *Edge
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE edges SET label = ? WHERE id = ?`, label, id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return errf(CodeNotFound, "edge %d not found", id)
		}
		if err := bumpRevision(ctx, tx); err != nil {
			return err
		}
		row := tx.QueryRowContext(ctx, `SELECT id, blocker_id, blocked_id, label FROM edges WHERE id = ?`, id)
		out = &Edge{}
		if err := row.Scan(&out.ID, &out.BlockerID, &out.BlockedID, &out.Label); err != nil {
			return err
		}
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, out.BlockerID).Scan(&status); err == nil {
			out.Satisfied = isTerminal(status)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
