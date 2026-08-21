package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// The plan is a QUESTION: a GET with no side effects, so it can be linked,
// refreshed, and returned to after stopping something.
func TestPlanPageHasNoSideEffects(t *testing.T) {
	h := newTestServer(t)
	before := send(t, h, http.MethodGet, "/api/deployments", "", "").Body.String()
	rr := send(t, h, http.MethodGet, "/model/qwen38/plan", "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %.200s", rr.Code, rr.Body.String())
	}
	after := send(t, h, http.MethodGet, "/api/deployments", "", "").Body.String()
	if before != after {
		t.Error("asking for the plan changed what is deployed")
	}
	if !strings.Contains(rr.Body.String(), "Projected") {
		t.Error("no projection on the plan page")
	}
}

// A REFUSAL IS A PAGE, NOT A SENTENCE. It carries the margin, what to stop and
// the force path - none of which fits in a query string, which is what it was
// reduced to before.
func TestRefusedDeployRendersThePlanWith409(t *testing.T) {
	h := newTestServer(t)
	// Fill the pool so the next one cannot fit.
	if rr := post(t, h, "/api/deploy/qwen38", "", ""); rr.Code != http.StatusOK {
		t.Fatalf("first deploy: %d", rr.Code)
	}

	// A BROWSER is a form post, not an Accept header: httpapi.wantsHTML keys
	// on Content-Type because that is what separates a form from an API call.
	// (The auth package has a same-named function that checks Accept for a
	// different question.)
	rr := post(t, h, "/api/deploy/qwen36", "application/x-www-form-urlencoded", "")

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"Stop one of these", "Margin", "Deploy anyway"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal does not offer %q", want)
		}
	}
	// The old behaviour: a redirect carrying the whole answer as a string.
	if strings.Contains(body, "err=1") {
		t.Error("the refusal is still a query-string banner")
	}
}

// A script must keep getting the structured refusal it can act on.
func TestScriptedRefusalStaysJSON(t *testing.T) {
	h := newTestServer(t)
	if rr := post(t, h, "/api/deploy/qwen38", "", ""); rr.Code != http.StatusOK {
		t.Fatalf("first deploy: %d", rr.Code)
	}
	rr := post(t, h, "/api/deploy/qwen36", "", "")
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "must_free") {
		t.Errorf("JSON refusal lost must_free: %s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "<html") {
		t.Error("a script was sent HTML")
	}
}

// Force stays reachable, because the reserve is deliberately conservative and
// the operator sometimes knows better - but it is typed, not clicked.
func TestForceIsOfferedButRequiresTyping(t *testing.T) {
	h := newTestServer(t)
	body := send(t, h, http.MethodGet, "/model/qwen36/plan", "", "").Body.String()
	if strings.Contains(body, "Deploy anyway") {
		if !strings.Contains(body, `name="confirm"`) {
			t.Error("force is offered with no typed confirmation")
		}
		if !strings.Contains(body, "OOM") {
			t.Error("the force copy does not say what the risk is")
		}
	}
}
