package httpapi

import (
	"net/http"
	"strings"
)

// Confirm is a two-click confirmation for a destructive action.
type Confirm struct {
	Action string
	Label  string
	Cost   string
}

// confirmed reports whether the request carries the button's confirmation.
//
// SERVER-SIDE, always. These are form posts and a form post can arrive from
// anywhere, so the button in the drawer is a courtesy to whoever is clicking,
// not the control itself - the check has to hold even against a request the
// button never produced.
//
// A FIXED SENTINEL, not a typed name. This used to require the exact recipe
// id, repo, or key id, and the exact match was refusing people on a typo of a
// long or mixed-case name more often than it was catching an accidental
// click - which is the one thing it existed to catch. The two-click drawer
// still requires deliberate action; it no longer requires spelling.
func confirmed(r *http.Request) bool {
	return strings.EqualFold(strings.TrimSpace(r.FormValue("confirm")), "yes")
}

// requireConfirm answers an unconfirmed destructive request.
//
// 409, not 400: the request is well-formed and the caller is not wrong, they
// have simply not confirmed yet - and the difference matters to anything
// deciding whether to retry.
func (s *Server) requireConfirm(w http.ResponseWriter, r *http.Request, id, back string) bool {
	if confirmed(r) {
		return true
	}
	msg := "not confirmed - click the button again to confirm " + id
	if wantsHTML(r) {
		s.redirect(w, r, back, msg, true)
		return false
	}
	writeErr(w, http.StatusConflict, msg)
	return false
}
