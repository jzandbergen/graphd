// Package mcp implements the graphd MCP server: newline-delimited JSON-RPC 2.0
// over stdin/stdout, hand-rolled, no SDK dependency (SPEC §8).
//
// CRITICAL: stdout is the protocol channel. NOTHING may be written to stdout
// that is not a JSON-RPC message. All logging goes to stderr. This is the most
// likely bug in this layer, so it is stated here, in main.go, and in the
// package doc.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"graphd/internal/store"
)

// ProtocolVersion is the MCP revision this server speaks.
const ProtocolVersion = "2024-11-05"

// ServerName and ServerVersion identify this implementation.
const (
	ServerName    = "graphd"
	ServerVersion = "0.1.0"
)

// Server is the stdio MCP server.
type Server struct {
	store *store.Store
}

// New builds an MCP server over an open store.
func New(st *store.Store) *Server { return &Server{store: st} }

// request is one JSON-RPC 2.0 request or notification.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// response is one JSON-RPC 2.0 response. A notification (no id) gets no
// response at all.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is reserved for malformed requests and unknown methods. Tool-level
// failures are NOT protocol errors — they come back as a normal tools/call
// result with isError:true (SPEC §8.1).
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Standard JSON-RPC error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// Serve reads requests line by line from in and writes responses to out until
// in reaches EOF.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	reader := bufio.NewReaderSize(in, 1<<20)
	writer := bufio.NewWriter(out)
	defer writer.Flush()

	for {
		line, err := readLine(reader)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("mcp: read: %w", err)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var req request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			// A parse error has no usable id, so the response carries null.
			s.write(writer, response{
				JSONRPC: "2.0",
				ID:      json.RawMessage("null"),
				Error:   &rpcError{Code: codeParseError, Message: "parse error: " + err.Error()},
			})
			continue
		}
		// Notifications carry no id and get no response.
		if len(req.ID) == 0 {
			s.handleNotification(req)
			continue
		}
		resp := s.handle(ctx, req)
		s.write(writer, resp)
	}
}

// readLine reads one line of arbitrary length (JSON-RPC lines can be large —
// a scaffold_plan payload is not small), without the bufio.Scanner token limit.
func readLine(r *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		chunk, err := r.ReadString('\n')
		sb.WriteString(chunk)
		if err == nil {
			return sb.String(), nil
		}
		if errors.Is(err, io.EOF) {
			if sb.Len() > 0 {
				return sb.String(), nil
			}
			return "", io.EOF
		}
		return "", err
	}
}

func (s *Server) write(w *bufio.Writer, resp response) {
	resp.JSONRPC = "2.0"
	b, err := json.Marshal(resp)
	if err != nil {
		slog.Error("mcp: marshal response", "err", err)
		return
	}
	b = append(b, '\n')
	if _, err := w.Write(b); err != nil {
		slog.Error("mcp: write response", "err", err)
		return
	}
	if err := w.Flush(); err != nil {
		slog.Error("mcp: flush response", "err", err)
	}
}

func (s *Server) handleNotification(req request) {
	switch req.Method {
	case "notifications/initialized":
		slog.Debug("mcp: client initialized")
	default:
		slog.Debug("mcp: ignoring notification", "method", req.Method)
	}
}

func (s *Server) handle(ctx context.Context, req request) response {
	resp := response{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": ServerName, "version": ServerVersion},
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": toolList()}
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &rpcError{Code: codeInvalidParams, Message: "invalid tools/call params: " + err.Error()}
			return resp
		}
		resp.Result = s.callTool(ctx, params.Name, params.Arguments)
	case "notifications/initialized":
		// Tolerated as a request; answers empty.
		resp.Result = map[string]any{}
	default:
		resp.Error = &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}
	}
	return resp
}

// toolContent is one content block in a tools/call result.
type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// toolResult is the tools/call result shape (SPEC §8.1). Tool-level failures
// are normal results with isError:true.
type toolResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError"`
}

// callTool dispatches one tool call. A returned error becomes isError:true with
// the explanation in the text content — never a JSON-RPC error object.
func (s *Server) callTool(ctx context.Context, name string, rawArgs json.RawMessage) toolResult {
	args := map[string]any{}
	if len(rawArgs) > 0 {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return errorResult(fmt.Errorf("invalid arguments: %w", err))
		}
	}
	fn, ok := toolHandlers[name]
	if !ok {
		return errorResult(fmt.Errorf("unknown tool %q", name))
	}
	out, err := fn(ctx, s.store, args)
	if err != nil {
		return errorResult(err)
	}
	// Pretty-printed JSON in a single text content block.
	pretty, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return errorResult(err)
	}
	return toolResult{Content: []toolContent{{Type: "text", Text: string(pretty)}}, IsError: false}
}

func errorResult(err error) toolResult {
	se := store.AsError(err)
	msg := se.Message
	if se.Code != "" && se.Code != "internal" {
		msg = se.Code + ": " + msg
	}
	if se.Cycle != nil {
		msg += fmt.Sprintf("\ncycle: %v", se.Cycle)
	}
	return toolResult{
		Content: []toolContent{{Type: "text", Text: msg}},
		IsError: true,
	}
}
