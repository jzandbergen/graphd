package webui

import (
	"os/exec"
	"testing"
)

// §11.6 — layout determinism. The layout engine runs in the browser, so this
// test runs the same engine under node. There is no npm and no node_modules:
// layout_test.js loads the vendored UMD builds through a tiny CommonJS shim.
//
// If node is not installed the test is skipped rather than failed — the Go
// build and every other acceptance test are unaffected — but on any machine
// with node it runs and is part of `go test ./...`.
func TestLayoutDeterminism(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping the §11.6 layout determinism check")
	}
	cmd := exec.Command(node, "layout_test.js")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("layout determinism check failed:\n%s", out)
	}
	t.Logf("%s", out)
}
