package api

import (
	"io/fs"
	"net/http"
	"time"

	"graphd/internal/webui"
)

// assetFS returns the embedded assets rooted so that "app.js" is at the top
// level, matching the /assets/ URL prefix.
func assetFS() fs.FS {
	sub, err := fs.Sub(webui.FS, "assets")
	if err != nil {
		panic("webui: assets subdirectory missing: " + err.Error())
	}
	return sub
}

func assetString(name string) (string, error) {
	b, err := fs.ReadFile(assetFS(), name)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ---- SSE ----

// handleEvents streams `event: changed / data: {"revision":N}` to the client,
// with a `: ping` comment every 20s to keep the connection alive (SPEC §6.3).
// On reconnect EventSource retries automatically; the client simply refetches
// current state on every event, so there is no Last-Event-ID handling.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if _, err := s.projectID(r); err != nil {
		writeError(w, r, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Subscribe before reading the current revision: that ordering means a write
	// landing between the two is delivered rather than lost. A duplicate event
	// is harmless — the client just refetches.
	ch := s.sse.Subscribe()
	defer s.sse.Unsubscribe(ch)

	// Send the current revision immediately so a fresh client is in sync.
	if rev, err := s.store.Revision(r.Context()); err == nil {
		writeSSE(w, rev)
		flusher.Flush()
	}

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case rev, ok := <-ch:
			if !ok {
				return
			}
			writeSSE(w, rev)
			flusher.Flush()
		case <-ping.C:
			_, _ = w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, rev int64) {
	_, _ = w.Write([]byte("event: changed\ndata: {\"revision\":"))
	_, _ = w.Write([]byte(itoa(rev)))
	_, _ = w.Write([]byte("}\n\n"))
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
