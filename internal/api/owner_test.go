package api

import (
	"context"
	"testing"

	"graphd/internal/store"
)

// The owner is writable through the same PATCH the UI already uses for every
// other field, and comes back on the task view.
func TestAPIOwnerPatch(t *testing.T) {
	s, st, _ := newTestServer(t)

	rec, body := do(t, s, "PATCH", "/api/tasks/3", map[string]any{"owner": "human"})
	if rec.Code != 200 {
		t.Fatalf("PATCH owner = %d (%v)", rec.Code, body)
	}
	if body["owner"] != "human" {
		t.Errorf("patched owner = %v, want human", body["owner"])
	}
	// The patch response is a full task view, so the derived fields are present —
	// ownership does not suppress any of them.
	if _, ok := body["ready"]; !ok {
		t.Errorf("PATCH response has no ready field: %v", body)
	}

	// And it persists: a fresh read agrees.
	rec, body = do(t, s, "GET", "/api/tasks/3", nil)
	if rec.Code != 200 {
		t.Fatalf("GET task = %d", rec.Code)
	}
	if body["owner"] != "human" {
		t.Errorf("owner after re-read = %v, want human", body["owner"])
	}

	// A garbage owner is refused with the coded body and does not change the row.
	rec, body = do(t, s, "PATCH", "/api/tasks/3", map[string]any{"owner": "robot"})
	if rec.Code != 400 {
		t.Errorf("PATCH bad owner = %d, want 400 (%v)", rec.Code, body)
	}
	if errObj, ok := body["error"].(map[string]any); !ok || errObj["code"] != store.CodeInvalidOwner {
		t.Errorf("bad owner error body = %v, want code %s", body, store.CodeInvalidOwner)
	}
	got, err := st.GetTask(context.Background(), 3)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Owner != store.OwnerHuman {
		t.Errorf("a refused patch changed the row: owner = %q", got.Owner)
	}
}

// Ownership does not move the frontier over the API either: /ready still lists
// the human task, with its owner set. The frontier is unfiltered on purpose —
// it is the human's own queue too.
func TestAPIReadyCarriesOwnerUnfiltered(t *testing.T) {
	s, _, p := newTestServer(t)

	rec, body := do(t, s, "PATCH", "/api/tasks/3", map[string]any{"owner": "human"})
	if rec.Code != 200 {
		t.Fatalf("PATCH owner = %d (%v)", rec.Code, body)
	}

	rec, body = do(t, s, "GET", "/api/projects/"+itoa(p.ID)+"/ready", nil)
	if rec.Code != 200 {
		t.Fatalf("GET ready = %d", rec.Code)
	}
	ready, _ := body["ready"].([]any)
	if len(ready) != 3 {
		t.Fatalf("ready has %d entries, want 3 (the frontier is not filtered by owner)", len(ready))
	}
	first, _ := ready[0].(map[string]any)
	if first["key"] != "FIX-3" {
		t.Errorf("ready[0].key = %v, want FIX-3", first["key"])
	}
	if first["owner"] != "human" {
		t.Errorf("ready[0].owner = %v, want human", first["owner"])
	}
}

// The board fragment marks a human card, so the board shows a plan's manual
// steps without opening anything.
func TestAPIBoardMarksHumanCards(t *testing.T) {
	s, _, p := newTestServer(t)

	rec, body := do(t, s, "PATCH", "/api/tasks/3", map[string]any{"owner": "human"})
	if rec.Code != 200 {
		t.Fatalf("PATCH owner = %d (%v)", rec.Code, body)
	}

	rec, _ = do(t, s, "GET", "/api/projects/"+itoa(p.ID)+"/board", nil)
	if rec.Code != 200 {
		t.Fatalf("GET board fragment = %d", rec.Code)
	}
	html := rec.Body.String()
	if !contains(html, `class="card-human"`) {
		t.Errorf("board fragment has no human marker:\n%s", html)
	}
	// The marker must be on the right card, not merely present: FIX-3's own
	// <article> is the one that should carry it.
	if !cardFor(html, "FIX-3", "card-human") {
		t.Errorf("FIX-3's card carries no human marker:\n%s", html)
	}
	// And it must not be on a card that is not human.
	if cardFor(html, "FIX-4", "card-human") {
		t.Errorf("FIX-4's card carries a human marker but FIX-4 is agent-owned")
	}
	// Exactly two human tasks exist: the fixture seeds one (FIX-10, "Rollout
	// behind flag") and this test marked FIX-3. A marker on anything else is a
	// bug, so the count is asserted rather than bounded.
	if n := count(html, `class="card-human"`); n != 2 {
		t.Errorf("board has %d human markers, want 2 (FIX-10 from the fixture, FIX-3 patched here)", n)
	}
}

// cardFor reports whether the board card whose key is `key` contains `needle`.
// It slices the fragment from the key to the end of that card's </article>, so
// a marker on a neighbouring card cannot satisfy the check.
func cardFor(html, key, needle string) bool {
	i := index(html, ">"+key+"<")
	if i < 0 {
		return false
	}
	rest := html[i:]
	end := index(rest, "</article>")
	if end < 0 {
		return false
	}
	return contains(rest[:end], needle)
}

func index(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func count(s, sub string) int {
	n := 0
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			n++
		}
	}
	return n
}
