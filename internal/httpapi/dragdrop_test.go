package httpapi

import (
	"net/http"
	"strings"
	"testing"

	pb "github.com/codemug/sous/internal/pb/souslet/v1"
)

// ---------- Task 13: drag-and-drop deploy wiring ----------
//
// No browser is available in this suite (see the task brief's own note),
// so these tests stop at what an httptest.Recorder can see: the rendered
// HTML carries the right attributes and the script tag, and the static
// route actually serves dragdrop.js. Whether a real drag gesture in a real
// browser fires the events dragdrop.js listens for is outside what this
// package can exercise - that stays a manual verification per the plan.

// TestModelsPageCardsAreDraggableWithRecipeID guards Step 1: every recipe
// card on Models must be a drag source dragdrop.js's
// `document.querySelectorAll('[data-recipe-id]')` can find, carrying the id
// dragstart hands to dataTransfer.
func TestModelsPageCardsAreDraggableWithRecipeID(t *testing.T) {
	h := newTestServer(t)
	body := send(t, h, http.MethodGet, "/models", "", "").Body.String()
	if !strings.Contains(body, `draggable="true"`) {
		t.Error("expected at least one draggable=\"true\" recipe card on /models")
	}
	// qwen36 is part of the seeded catalog (see weights_test.go's own use of
	// it) and exists regardless of whether any node is configured.
	if !strings.Contains(body, `data-recipe-id="qwen36"`) {
		t.Errorf("expected data-recipe-id=\"qwen36\" on qwen36's card; body:\n%s", body)
	}
}

// TestModelsPageDraggableCardsCoexistWithWeightChips proves Task 13's
// attributes land on the SAME <article> Task 11's per-node weight chips and
// clear-weights buttons already render into, rather than a second
// duplicate card or a clobbered one - the two tasks touch the same loop in
// models.html.
func TestModelsPageDraggableCardsCoexistWithWeightChips(t *testing.T) {
	h, nodes := newTestServerWithNodes(t)
	nodes.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{
		NodeId:            "asus-gx10",
		CachedWeightRepos: []string{"Qwen/Qwen3.6-35B-A3B-FP8"}, // qwen36's model
	})
	body := send(t, h, http.MethodGet, "/models", "", "").Body.String()

	if !strings.Contains(body, `data-recipe-id="qwen36"`) {
		t.Error("expected qwen36's card to carry data-recipe-id")
	}
	if !strings.Contains(body, "asus-gx10: weights cached") {
		t.Error("expected Task 11's resident chip to still render alongside the drag attributes")
	}
	if !strings.Contains(body, `action="/api/weights/qwen36/asus-gx10/delete"`) {
		t.Error("expected Task 11's clear-weights action to still render alongside the drag attributes")
	}
}

// TestNodePageFleetCardsAreDropTargets guards Step 2: every fleet card on
// Node must be a drop target dragdrop.js's
// `document.querySelectorAll('[data-node-id]')` can find, carrying the id
// used to build the deploy URL, and the drop-target class dragdrop.js
// toggles drop-hover on.
func TestNodePageFleetCardsAreDropTargets(t *testing.T) {
	h, nodes := newTestServerWithNodes(t)
	nodes.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{
		NodeId: "asus-gx10", PoolGib: 121.6, ReserveGib: 24,
	})
	body := send(t, h, http.MethodGet, "/", "", "").Body.String()

	if !strings.Contains(body, `data-node-id="asus-gx10"`) {
		t.Errorf("expected data-node-id=\"asus-gx10\" on the fleet card; body:\n%s", body)
	}
	if !strings.Contains(body, "drop-target") {
		t.Error("expected the drop-target class on the fleet card")
	}
	// Task 12's own data-node attribute and is-idle/connected chip must
	// still be there - Task 13 adds to this card, it does not replace it.
	if !strings.Contains(body, `data-node="asus-gx10"`) {
		t.Error("expected Task 12's own data-node attribute to still render")
	}
}

// TestLayoutIncludesDragDropScript guards Step 5: every page renders the
// script tag, since layout.html's "head" template is shared by all of them.
func TestLayoutIncludesDragDropScript(t *testing.T) {
	h := newTestServer(t)
	body := send(t, h, http.MethodGet, "/models", "", "").Body.String()
	if !strings.Contains(body, `<script src="/static/dragdrop.js" defer></script>`) {
		t.Errorf("expected the dragdrop.js script tag on /models; body:\n%s", body)
	}
}

// TestStaticDragDropJSIsServed guards Step 4: the embedded file is actually
// reachable at the URL the script tag and dragdrop.js's own fetch calls
// both assume.
func TestStaticDragDropJSIsServed(t *testing.T) {
	h := newTestServer(t)
	rr := send(t, h, http.MethodGet, "/static/dragdrop.js", "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /static/dragdrop.js: status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"data-recipe-id", "data-node-id", "/api/deploy/", "dragstart", "drop"} {
		if !strings.Contains(body, want) {
			t.Errorf("served dragdrop.js missing expected content %q", want)
		}
	}
	ct := rr.Header().Get("Content-Type")
	if !strings.Contains(strings.ToLower(ct), "javascript") {
		t.Errorf("Content-Type = %q, expected a javascript type", ct)
	}
}
