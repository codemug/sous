package httpapi

import (
	"net/http"
	"net/http/httptest"
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

	for _, path := range []string{"/", "/models", "/keys", "/sources", "/model/qwen38", "/model/qwen36/plan"} {
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

// A card is a flex column, and a flex item's default min-width is auto - so any
// child wider than the card pushes past its border instead of shrinking. The
// nested .panel was the visible case: a box with its own border, background and
// margin inside another box, breaking out on the right and bottom.
func TestCardsDoNotNestPanels(t *testing.T) {
	h := newTestServerWithHub(t)
	post(t, h, "/api/keys", "application/json", `{"name":"probe"}`)

	for _, path := range []string{"/keys"} {
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
// bounced back with "not confirmed" and nothing was ever removed.
//
// Converting it to the shared partial fixes the path and closes the gap in the
// same change.
func TestModelPageDeleteActuallyDeletes(t *testing.T) {
	h := newTestServer(t)

	form := "application/x-www-form-urlencoded"
	if rr := post(t, h, "/model/qwen38/delete?force=true", form, "confirm=yes"); rr.Code >= 400 {
		t.Fatalf("delete returned %d: %s", rr.Code, rr.Body.String())
	}
	// Asserted against the listing, not the drill-down: an unknown recipe id
	// currently answers 500 rather than 404, which is a separate defect and not
	// something this test should encode as correct.
	if got := ids(t, h); slices.Contains(got, "qwen38") {
		t.Errorf("recipe still present after a confirmed delete: %v", got)
	}
}

// A delete WITHOUT the confirmation must still be refused, or the button is
// decoration - the field can arrive from anywhere on a raw POST, typed name or
// fixed sentinel makes no difference to that requirement.
func TestModelPageDeleteRefusesWithoutConfirmation(t *testing.T) {
	h := newTestServer(t)
	form := "application/x-www-form-urlencoded"
	post(t, h, "/model/qwen38/delete?force=true", form, "confirm=wrong")
	if rr := send(t, h, http.MethodGet, "/model/qwen38", "", ""); rr.Code != http.StatusOK {
		t.Error("a recipe was deleted despite an unconfirmed request")
	}
}

// One partial, one field name, four routes. Each hand-rolled copy is a chance
// for the route and the form to disagree about what confirms it - and they
// already had: larder typed the repo, keys the key id, the model page nothing,
// and the HuggingFace token's clear route did not check at all.
func TestEveryDestructivePathUsesTheSharedConfirmation(t *testing.T) {
	h := newTestServerWithHub(t)
	// A revoke drawer only exists once a key does; a remove-token drawer only
	// exists once a token is configured.
	if rr := post(t, h, "/keys", "application/x-www-form-urlencoded", "name=test"); rr.Code >= 400 {
		t.Fatalf("could not create a key: %d %s", rr.Code, rr.Body.String())
	}
	installToken(t, h)
	for _, path := range []string{"/model/qwen38", "/keys", "/admin"} {
		body := send(t, h, http.MethodGet, path, "", "").Body.String()
		if !strings.Contains(body, `name="confirm" value="yes"`) {
			t.Errorf("%s has no shared confirmation button", path)
		}
	}
}

// hf-token's clear route never called the shared check at all - the drawer
// looked identical to the other three but any POST removed the token.
func TestHFTokenClearRequiresConfirmation(t *testing.T) {
	h := newTestServerWithHub(t)
	installToken(t, h)
	form := "application/x-www-form-urlencoded"
	post(t, h, "/admin/hf-token/clear", form, "")
	if rr := send(t, h, http.MethodGet, "/api/hf-token", "", ""); !strings.Contains(rr.Body.String(), `"configured":true`) {
		t.Error("the token was cleared without confirmation")
	}
	post(t, h, "/admin/hf-token/clear", form, "confirm=yes")
	if rr := send(t, h, http.MethodGet, "/api/hf-token", "", ""); !strings.Contains(rr.Body.String(), `"configured":false`) {
		t.Error("a confirmed clear did not remove the token")
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
		"/admin",        // the page the token is configured on
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

// THE SETTING LIVES ON ADMIN, and the form has to be there.
func TestAdminPageCarriesTheTokenForm(t *testing.T) {
	h := newTestServerWithHub(t)
	body := send(t, h, http.MethodGet, "/admin", "", "").Body.String()
	if !strings.Contains(body, "HuggingFace token") {
		t.Fatal("no token section on the admin page")
	}
	if !strings.Contains(body, `name="token"`) {
		t.Error("the admin page shows the token state but offers no way to set it")
	}
	if !strings.Contains(body, "401") {
		t.Error("the page does not name the failure an operator will actually see")
	}
}

// Admin has to be reachable without knowing the URL.
func TestAdminIsInTheNav(t *testing.T) {
	h := newTestServerWithHub(t)
	body := send(t, h, http.MethodGet, "/", "", "").Body.String()
	if !strings.Contains(body, `href="/admin"`) {
		t.Error("no Admin entry in the nav")
	}
}

// The 0.17.0 path stays working: it was in a release, and something may be
// scripted against it.
func TestTheOldLarderTokenPathStillWorks(t *testing.T) {
	h := newTestServerWithHub(t)
	rr := post(t, h, "/larder/hf-token", "application/x-www-form-urlencoded",
		"token="+testHFToken)
	if rr.Code >= 400 {
		t.Fatalf("the old path returned %d: %s", rr.Code, rr.Body.String())
	}
	body := send(t, h, http.MethodGet, "/api/hf-token", "", "").Body.String()
	if !strings.Contains(body, `"configured":true`) {
		t.Error("the old path did not actually store the token")
	}
}

func setAlias(t *testing.T, h http.Handler, id, names string) *httptest.ResponseRecorder {
	t.Helper()
	return send(t, h, http.MethodPut, "/api/aliases/"+id, "application/json",
		`{"aliases":`+names+`}`)
}

// THE RULE THE FEATURE WAS ASKED FOR, through the API a caller actually uses.
func TestAliasCollisionIsRefusedOverTheAPI(t *testing.T) {
	h := newTestServer(t)
	if rr := setAlias(t, h, "qwen38", `["fast"]`); rr.Code != http.StatusOK {
		t.Fatalf("first set: %d %s", rr.Code, rr.Body.String())
	}
	rr := setAlias(t, h, "qwen36", `["fast"]`)
	// 409: the request is well-formed, it conflicts with current state.
	if rr.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409; body %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "qwen38") {
		t.Errorf("the refusal does not name the holder: %s", rr.Body.String())
	}
}

// ALIASES ARE NOT CONFIGURATION. A recipe travels to other nodes and this
// naming must not travel with it, so it must appear in nothing publishable.
func TestAliasesAreNotInTheRecipe(t *testing.T) {
	h := newTestServer(t)
	if rr := setAlias(t, h, "qwen38", `["housealias"]`); rr.Code != http.StatusOK {
		t.Fatalf("set: %d %s", rr.Code, rr.Body.String())
	}
	for _, path := range []string{"/api/recipes", "/api/recipes/qwen38"} {
		body := send(t, h, http.MethodGet, path, "", "").Body.String()
		if strings.Contains(body, "housealias") {
			t.Errorf("%s carries the alias; it would travel to other nodes", path)
		}
	}
	// It IS on the model page, which is a local view rather than a publishable
	// artifact - that is where an operator manages it.
	page := send(t, h, http.MethodGet, "/model/qwen38", "", "").Body.String()
	if !strings.Contains(page, "housealias") {
		t.Error("the model page does not show the alias it lets you set")
	}
}

// A REDEPLOY IS ROUTINE HERE - changing a flag is an undeploy and a deploy - so
// aliases keyed to the deployment would take every client calling them down
// with each config change.
func TestAliasesSurviveAnUndeploy(t *testing.T) {
	h, rt := newTestServerWithRuntime(t)
	if rr := setAlias(t, h, "qwen38", `["sticky"]`); rr.Code != http.StatusOK {
		t.Fatalf("set: %d %s", rr.Code, rr.Body.String())
	}
	if rr := post(t, h, "/api/deploy/qwen38", "", ""); rr.Code != http.StatusOK {
		t.Fatalf("deploy: %d", rr.Code)
	}
	if rr := post(t, h, "/api/undeploy/qwen38", "", ""); rr.Code >= 400 {
		t.Fatalf("undeploy: %d", rr.Code)
	}
	_ = rt

	body := send(t, h, http.MethodGet, "/api/aliases", "", "").Body.String()
	if !strings.Contains(body, "sticky") {
		t.Errorf("the alias did not survive an undeploy: %s", body)
	}
}

// Clearing frees the name for another model.
func TestClearingAnAliasFreesTheName(t *testing.T) {
	h := newTestServer(t)
	if rr := setAlias(t, h, "qwen38", `["shared"]`); rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}
	if rr := setAlias(t, h, "qwen38", `[]`); rr.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", rr.Code, rr.Body.String())
	}
	if rr := setAlias(t, h, "qwen36", `["shared"]`); rr.Code != http.StatusOK {
		t.Errorf("a cleared alias stayed reserved: %d %s", rr.Code, rr.Body.String())
	}
}

// End to end: an alias set through the API shows up as a model on the gateway.
func TestAliasSetOverTheAPIAppearsInV1Models(t *testing.T) {
	h, _ := newTestServerWithRuntime(t)
	if rr := post(t, h, "/api/deploy/qwen38", "", ""); rr.Code != http.StatusOK {
		t.Fatalf("deploy: %d", rr.Code)
	}
	if rr := setAlias(t, h, "qwen38", `["housefast"]`); rr.Code != http.StatusOK {
		t.Fatalf("set: %d %s", rr.Code, rr.Body.String())
	}
	body := send(t, h, http.MethodGet, "/v1/models", "", "").Body.String()
	if !strings.Contains(body, "housefast") {
		t.Errorf("/v1/models does not list the alias: %s", body)
	}
}

// The cards have to show the alias, or the only place it is visible is the page
// you already had to know to visit.
func TestCardsShowEveryCallableName(t *testing.T) {
	h, _ := newTestServerWithRuntime(t)
	if rr := post(t, h, "/api/deploy/qwen38", "", ""); rr.Code != http.StatusOK {
		t.Fatalf("deploy: %d", rr.Code)
	}
	if rr := setAlias(t, h, "qwen38", `["cardalias"]`); rr.Code != http.StatusOK {
		t.Fatalf("set: %d %s", rr.Code, rr.Body.String())
	}
	for _, path := range []string{"/models", "/"} {
		body := send(t, h, http.MethodGet, path, "", "").Body.String()
		if !strings.Contains(body, "cardalias") {
			t.Errorf("%s does not show the alias on the card", path)
		}
		// The recipe's own name stays alongside it; an alias adds a name, it
		// does not replace the ones the recipe declared.
		if !strings.Contains(body, "qwen38") {
			t.Errorf("%s dropped the recipe's own served_as", path)
		}
	}
}

// A model with no alias still says what to call it, rather than rendering an
// empty strip.
func TestCardsNameAModelWithNoAliases(t *testing.T) {
	h := newTestServer(t)
	body := send(t, h, http.MethodGet, "/models", "", "").Body.String()
	if !strings.Contains(body, `class="names"`) {
		t.Error("no callable names on the cards at all")
	}
}

// The job's own output has to be reachable. Without it, diagnosing a slow
// download means polling byte counts from outside Sous - which is exactly what
// a 29 GiB transfer stalling in bursts forced.
func TestFetchLogsAreReachable(t *testing.T) {
	h := newTestServerWithHub(t)
	rr := send(t, h, http.MethodGet, "/api/fetch/logs?repo=org/thing", "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "lines") {
		t.Errorf("no lines field: %s", rr.Body.String())
	}
}

func TestFetchLogsNeedARepo(t *testing.T) {
	h := newTestServerWithHub(t)
	if rr := send(t, h, http.MethodGet, "/api/fetch/logs", "", ""); rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}
