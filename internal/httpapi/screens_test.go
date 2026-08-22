package httpapi

import (
	"net/http"
	"net/url"
	"slices"
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

// THE MODEL PAGE'S DELETE WAS ALREADY BROKEN, not merely unsafe. Its form
// posted a `force` checkbox and no `confirm` field at all, while recipes.go
// guards the route with requireConfirm - so every browser delete from this page
// bounced back with "type <id> to confirm this" and nothing was ever removed.
//
// Converting it to the shared partial fixes the path and closes the gap in the
// same change.
func TestModelPageDeleteActuallyDeletes(t *testing.T) {
	h := newTestServer(t)

	form := "application/x-www-form-urlencoded"
	if rr := post(t, h, "/model/qwen38/delete?force=true", form, "confirm=qwen38"); rr.Code >= 400 {
		t.Fatalf("delete returned %d: %s", rr.Code, rr.Body.String())
	}
	// Asserted against the listing, not the drill-down: an unknown recipe id
	// currently answers 500 rather than 404, which is a separate defect and not
	// something this test should encode as correct.
	if got := ids(t, h); slices.Contains(got, "qwen38") {
		t.Errorf("recipe still present after a confirmed delete: %v", got)
	}
}

// A delete WITHOUT the typed confirmation must still be refused, or the partial
// is decoration.
func TestModelPageDeleteRefusesWithoutTheTypedText(t *testing.T) {
	h := newTestServer(t)
	form := "application/x-www-form-urlencoded"
	post(t, h, "/model/qwen38/delete?force=true", form, "confirm=wrong")
	if rr := send(t, h, http.MethodGet, "/model/qwen38", "", ""); rr.Code != http.StatusOK {
		t.Error("a recipe was deleted despite a mismatched confirmation")
	}
}

// One partial, one field name, four routes. Each hand-rolled copy is a chance
// for the route and the form to disagree about what is being typed - and they
// already had: larder typed the repo, keys the key id, the model page nothing.
func TestEveryDestructivePathUsesTheSharedConfirmation(t *testing.T) {
	h := newTestServerWithHub(t)
	// A revoke drawer only exists once a key does.
	if rr := post(t, h, "/keys", "application/x-www-form-urlencoded", "name=test"); rr.Code >= 400 {
		t.Fatalf("could not create a key: %d %s", rr.Code, rr.Body.String())
	}
	for _, c := range []struct{ path, id string }{
		{"/model/qwen38", "qwen38"},
		{"/keys", ""},
		{"/larder", ""},
	} {
		body := send(t, h, http.MethodGet, c.path, "", "").Body.String()
		if !strings.Contains(body, `name="confirm"`) {
			t.Errorf("%s has no typed confirmation", c.path)
		}
		// The partial gives every confirmation input an id derived from the
		// target; a hand-rolled copy has no id at all.
		if !strings.Contains(body, `id="c-`) {
			t.Errorf("%s still hand-rolls its confirmation instead of calling the partial", c.path)
		}
	}
}

// A FLASH MESSAGE DIED IN A REDIRECT HOP. Creating, deleting and syncing all
// redirected to /catalog with ?msg=…, and /catalog is a 301 to /models that
// dropped the query - so the action worked and the page said nothing about it.
func TestCatalogRedirectKeepsTheMessage(t *testing.T) {
	h := newTestServer(t)
	rr := send(t, h, http.MethodGet, "/catalog?msg=created+qwen38", "", "")
	if rr.Code != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want 301", rr.Code)
	}
	loc := rr.Header().Get("Location")
	if !strings.HasPrefix(loc, "/models") {
		t.Errorf("Location = %q, want /models", loc)
	}
	if !strings.Contains(loc, "msg=created") {
		t.Errorf("Location = %q dropped the message; the confirmation is lost", loc)
	}
}

// And the handlers no longer take that hop at all.
func TestRecipeCreationLandsOnModelsWithItsMessage(t *testing.T) {
	h := newTestServer(t)
	yaml := "id: made-up\nkind: vllm\nmodality: text\n" +
		"image: vllm/vllm-openai:latest\nmodel: org/Model-Name\n"
	rr := post(t, h, "/api/recipes", "application/x-www-form-urlencoded",
		"body="+url.QueryEscape(yaml))
	loc := rr.Header().Get("Location")
	if !strings.HasPrefix(loc, "/models") {
		t.Errorf("created a recipe and landed on %q, want /models", loc)
	}
	if !strings.Contains(loc, "msg=") {
		t.Errorf("Location = %q carries no confirmation", loc)
	}
}

// "The pool is entirely free" sat directly beneath a list of records that had
// gone wrong. Orphans hold no memory, so Residents is zero - but the pool is
// not what the operator is looking at.
func TestEmptyStateDoesNotClaimAFreePoolBesideOrphans(t *testing.T) {
	h, rt := newTestServerWithRuntime(t)
	if rr := post(t, h, "/api/deploy/qwen38", "", ""); rr.Code != http.StatusOK {
		t.Fatalf("deploy failed: %d", rr.Code)
	}
	rt.mu.Lock()
	rt.running = map[string]bool{}
	rt.mu.Unlock()

	body := send(t, h, http.MethodGet, "/", "", "").Body.String()
	if strings.Contains(body, "entirely free") {
		t.Error("the page claims an entirely free pool while listing orphaned records")
	}
	if !strings.Contains(body, "Nothing is holding memory") {
		t.Error("no empty state at all for a node whose only records are orphans")
	}
}

// Nine-plus recipes as equal cards with no way to narrow. The filters are links
// carrying ?filter=, so each is a real URL - bookmarkable, and no JS.
func TestModelsPageFilters(t *testing.T) {
	h, rt := newTestServerWithRuntime(t)
	if rr := post(t, h, "/api/deploy/qwen38", "", ""); rr.Code != http.StatusOK {
		t.Fatalf("deploy failed: %d", rr.Code)
	}
	_ = rt

	all := send(t, h, http.MethodGet, "/models", "", "").Body.String()
	if !strings.Contains(all, `href="/models?filter=pool"`) {
		t.Fatal("no filter links on the models page")
	}

	pool := send(t, h, http.MethodGet, "/models?filter=pool", "", "").Body.String()
	if !strings.Contains(pool, "qwen38") {
		t.Error("the deployed model is missing from the in-the-pool filter")
	}
	// A recipe that is not deployed must not survive that filter. qwen36 is
	// seeded and nothing deployed it.
	if strings.Contains(pool, `/model/qwen36"`) {
		t.Error("an undeployed recipe appears under in-the-pool")
	}

	// A bad filter shows everything rather than an empty page, which would read
	// as "there are no models" for what is really a mistyped URL.
	bad := send(t, h, http.MethodGet, "/models?filter=nonsense", "", "").Body.String()
	if !strings.Contains(bad, "qwen38") {
		t.Error("an unknown filter emptied the page instead of falling back to all")
	}
}

const testHFToken = "hf_thisisthesecrettokenvalue"

func installToken(t *testing.T, h http.Handler) {
	t.Helper()
	rr := send(t, h, http.MethodPut, "/api/hf-token", "application/json",
		`{"token":"`+testHFToken+`"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("installing the token: %d %s", rr.Code, rr.Body.String())
	}
}

// THE ONE INVARIANT THAT JUSTIFIES THE WHOLE DESIGN. Recipes are published to
// git - the catalog in this repo is generated from them - so a token that
// reached recipe.Env would be a credential in version control the moment
// anyone regenerated the catalog. It is injected at container-creation time
// instead, and nothing that can be printed, diffed or committed ever sees it.
func TestHFTokenNeverAppearsInAnythingPublishable(t *testing.T) {
	h := newTestServerWithHub(t)
	installToken(t, h)

	for _, path := range []string{
		"/api/recipes",  // the machine-readable catalog
		"/models",       // the page listing them
		"/model/qwen38", // the drill-down, which renders the YAML
		"/larder",       // the page the token is configured on
		"/api/status",   // what a monitor scrapes
		"/api/hf-token", // the token's own endpoint
	} {
		body := send(t, h, http.MethodGet, path, "", "").Body.String()
		if strings.Contains(body, testHFToken) {
			t.Errorf("%s leaks the HuggingFace token", path)
		}
	}
}

// The endpoint reports WHETHER a token is installed and which one, never what
// it is. There is no one-time reveal because Sous did not mint this.
func TestHFTokenEndpointReturnsAHintNotTheToken(t *testing.T) {
	h := newTestServerWithHub(t)
	installToken(t, h)

	body := send(t, h, http.MethodGet, "/api/hf-token", "", "").Body.String()
	if !strings.Contains(body, `"configured":true`) {
		t.Errorf("token not reported as configured: %s", body)
	}
	if strings.Contains(body, testHFToken) {
		t.Fatal("the endpoint returned the token itself")
	}
	// The last four characters, so two tokens can be told apart.
	if !strings.Contains(body, "alue") {
		t.Errorf("no usable hint in %s", body)
	}
}

// Validation happens at the boundary, not inside a download container ten
// minutes later.
func TestHFTokenRejectsAValueThatIsNotAToken(t *testing.T) {
	h := newTestServerWithHub(t)
	rr := send(t, h, http.MethodPut, "/api/hf-token", "application/json",
		`{"token":"my-username"}`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "hf_") {
		t.Errorf("the error does not say what a token looks like: %s", rr.Body.String())
	}
}

func TestHFTokenCanBeCleared(t *testing.T) {
	h := newTestServerWithHub(t)
	installToken(t, h)
	if rr := send(t, h, http.MethodDelete, "/api/hf-token", "", ""); rr.Code != http.StatusOK {
		t.Fatalf("clear returned %d", rr.Code)
	}
	body := send(t, h, http.MethodGet, "/api/hf-token", "", "").Body.String()
	if !strings.Contains(body, `"configured":false`) {
		t.Errorf("still configured after a clear: %s", body)
	}
}

// The panel has to say what a missing token actually costs, because "401" on a
// repo whose agreement you accepted in a browser is genuinely confusing.
func TestLarderExplainsWhyAGatedRepoStillFails(t *testing.T) {
	h := newTestServerWithHub(t)
	body := send(t, h, http.MethodGet, "/larder", "", "").Body.String()
	if !strings.Contains(body, "HuggingFace token") {
		t.Fatal("no token panel on the larder page")
	}
	if !strings.Contains(body, "401") {
		t.Error("the panel does not name the failure an operator will actually see")
	}
}
