package httpapi

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/codemug/sous/internal/auth"
)

// loginPage is what the form template renders against.
type loginPage struct {
	Next  string
	Error string
}

// safeNext keeps a post-login redirect on this origin.
//
// An unchecked ?next= is an open redirect: a crafted link to this panel would
// send an operator who has just typed their password onto someone else's page,
// and the address bar shows the real host right up until the bounce.
func safeNext(raw string) string {
	if raw == "" {
		return "/"
	}
	u, err := url.Parse(raw)
	// Anything with a host, a scheme, or a leading "//" is off-origin.
	if err != nil || u.IsAbs() || u.Host != "" ||
		!strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return "/"
	}
	return raw
}

func (s *Server) pageLogin(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))

	// Already signed in: send them where they were going rather than showing a
	// form that would immediately be redundant.
	if s.guard.Authorized(r) {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	s.renderLogin(w, r, loginPage{Next: next})
}

func (s *Server) doLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderLogin(w, r, loginPage{Next: "/", Error: "Could not read that form."})
		return
	}
	next := safeNext(r.PostFormValue("next"))
	user := strings.TrimSpace(r.PostFormValue("username"))
	pass := r.PostFormValue("password")

	if !s.guard.CheckPassword(user, pass) {
		// One message for both a wrong username and a wrong password. Saying
		// which was wrong confirms whether a username exists, and there is no
		// usability gain here - there is exactly one account.
		//
		// 401 rather than 200 so the failure is visible to anything reading
		// status codes, and the form is returned in the same response so the
		// operator does not lose their place.
		w.WriteHeader(http.StatusUnauthorized)
		s.renderLogin(w, r, loginPage{Next: next, Error: "Wrong username or password."})
		return
	}
	s.guard.SetSessionCookie(w, r, user)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) doLogout(w http.ResponseWriter, r *http.Request) {
	auth.ClearSessionCookie(w, r)
	http.Redirect(w, r, auth.LoginPath, http.StatusSeeOther)
}

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, d loginPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// A cached login form can render a stale error, and a cached one served
	// after logout looks like the session survived.
	w.Header().Set("Cache-Control", "no-store")
	if err := s.tpl.ExecuteTemplate(w, "login", d); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
