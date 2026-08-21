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

// A box that draws a border and a background needs the inset that goes with
// them. .panel and .wrap both grew a border and never grew the padding, so
// every heading, form and paragraph sat flush against the line.
func TestBoxesCarryTheirPadding(t *testing.T) {
	h := newTestServer(t)
	css := send(t, h, http.MethodGet, "/", "", "").Body.String()

	for _, sel := range []string{".panel{", ".wrap{"} {
		i := strings.Index(css, sel)
		if i < 0 {
			t.Errorf("%s not in the stylesheet", sel)
			continue
		}
		end := strings.Index(css[i:], "}")
		if end < 0 {
			t.Errorf("%s rule unterminated", sel)
			continue
		}
		rule := css[i : i+end]
		if !strings.Contains(rule, "padding") {
			t.Errorf("%s draws a box with no padding; its text will sit on the border", sel)
		}
	}
}

// A FAILED RECORD WAS A DEAD END ON THIS PAGE. The Deploy… link rendered only
// when the recipe was not Deployed, and Deployed() is true for failed and gone
// alike - so a crash-looped model showed Open and nothing else, and the only
// route to acting on it was the orphan list on the Node page.
//
// Neither phase holds any memory, so a plan for one is valid and the button
// belongs there.
func TestFailedModelStillOffersAPlan(t *testing.T) {
	h, rt := newTestServerWithRuntime(t)
	if rr := post(t, h, "/api/deploy/qwen38", "", ""); rr.Code != http.StatusOK {
		t.Fatalf("deploy failed: %d", rr.Code)
	}
	// Kill it behind Sous's back: the record survives, the container does not.
	rt.mu.Lock()
	rt.running = map[string]bool{}
	rt.mu.Unlock()

	body := send(t, h, http.MethodGet, "/models", "", "").Body.String()
	// The id-specific href, not the label: every undeployed recipe on the page
	// renders a Deploy… of its own, so the bare word proves nothing.
	if !strings.Contains(body, "/model/qwen38/plan") {
		t.Error("a model whose container is gone offers no way to redeploy it")
	}
}

// The heading followed the route and the nav; it was the last thing still
// calling this page the Catalog.
func TestModelsPageIsCalledModels(t *testing.T) {
	h := newTestServer(t)
	body := send(t, h, http.MethodGet, "/models", "", "").Body.String()
	if !strings.Contains(body, "<h1>Models</h1>") {
		t.Error("the Models page does not call itself Models")
	}
	// Archived recipes were dimmed by tr.arch td until the tables became cards.
	if !strings.Contains(body, ".card.is-arch{") {
		t.Error("no rule dims an archived card; archived reads at full strength")
	}
}
