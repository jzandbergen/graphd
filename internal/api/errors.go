package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"graphd/internal/store"
)

// APIError is the JSON error body from SPEC §6.2.
type APIError struct {
	Error struct {
		Code    string  `json:"code"`
		Message string  `json:"message"`
		Cycle   []int64 `json:"cycle,omitempty"`
	} `json:"error"`
}

// statusFor maps an error code to its HTTP status (SPEC §6.2).
func statusFor(code string) int {
	switch code {
	case store.CodeNotFound:
		return http.StatusNotFound
	case store.CodeCycleDetected, store.CodeDuplicateEdge, store.CodeDuplicateKey,
		store.CodeDuplicateProject, store.CodeHumanConfirmation:
		return http.StatusConflict
	case store.CodeSelfEdge, store.CodeCrossProjectEdge, store.CodeInvalidStatus,
		store.CodeInvalidPriority, store.CodeInvalidPrefix, store.CodeNameRequired,
		store.CodeConfirmRequired, store.CodeInvalidInput, store.CodeInvalidOwner:
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// writeError renders any error as the coded JSON body. Unknown errors become
// 500s and are logged.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	se := store.AsError(err)
	status := statusFor(se.Code)
	if status >= 500 {
		slog.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	}
	var body APIError
	body.Error.Code = se.Code
	body.Error.Message = se.Message
	body.Error.Cycle = se.Cycle
	writeJSONStatus(w, status, body)
}

func writeJSON(w http.ResponseWriter, v any) {
	writeJSONStatus(w, http.StatusOK, v)
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		slog.Error("encode response", "err", err)
	}
}

// decodeJSON reads a JSON request body, rejecting trailing garbage.
func decodeJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return store.AsError(errors.New("empty request body"))
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &store.Error{Code: store.CodeInvalidInput, Message: "invalid JSON body: " + err.Error()}
	}
	return nil
}
