package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

const form = "application/x-www-form-urlencoded"

// A form post can arrive from anywhere, so the confirmation is checked on the
// server. The button posting confirm=yes is a courtesy to whoever is
// clicking, not the control itself.
func TestDestructiveFormPostsRequireConfirmation(t *testing.T) {
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

	// Anything other than the sentinel is a refusal, not just an empty field.
	rr = post(t, h, "/model/qwen38/delete", form, "confirm=something-else")
	if rr.Code == http.StatusOK || rr.Code == http.StatusNoContent {
		t.Error("an arbitrary confirm value was accepted")
	}
	if got := ids(t, h); !contains(got, "qwen38") {
		t.Fatal("the recipe went on an arbitrary confirm value")
	}
}

func TestCorrectConfirmationGoesThrough(t *testing.T) {
	h := newTestServer(t)
	rr := post(t, h, "/model/qwen38/delete", form, "confirm=yes")
	if rr.Code >= 400 {
		t.Fatalf("a correct confirmation was refused: %d %s", rr.Code, rr.Body.String())
	}
	if got := ids(t, h); contains(got, "qwen38") {
		t.Error("the recipe survived a confirmed delete")
	}
}

// The sentinel is case-insensitive, matching how the rest of the server
// compares tokens it did not generate itself.
func TestConfirmationIsCaseInsensitive(t *testing.T) {
	h := newTestServer(t)
	rr := post(t, h, "/model/qwen38/delete", form, "confirm=YES")
	if rr.Code >= 400 {
		t.Fatalf("YES was refused: %d %s", rr.Code, rr.Body.String())
	}
}

// Confirmation is a BROWSER control. A script holding the admin token and
// calling the API deliberately is a different act, and making it post a
// sentinel into JSON is theatre rather than safety.
func TestAPICallersAreNotAskedToConfirm(t *testing.T) {
	h := newTestServer(t)
	rr := send(t, h, http.MethodDelete, "/api/recipes/qwen38", "", "")
	if rr.Code >= 400 {
		t.Errorf("an API delete was refused for want of confirmation: %d", rr.Code)
	}
}

// The refusal has to name what it is refusing, or it is a dead end.
func TestRefusalNamesTheTarget(t *testing.T) {
	h := newTestServer(t)
	rr := post(t, h, "/model/qwen38/delete", form, "")
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "qwen38") && !strings.Contains(rr.Body.String(), "qwen38") {
		t.Errorf("the refusal does not name the target: %q %s", loc, rr.Body.String())
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
