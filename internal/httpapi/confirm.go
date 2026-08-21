package httpapi

import (
	"net/http"
	"strings"
)

// Confirm is a typed confirmation for a destructive action.
type Confirm struct {
	Action string
	ID     string
	Label  string
	Cost   string
}

// confirmed reports whether the request carries a matching typed confirmation.
//
// SERVER-SIDE, always. The page disables its button until the text matches, but
// that is a courtesy to whoever is typing - these are form posts, a form post
// can arrive from anywhere, and a check that only exists in the browser is not
// a check.
func confirmed(r *http.Request, want string) bool {
	got := strings.TrimSpace(r.FormValue("confirm"))
	return got != "" && strings.EqualFold(got, want)
}

// requireConfirm answers an unconfirmed destructive request.
//
// 409, not 400: the request is well-formed and the caller is not wrong, they
// have simply not confirmed yet - and the difference matters to anything
// deciding whether to retry.
func (s *Server) requireConfirm(w http.ResponseWriter, r *http.Request, id, back string) bool {
	if confirmed(r, id) {
		return true
	}
	msg := "type " + id + " to confirm this"
	if wantsHTML(r) {
		s.redirect(w, r, back, msg, true)
		return false
	}
	writeErr(w, http.StatusConflict, msg)
	return false
}
