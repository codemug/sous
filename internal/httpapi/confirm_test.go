package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

const form = "application/x-www-form-urlencoded"

// A form post can arrive from anywhere, so the typed confirmation is checked on
// the server. Disabling the button until the text matches is a courtesy to
// whoever is typing, not a control.
func TestDestructiveFormPostsRequireTheTypedName(t *testing.T) {
	h := newTestServer(t)

	// No confirm at all.
	rr := post(t, h, "/model/qwen38/delete", form, "")
	if rr.Code == http.StatusOK || rr.Code == http.StatusNoContent {
		t.Error("a recipe was deleted with no confirmation")
	}
	// Still there.
	if got := ids(t, h); !contains(got, "qwen38") {
		t.Fatal("the recipe went anyway")
	}

	// Wrong confirm.
	rr = post(t, h, "/model/qwen38/delete", form, "confirm=something-else")
	if rr.Code == http.StatusOK || rr.Code == http.StatusNoContent {
		t.Error("a mismatched confirmation was accepted")
	}
	if got := ids(t, h); !contains(got, "qwen38") {
		t.Fatal("the recipe went on a mismatched confirmation")
	}
}

func TestCorrectConfirmationGoesThrough(t *testing.T) {
	h := newTestServer(t)
	rr := post(t, h, "/model/qwen38/delete", form, "confirm=qwen38")
	if rr.Code >= 400 {
		t.Fatalf("a correct confirmation was refused: %d %s", rr.Code, rr.Body.String())
	}
	if got := ids(t, h); contains(got, "qwen38") {
		t.Error("the recipe survived a confirmed delete")
	}
}

// Confirmation is a BROWSER control. A script holding the admin token and
// calling the API deliberately is a different act, and making it type a name
// into JSON is theatre rather than safety.
func TestAPICallersAreNotAskedToType(t *testing.T) {
	h := newTestServer(t)
	rr := send(t, h, http.MethodDelete, "/api/recipes/qwen38", "", "")
	if rr.Code >= 400 {
		t.Errorf("an API delete was refused for want of a typed confirmation: %d", rr.Code)
	}
}

// The refusal has to say what to type, or it is a dead end.
func TestRefusalSaysWhatToType(t *testing.T) {
	h := newTestServer(t)
	rr := post(t, h, "/model/qwen38/delete", form, "")
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "qwen38") && !strings.Contains(rr.Body.String(), "qwen38") {
		t.Errorf("the refusal does not name what to type: %q %s", loc, rr.Body.String())
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
