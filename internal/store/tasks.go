package store

import (
	"context"
	"database/sql"
	"strings"
)

// TaskPatch is a partial update. Every field is optional; nil means "leave
// unchanged". Positions are *float64 so that 0 is a legal coordinate.
type TaskPatch struct {
	Label    *string
	Notes    *string
	Output   *string
	Status   *string
	Priority *int
	Tags     *string
	X        *float64
	Y        *float64
}

// NewTask describes a task to create.
type NewTask struct {
	Label    string
	Notes    string
	Output   string
	Status   string // defaults to todo
	Priority int    // defaults to 3
	Tags     string
	Key      string // optional explicit key
}

const taskCols = `id, project_id, key, label, notes, output, status, priority, tags, x, y, archived, created_at, updated_at`

func scanTask(sc interface{ Scan(...any) error }) (*Task, error) {
	t := &Task{Inputs: []Input{}}
	var archived int
	var x, y sql.NullFloat64
	if err := sc.Scan(&t.ID, new(int64), &t.Key, &t.Label, &t.Notes, &t.Output, &t.Status,
		&t.Priority, &t.Tags, &x, &y, &archived, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	if x.Valid {
		t.X = &x.Float64
	}
	if y.Valid {
		t.Y = &y.Float64
	}
	t.Archived = archived != 0
	return t, nil
}

// loadTasks reads tasks for a project ordered by id. When includeArchived is
// false, archived rows are omitted (the default for canvas and board).
func (s *Store) loadTasks(ctx context.Context, projectID int64, includeArchived bool) ([]*Task, error) {
	q := `SELECT ` + taskCols + ` FROM tasks WHERE project_id = ?`
	if !includeArchived {
		q += ` AND archived = 0`
	}
	q += ` ORDER BY id`
	rows, err := s.db.QueryContext(ctx, q, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateTask inserts a task, allocating its key inside the same transaction.
func (s *Store) CreateTask(ctx context.Context, projectID int64, in NewTask) (*Task, error) {
	label := strings.TrimSpace(in.Label)
	if label == "" {
		return nil, errf(CodeInvalidInput, "task label is required")
	}
	status := in.Status
	if status == "" {
		status = StatusTodo
	}
	if !ValidStatus(status) {
		return nil, errf(CodeInvalidStatus, "invalid status %q", status)
	}
	priority := in.Priority
	if priority == 0 {
		priority = 3
	}
	if priority < 1 || priority > 5 {
		return nil, errf(CodeInvalidPriority, "priority %d outside 1..5", priority)
	}
	var out *Task
	now := nowUTC()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		key := strings.TrimSpace(in.Key)
		if key == "" {
			k, err := nextKey(ctx, tx, projectID)
			if err != nil {
				return err
			}
			key = k
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO tasks(project_id, key, label, notes, output, status, priority, tags,
			                  x, y, archived, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, 0, ?, ?)`,
			projectID, key, label, in.Notes, in.Output, status, priority, in.Tags, now, now)
		if err != nil {
			if isUniqueViolation(err) {
				return errf(CodeDuplicateKey, "task key %q already exists in project %d", key, projectID)
			}
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		out, err = s.getTaskTx(ctx, tx, id)
		if err != nil {
			return err
		}
		return bumpRevision(ctx, tx)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) getTaskTx(ctx context.Context, tx *sql.Tx, id int64) (*Task, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = ?`, id)
	return scanTask(row)
}

// GetTask fetches a task by integer id.
func (s *Store) GetTask(ctx context.Context, id int64) (*Task, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = ?`, id)
	t, err := scanTask(row)
	if err == sql.ErrNoRows {
		return nil, errf(CodeNotFound, "task %d not found", id)
	}
	return t, err
}

// ResolveTask resolves a task reference per SPEC §4.3: if the string is all
// digits, try integer id first, then fall back to key; otherwise match key
// case-insensitively. A bare key (no prefix) is not a valid reference.
func (s *Store) ResolveTask(ctx context.Context, ref string) (*Task, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errf(CodeNotFound, "no task specified")
	}
	if id, ok := parseID(ref); ok {
		t, err := s.GetTask(ctx, id)
		if err == nil {
			return t, nil
		}
		if e := AsError(err); e.Code != CodeNotFound {
			return nil, err
		}
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+taskCols+` FROM tasks WHERE key = ? COLLATE NOCASE ORDER BY id LIMIT 1`, ref)
	t, err := scanTask(row)
	if err == sql.ErrNoRows {
		return nil, errf(CodeNotFound, "task %q not found", ref)
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// UpdateTask applies a partial patch. Positions are accepted here because this
// is the drag-persist path (SPEC §6); MCP has no position-write path at all.
func (s *Store) UpdateTask(ctx context.Context, id int64, p TaskPatch) (*Task, error) {
	if p.Status != nil && !ValidStatus(*p.Status) {
		return nil, errf(CodeInvalidStatus, "invalid status %q", *p.Status)
	}
	if p.Priority != nil && (*p.Priority < 1 || *p.Priority > 5) {
		return nil, errf(CodeInvalidPriority, "priority %d outside 1..5", *p.Priority)
	}
	var out *Task
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := s.getTaskTx(ctx, tx, id); err != nil {
			if err == sql.ErrNoRows {
				return errf(CodeNotFound, "task %d not found", id)
			}
			return err
		}
		sets := []string{}
		args := []any{}
		add := func(col string, v any) {
			sets = append(sets, col+" = ?")
			args = append(args, v)
		}
		if p.Label != nil {
			add("label", strings.TrimSpace(*p.Label))
		}
		if p.Notes != nil {
			add("notes", *p.Notes)
		}
		if p.Output != nil {
			add("output", *p.Output)
		}
		if p.Status != nil {
			add("status", *p.Status)
		}
		if p.Priority != nil {
			add("priority", *p.Priority)
		}
		if p.Tags != nil {
			add("tags", normalizeTags(*p.Tags))
		}
		if p.X != nil {
			add("x", *p.X)
		}
		if p.Y != nil {
			add("y", *p.Y)
		}
		if len(sets) == 0 {
			var err error
			out, err = s.getTaskTx(ctx, tx, id)
			return err
		}
		add("updated_at", nowUTC())
		args = append(args, id)
		q := `UPDATE tasks SET ` + strings.Join(sets, ", ") + ` WHERE id = ?`
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return err
		}
		var err error
		out, err = s.getTaskTx(ctx, tx, id)
		if err != nil {
			return err
		}
		return bumpRevision(ctx, tx)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ArchiveTask is a soft delete defined entirely in terms of what already
// exists (SPEC §2.5): archived = 1 AND status = cancelled. Because cancelled is
// already terminal, the frontier predicate needs no special case for archived
// tasks — an archived blocker stops blocking automatically.
func (s *Store) ArchiveTask(ctx context.Context, id int64) (*Task, error) {
	var out *Task
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE tasks SET archived = 1, status = 'cancelled', updated_at = ? WHERE id = ?`,
			nowUTC(), id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return errf(CodeNotFound, "task %d not found", id)
		}
		out, err = s.getTaskTx(ctx, tx, id)
		if err != nil {
			return err
		}
		return bumpRevision(ctx, tx)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RestoreTask reverses ArchiveTask: archived = 0, status = todo.
func (s *Store) RestoreTask(ctx context.Context, id int64) (*Task, error) {
	var out *Task
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE tasks SET archived = 0, status = 'todo', updated_at = ? WHERE id = ?`,
			nowUTC(), id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return errf(CodeNotFound, "task %d not found", id)
		}
		out, err = s.getTaskTx(ctx, tx, id)
		if err != nil {
			return err
		}
		return bumpRevision(ctx, tx)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Position is one node's coordinates.
type Position struct {
	ID int64   `json:"id"`
	X  float64 `json:"x"`
	Y  float64 `json:"y"`
}

// SetPositions writes many positions in one transaction. This is the bulk path
// used after auto-layout.
func (s *Store) SetPositions(ctx context.Context, projectID int64, ps []Position) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		now := nowUTC()
		for _, p := range ps {
			res, err := tx.ExecContext(ctx,
				`UPDATE tasks SET x = ?, y = ?, updated_at = ? WHERE id = ? AND project_id = ?`,
				p.X, p.Y, now, p.ID, projectID)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			if n == 0 {
				return errf(CodeNotFound, "task %d not found in project %d", p.ID, projectID)
			}
		}
		return bumpRevision(ctx, tx)
	})
}

// Positions returns every placed task's coordinates for a project, ordered by
// id.
func (s *Store) Positions(ctx context.Context, projectID int64) ([]Position, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, x, y FROM tasks
		WHERE project_id = ? AND x IS NOT NULL AND y IS NOT NULL AND archived = 0
		ORDER BY id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Position{}
	for rows.Next() {
		var p Position
		if err := rows.Scan(&p.ID, &p.X, &p.Y); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// normalizeTags trims whitespace around each comma-separated tag and drops
// empties. The column stays a plain comma-separated string (SPEC §2.6).
func normalizeTags(s string) string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ",")
}
