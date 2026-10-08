package store

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
)

// prefixRe is the key_prefix grammar from SPEC §6.2: ^[A-Z][A-Z0-9]{0,7}$.
var prefixRe = regexp.MustCompile(`^[A-Z][A-Z0-9]{0,7}$`)

// ValidPrefix reports whether a key prefix is well-formed.
func ValidPrefix(p string) bool { return prefixRe.MatchString(p) }

// CreateProject inserts a project. Name is required and both name and prefix
// must be unique.
func (s *Store) CreateProject(ctx context.Context, name, keyPrefix string) (*Project, error) {
	name = strings.TrimSpace(name)
	keyPrefix = strings.TrimSpace(strings.ToUpper(keyPrefix))
	if name == "" {
		return nil, errf(CodeNameRequired, "project name is required")
	}
	if !ValidPrefix(keyPrefix) {
		return nil, errf(CodeInvalidPrefix,
			"key_prefix %q is invalid: must match ^[A-Z][A-Z0-9]{0,7}$", keyPrefix)
	}
	p := &Project{Name: name, KeyPrefix: keyPrefix, CreatedAt: nowUTC()}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO projects(name, key_prefix, next_task_number, created_at) VALUES (?, ?, 1, ?)`,
			p.Name, p.KeyPrefix, p.CreatedAt)
		if err != nil {
			if isUniqueViolation(err) {
				return errf(CodeDuplicateProject,
					"project name or key_prefix %q already exists", name)
			}
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		p.ID = id
		return bumpRevision(ctx, tx)
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// GetProject looks a project up by integer id.
func (s *Store) GetProject(ctx context.Context, id int64) (*Project, error) {
	p := &Project{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, key_prefix, created_at FROM projects WHERE id = ?`, id).
		Scan(&p.ID, &p.Name, &p.KeyPrefix, &p.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, errf(CodeNotFound, "project %d not found", id)
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// ListProjects returns every project, ordered by id.
func (s *Store) ListProjects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.name, p.key_prefix, p.created_at,
		       (SELECT COUNT(*) FROM tasks t WHERE t.project_id = p.id AND t.archived = 0)
		FROM projects p ORDER BY p.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Project{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.KeyPrefix, &p.CreatedAt, &p.TaskCount); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ResolveProject resolves a user-supplied project identifier in the order
// specified by SPEC §8.3: integer id, then exact case-insensitive name, then
// exact case-insensitive key_prefix.
func (s *Store) ResolveProject(ctx context.Context, ref string) (*Project, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errf(CodeNotFound, "no project specified")
	}
	if id, ok := parseID(ref); ok {
		p, err := s.GetProject(ctx, id)
		if err == nil {
			return p, nil
		}
		if e := AsError(err); e.Code != CodeNotFound {
			return nil, err
		}
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, key_prefix, created_at FROM projects
		WHERE name = ? COLLATE NOCASE OR key_prefix = ? COLLATE NOCASE
		ORDER BY id LIMIT 1`, ref, ref)
	p := &Project{}
	err := row.Scan(&p.ID, &p.Name, &p.KeyPrefix, &p.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, errf(CodeNotFound, "project %q not found", ref)
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// DeleteProject hard-deletes a project and (via ON DELETE CASCADE) its tasks
// and edges. This is the one place a hard delete exists; tasks themselves are
// only ever soft-deleted. Requires confirm == the project name (SPEC §6).
func (s *Store) DeleteProject(ctx context.Context, id int64, confirm string) error {
	p, err := s.GetProject(ctx, id)
	if err != nil {
		return err
	}
	if confirm == "" {
		return errf(CodeConfirmRequired,
			"deleting a project requires ?confirm=%s", p.Name)
	}
	if confirm != p.Name {
		return errf(CodeConfirmRequired,
			"confirm %q does not match project name %q", confirm, p.Name)
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id); err != nil {
			return err
		}
		return bumpRevision(ctx, tx)
	})
}

// nextKey allocates the next task key for a project inside an open transaction
// (SPEC §4.2). The counter only ever increases.
func nextKey(ctx context.Context, tx *sql.Tx, projectID int64) (string, error) {
	var prefix string
	var n int64
	err := tx.QueryRowContext(ctx,
		`SELECT key_prefix, next_task_number FROM projects WHERE id = ?`, projectID).
		Scan(&prefix, &n)
	if err == sql.ErrNoRows {
		return "", errf(CodeNotFound, "project %d not found", projectID)
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE projects SET next_task_number = next_task_number + 1 WHERE id = ?`, projectID); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%d", prefix, n), nil
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") || strings.Contains(msg, "constraint failed")
}

// parseID reports whether s is all digits and, if so, its integer value.
func parseID(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int64(r-'0')
	}
	return n, true
}
