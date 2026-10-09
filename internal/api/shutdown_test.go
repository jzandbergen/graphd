package api

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Issue #10: `graphd serve` did not cleanly quit on Ctrl-C when a browser had
// the UI open.
//
// The UI holds an SSE stream to /events. That stream is long-lived by design, so
// http.Server.Shutdown's idle-connection sweep never touches it — it is not
// idle. The drain therefore waited for the full 5s deadline, and the error that
// came back was returned from main, so the process exited 1 after appearing to
// hang. With no client attached it exited in milliseconds, which is why it
// looked like an intermittent hang.
//
// This test is the shape of the bug: open a stream, interrupt, and require both
// a prompt exit and a zero exit code.
func TestShutdownWithSSEClient(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	bin := buildBinary(t)
	dir := t.TempDir()
	db := dir + "/graphd.db"
	seedFixture(t, bin, db)

	srv, addr := startServe(t, bin, db)

	// Attach a real SSE stream, exactly as the browser does, and keep it open
	// across the shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", addr+"/api/projects/1/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE connect: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("SSE content type = %q", ct)
	}
	// Read the initial revision event so the handler is provably past its setup
	// and parked in the select loop when the signal lands.
	reader := bufio.NewReader(resp.Body)
	rev0 := readRevision(t, reader)
	if rev0 < 0 {
		t.Fatalf("no initial revision event")
	}

	// stop() asserts a zero exit and a prompt shutdown; before the fix this
	// failed on both counts (exit 1 after ~5s).
	stop(t, srv)

	// The stream must be closed by the server, not left dangling.
	done := make(chan struct{})
	go func() {
		_, _ = reader.ReadString('\n')
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Errorf("the SSE stream was not released on shutdown")
	}
}

// The same guarantee without a client: a bare Ctrl-C must stay prompt and clean.
// This is the control for the test above — it passes both before and after the
// fix, and pins that the fix did not slow the simple path down.
func TestShutdownWithoutClients(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	bin := buildBinary(t)
	dir := t.TempDir()
	db := dir + "/graphd.db"
	seedFixture(t, bin, db)

	srv, _ := startServe(t, bin, db)
	stop(t, srv)
}

// A second signal must not be required to get out, and repeated signals must not
// panic. Exercises beginShutdown's idempotence through the public path.
func TestShutdownTwiceIsSafe(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	bin := buildBinary(t)
	dir := t.TempDir()
	db := dir + "/graphd.db"
	seedFixture(t, bin, db)

	srv, addr := startServe(t, bin, db)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", addr+"/api/projects/1/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE connect: %v", err)
	}
	defer resp.Body.Close()

	// Two signals in quick succession: the second lands after the shutdown
	// channel is already closed.
	_ = srv.Process.Signal(os.Interrupt)
	time.Sleep(50 * time.Millisecond)
	_ = srv.Process.Signal(os.Interrupt)

	done := make(chan error, 1)
	go func() { done <- srv.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve exited non-zero on repeated SIGINT: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = srv.Process.Kill()
		t.Errorf("serve did not exit after repeated SIGINT")
	}
}
