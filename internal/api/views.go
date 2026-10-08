package api

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"
)

// The board is server-rendered HTML: a Go template fragment, re-fetched on
// every SSE revision change and swapped wholesale (SPEC §7.4). No client-side
// rendering framework.
var boardTmpl = template.Must(template.New("board").Funcs(template.FuncMap{
	"priorityClass": func(p int) string { return fmt.Sprintf("prio-%d", p) },
	"tags": func(s string) []string {
		if s == "" {
			return nil
		}
		parts := strings.Split(s, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	},
	"truncate": func(n int, s string) string {
		r := []rune(s)
		if len(r) <= n {
			return s
		}
		return string(r[:n-1]) + "\u2026"
	},
}).Parse(boardFragmentHTML))

const boardFragmentHTML = `
<div class="board" data-board>
{{- range . }}
  <section class="column" data-status="{{ .Status }}">
    <header class="column-head">
      <span class="column-title">{{ .Title }}</span>
      <span class="column-count">{{ len .Tasks }}</span>
    </header>
    <div class="column-body" data-drop="{{ .Status }}">
    {{- range .Tasks }}
      <article class="card" draggable="true" data-id="{{ .ID }}" data-status="{{ .Status }}"
               data-priority="{{ .Priority }}">
        <div class="card-top">
          <span class="key">{{ .Key }}</span>
          <span class="chip {{ priorityClass .Priority }}">P{{ .Priority }}</span>
          {{- if .Ready }}<span class="ready-dot" title="ready"></span>{{ end }}
          {{- if gt .Unblocks 0 }}<span class="badge" title="unblocks">{{ .Unblocks }}</span>{{ end }}
        </div>
        <div class="card-label">{{ truncate 90 .Label }}</div>
        {{- if .BlockedByOpen }}
        <div class="card-blocked">blocked by {{ len .BlockedByOpen }}</div>
        {{- end }}
        {{- if .Notes }}<div class="card-notes" title="has notes">&#9636; notes</div>{{ end }}
        {{- if .Archived }}<div class="card-archived">archived</div>{{ end }}
        {{- with tags .Tags }}
        <div class="card-tags">{{ range . }}<span class="tag">{{ . }}</span>{{ end }}</div>
        {{- end }}
      </article>
    {{- end }}
    {{- if not .Tasks }}
      <div class="empty">nothing here</div>
    {{- end }}
    </div>
  </section>
{{- end }}
</div>
`

// serveShell renders the single HTML shell for both views. The shell is
// embedded and served verbatim; the view name is injected so app.js knows which
// view to boot.
func (s *Server) serveShell(w http.ResponseWriter, r *http.Request, view string) {
	html, err := assetString("index.html")
	if err != nil {
		http.Error(w, "ui asset missing: "+err.Error(), http.StatusInternalServerError)
		return
	}
	page := strings.ReplaceAll(html, "{{VIEW}}", view)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(page))
}

// handleAssets serves the embedded static files under /assets/.
func (s *Server) handleAssets(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/assets/")
	if rel == "" {
		http.NotFound(w, r)
		return
	}
	// http.FileServerFS handles content types, ranges and caching headers.
	fsys := assetFS()
	http.StripPrefix("/assets/", http.FileServerFS(fsys)).ServeHTTP(w, r)
}
