package store

import (
	"errors"
	"fmt"
)

// Error codes. The first block is exactly the table in SPEC.md §6.2. The last
// three are extensions this implementation needed and documents in the README:
//   - duplicate_project: projects.name and projects.key_prefix are UNIQUE, and
//     the spec's table has no code for that collision.
//   - invalid_input: catch-all for malformed request bodies.
//   - invalid_owner: the owner field is an addition beyond the spec
//     (docs/task-owners.md), so its validation code is an addition too.
const (
	CodeNotFound         = "not_found"
	CodeCycleDetected    = "cycle_detected"
	CodeSelfEdge         = "self_edge"
	CodeDuplicateEdge    = "duplicate_edge"
	CodeCrossProjectEdge = "cross_project_edge"
	CodeDuplicateKey     = "duplicate_key"
	CodeInvalidStatus    = "invalid_status"
	CodeInvalidPriority  = "invalid_priority"
	CodeInvalidOwner     = "invalid_owner"
	CodeInvalidPrefix    = "invalid_prefix"
	CodeNameRequired     = "name_required"
	CodeConfirmRequired  = "confirm_required"

	CodeDuplicateProject = "duplicate_project"
	CodeInvalidInput     = "invalid_input"
	// CodeHumanConfirmation is returned when an agent tries to close a task the
	// human owns. It is a guard, not a permission system: every other field on a
	// human task stays writable, and the code exists so a caller gets a clear
	// instruction instead of a silent no-op (docs/task-owners.md §3.2).
	CodeHumanConfirmation = "human_confirmation_required"
)

// Error is a domain error carrying a stable machine code. It marshals to the
// JSON error body in SPEC.md §6.2.
type Error struct {
	Code    string  `json:"code"`
	Message string  `json:"message"`
	Cycle   []int64 `json:"cycle,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// errf builds a coded error.
func errf(code, format string, a ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, a...)}
}

// AsError normalises any error into an *Error so callers always have a code.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: "internal", Message: err.Error()}
}
