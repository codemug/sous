package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// A template that fails part way through leaves a half-written page under a
// 200, which reads as success everywhere except the browser. Every screen gets
// checked for its closing markup.
func TestEveryScreenRendersWhole(t *testing.T) {
	h := newTestServer(t)
	post(t, h, "/api/deploy/qwen38", "", "")
	post(t, h, "/api/keys", "application/json", `{"name":"probe"}`)

	for _, path := range []string{"/", "/models", "/larder", "/keys", "/sources", "/model/qwen38", "/model/qwen36/plan"} {
		body := send(t, h, http.MethodGet, path, "", "").Body.String()
		if !strings.Contains(body, "</html>") {
			t.Errorf("%s truncated: …%s", path, tailOf(body, 120))
		}
		if strings.Contains(body, "template:") && strings.Contains(body, "executing") {
			t.Errorf("%s carries a template error", path)
		}
	}
}

// The card grid is the layout the design is built on; a screen still on a table
// has not been ported.
func TestListScreensUseCards(t *testing.T) {
	h := newTestServer(t)
	post(t, h, "/api/deploy/qwen38", "", "")
	post(t, h, "/api/keys", "application/json", `{"name":"probe"}`)

	for _, path := range []string{"/", "/models", "/keys"} {
		body := send(t, h, http.MethodGet, path, "", "").Body.String()
		if !strings.Contains(body, `class="cards`) {
			t.Errorf("%s is not on the card grid", path)
		}
	}
}

// The larder is the one screen where a botched edit hid: a regex matched the
// DOWNLOADS table inside {{with .Fetches}} and replaced that, so the cards only
// appeared while a download was in flight and the old table stayed below.
func TestLarderShowsCardsWithNoDownloadInFlight(t *testing.T) {
	h := newTestServerWithHub(t)
	body := send(t, h, http.MethodGet, "/larder", "", "").Body.String()
	if !strings.Contains(body, `class="cards"`) {
		t.Error("no card grid on the larder with nothing downloading")
	}
	if strings.Contains(body, "<table") && strings.Contains(body, "Referenced by") {
		t.Error("the old larder table is still rendered alongside the cards")
	}
	if !strings.Contains(body, "</html>") {
		t.Error("larder truncated")
	}
}

// A card is a flex column, and a flex item's default min-width is auto - so any
// child wider than the card pushes past its border instead of shrinking. The
// nested .panel was the visible case: a box with its own border, background and
// margin inside another box, breaking out on the right and bottom.
func TestCardsDoNotNestPanels(t *testing.T) {
	h := newTestServerWithHub(t)
	post(t, h, "/api/keys", "application/json", `{"name":"probe"}`)

	for _, path := range []string{"/keys", "/larder"} {
		body := send(t, h, http.MethodGet, path, "", "")
		b := body.Body.String()
		// A .panel inside a .card is the overflow: panels are sized for the
		// top level and their margins escape a card.
		if strings.Contains(b, `class="card `) && strings.Contains(b, `class="panel warn-panel"`) {
			t.Errorf("%s still nests a panel inside a card", path)
		}
		if !strings.Contains(b, "card-danger") && strings.Contains(b, "Delete weights") {
			t.Errorf("%s uses something other than the card-sized danger block", path)
		}
	}
}

// A 476 KiB download rendered as "0.0 GiB" reads as nothing at all - wrong
// twice over, because it is both present on disk and deletable.
func TestSmallEntriesGetAUsefulUnit(t *testing.T) {
	h := newTestServerWithHub(t)
	body := send(t, h, http.MethodGet, "/larder", "", "").Body.String()
	if strings.Contains(body, "0.0 GiB") {
		t.Error(`a small entry still renders as "0.0 GiB"`)
	}
	// The hub fixture writes 4 KiB files, so something must be in KiB.
	if !strings.Contains(body, "KiB") && !strings.Contains(body, "MiB") && !strings.Contains(body, "B<") {
		t.Errorf("no sub-gigabyte unit anywhere on the larder")
	}
}
