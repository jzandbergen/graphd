package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"graphd/internal/store"
)

// Listen binds the address. Binding is a separate step from serving so the
// caller can report the *actual* address — with a port of 0 the kernel picks
// one.
func (s *Server) Listen(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	return ln, nil
}

// Serve runs the HTTP server on ln until ctx is cancelled, then shuts down
// gracefully.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	httpSrv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		// Release the SSE streams BEFORE draining. They are held open by design
		// and are not idle connections, so Shutdown's idle sweep never touches
		// them: without this they would consume the entire deadline and the
		// process would exit non-zero (issue #10).
		s.beginShutdown()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			// The signal was received and the listener is closed; a connection
			// that outlived the deadline is not a failure worth a non-zero exit
			// on a Ctrl-C.
			slog.Warn("shutdown deadline exceeded; closed remaining connections",
				"err", err)
		}
		return nil
	}
}

// Server is the HTTP front end: the JSON API, the embedded UI and the SSE
// broadcaster.
type Server struct {
	store *store.Store
	mux   *http.ServeMux
	sse   *Broadcaster
	// shuttingDown is closed when Serve begins a graceful shutdown. It exists to
	// release the SSE streams, which are long-lived by design and would
	// otherwise hold the drain open until the deadline expired (issue #10).
	shuttingDown chan struct{}
	shutdownOnce sync.Once
}

// New builds the router. All routes use Go 1.22+ method+path patterns; there is
// deliberately no third-party router.
func New(st *store.Store) *Server {
	s := &Server{
		store:        st,
		mux:          http.NewServeMux(),
		sse:          NewBroadcaster(),
		shuttingDown: make(chan struct{}),
	}
	s.routes()
	return s
}

// beginShutdown releases every long-lived stream. Idempotent, and safe to call
// from any goroutine.
func (s *Server) beginShutdown() {
	s.shutdownOnce.Do(func() { close(s.shuttingDown) })
}

// Handler returns the root http.Handler with middleware applied.
func (s *Server) Handler() http.Handler {
	return s.logMiddleware(s.mux)
}

// Broadcaster exposes the SSE hub (used by the revision poller and by tests).
func (s *Server) Broadcaster() *Broadcaster { return s.sse }

func (s *Server) routes() {
	m := s.mux

	// UI shells.
	m.HandleFunc("GET /", s.handleRoot)
	m.HandleFunc("GET /canvas", s.handleCanvas)
	m.HandleFunc("GET /board", s.handleBoard)
	m.HandleFunc("GET /assets/", s.handleAssets)

	// Projects.
	m.HandleFunc("GET /api/projects", s.handleListProjects)
	m.HandleFunc("POST /api/projects", s.handleCreateProject)
	m.HandleFunc("GET /api/projects/{pid}", s.handleGetProject)
	m.HandleFunc("DELETE /api/projects/{pid}", s.handleDeleteProject)

	// Per-project reads.
	m.HandleFunc("GET /api/projects/{pid}/graph", s.handleGraph)
	m.HandleFunc("GET /api/projects/{pid}/ready", s.handleReady)
	m.HandleFunc("GET /api/projects/{pid}/events", s.handleEvents)
	m.HandleFunc("GET /api/projects/{pid}/export", s.handleExport)
	m.HandleFunc("POST /api/projects/{pid}/import", s.handleImport)
	m.HandleFunc("GET /api/projects/{pid}/board", s.handleBoardFragment)

	// Tasks.
	m.HandleFunc("POST /api/projects/{pid}/tasks", s.handleCreateTask)
	m.HandleFunc("GET /api/tasks/{tid}", s.handleGetTask)
	m.HandleFunc("PATCH /api/tasks/{tid}", s.handlePatchTask)
	m.HandleFunc("POST /api/tasks/{tid}/archive", s.handleArchiveTask)
	m.HandleFunc("POST /api/tasks/{tid}/restore", s.handleRestoreTask)

	// Edges.
	m.HandleFunc("POST /api/projects/{pid}/edges", s.handleCreateEdge)
	m.HandleFunc("DELETE /api/edges/{eid}", s.handleDeleteEdge)
	m.HandleFunc("PATCH /api/edges/{eid}", s.handlePatchEdge)

	// Positions.
	m.HandleFunc("POST /api/projects/{pid}/positions", s.handlePositions)
	m.HandleFunc("POST /api/projects/{pid}/relayout", s.handleRelayout)
}

// StartRevisionPoller polls meta.revision once per second — a single indexed
// row read — and pushes to SSE clients when it changes. This is the whole
// cross-process change-propagation mechanism: no filesystem watching, no
// socket, no shared daemon (SPEC §3.3).
func (s *Server) StartRevisionPoller(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		last, err := s.store.Revision(ctx)
		if err != nil {
			slog.Error("revision poll: initial read", "err", err)
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				rev, err := s.store.Revision(ctx)
				if err != nil {
					slog.Error("revision poll", "err", err)
					continue
				}
				if rev != last {
					last = rev
					s.sse.Broadcast(rev)
				}
			}
		}
	}()
}

// logMiddleware logs every request at info with method, path, status and
// duration (SPEC §9.5).
func (s *Server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start).Round(time.Millisecond).String(),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying writer so SSE works through the middleware.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Broadcaster fans a revision out to every connected SSE client.
type Broadcaster struct {
	mu      sync.Mutex
	clients map[chan int64]struct{}
}

// NewBroadcaster builds an empty hub.
func NewBroadcaster() *Broadcaster {
	return &Broadcaster{clients: map[chan int64]struct{}{}}
}

// Subscribe registers a client channel.
func (b *Broadcaster) Subscribe() chan int64 {
	ch := make(chan int64, 8)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

// Unsubscribe removes a client channel and closes it.
func (b *Broadcaster) Unsubscribe(ch chan int64) {
	b.mu.Lock()
	if _, ok := b.clients[ch]; ok {
		delete(b.clients, ch)
		close(ch)
	}
	b.mu.Unlock()
}

// Broadcast sends a revision to every client, dropping it for any client whose
// buffer is full (the client refetches current state on the next event, so a
// missed intermediate revision is harmless).
func (b *Broadcaster) Broadcast(rev int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		select {
		case ch <- rev:
		default:
		}
	}
}

// IsLoopback reports whether host (a host:port or bare host) resolves to a
// loopback address. SPEC §10: serve MUST refuse to bind a non-loopback address
// unless --allow-remote is passed, because there is no authentication of any
// kind.
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		// ":7331" binds every interface — not loopback.
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// pathID reads an integer path segment.
func pathID(r *http.Request, name string) (int64, error) {
	raw := r.PathValue(name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, &store.Error{Code: store.CodeNotFound, Message: fmt.Sprintf("%s %q is not a valid id", name, raw)}
	}
	return id, nil
}

// projectID resolves the {pid} path segment to a project id.
func (s *Server) projectID(r *http.Request) (int64, error) {
	pid, err := pathID(r, "pid")
	if err != nil {
		return 0, err
	}
	if _, err := s.store.GetProject(r.Context(), pid); err != nil {
		return 0, err
	}
	return pid, nil
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
