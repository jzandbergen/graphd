package webui

import (
	"os/exec"
	"testing"
)

// runNodeScript runs one of the node-based front-end checks, skipping (rather
// than failing) when node is not installed. The Go build and every other
// acceptance test are unaffected; on any machine with node these run as part of
// `go test ./...`.
func runNodeScript(t *testing.T, script, what string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not installed; skipping the %s check", what)
	}
	out, err := exec.Command(node, script).CombinedOutput()
	if err != nil {
		t.Fatalf("%s check failed:\n%s", what, out)
	}
	t.Logf("%s", out)
}

// §11.6 — layout determinism. The layout engine runs in the browser, so this
// test runs the same engine under node. There is no npm and no node_modules:
// layout_test.js loads the vendored UMD builds through a tiny CommonJS shim.
func TestLayoutDeterminism(t *testing.T) {
	runNodeScript(t, "layout_test.js", "§11.6 layout determinism")
}

// Markdown rendering, including the XSS cases. Notes are stored as plain text
// and rendered as a view; this proves the renderer neutralises raw HTML and
// filters link schemes rather than executing them.
func TestMarkdownRendering(t *testing.T) {
	runNodeScript(t, "markdown_test.js", "markdown rendering")
}
