package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/codemug/sous/internal/fetch"
)

// repoFromRequest reads the repo id from a form field or a JSON body, so the
// dashboard form and a script hit the same handler.
func repoFromRequest(r *http.Request) string {
	if v := strings.TrimSpace(r.FormValue("repo")); v != "" {
		return v
	}
	var body struct {
		Repo  string `json:"repo"`
		Model string `json:"model"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err == nil {
		if body.Repo != "" {
			return strings.TrimSpace(body.Repo)
		}
		// "model" accepted too: it is what the thing is called everywhere else
		// in this API, and a caller should not have to remember which noun a
		// particular endpoint chose.
		return strings.TrimSpace(body.Model)
	}
	return ""
}

// startFetch begins a download and returns immediately.
//
// The wantsHTML branch redirects to /models, not the retired /larder page
// (removed in Task 14 of the multi-node plan) - no page currently posts a
// browser form here (the old "Download a model" form lived only on
// larder.html), so this is a backstop against a raw form-encoded POST
// finding a 404 rather than something a real page still triggers.
func (s *Server) startFetch(w http.ResponseWriter, r *http.Request) {
	repo := repoFromRequest(r)
	j, err := s.fetch.Start(r.Context(), repo)
	if err != nil {
		if wantsHTML(r) {
			s.redirect(w, r, "/models", err.Error(), true)
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if wantsHTML(r) {
		s.redirect(w, r, "/models", "downloading "+j.Repo, false)
		return
	}
	// 202: accepted, not finished. Tens of gigabytes are still to come.
	writeJSON(w, http.StatusAccepted, j)
}

func (s *Server) listFetches(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"fetches": s.fetch.List(r.Context())})
}

// forgetFetch clears a finished job's row. See startFetch's doc comment for
// why the wantsHTML branch redirects to /models rather than /larder.
func (s *Server) forgetFetch(w http.ResponseWriter, r *http.Request) {
	repo := repoFromRequest(r)
	if err := s.fetch.Forget(r.Context(), repo); err != nil {
		if wantsHTML(r) {
			s.redirect(w, r, "/models", err.Error(), true)
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if wantsHTML(r) {
		s.redirect(w, r, "/models", "", false)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// fetchView is what a page needs to show about in-flight downloads (once
// only larder.html, since removed in Task 14 of the multi-node plan; no
// current page renders one, but the /api/fetch JSON listing still works).
type fetchView struct {
	Jobs []fetch.Job
	// Active is true while any download is running, so the page can poll only
	// while something can still change.
	Active bool
}
