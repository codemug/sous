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
func (s *Server) startFetch(w http.ResponseWriter, r *http.Request) {
	repo := repoFromRequest(r)
	j, err := s.fetch.Start(r.Context(), repo)
	if err != nil {
		if wantsHTML(r) {
			s.redirect(w, r, "/larder", err.Error(), true)
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if wantsHTML(r) {
		s.redirect(w, r, "/larder", "downloading "+j.Repo, false)
		return
	}
	// 202: accepted, not finished. Tens of gigabytes are still to come.
	writeJSON(w, http.StatusAccepted, j)
}

func (s *Server) listFetches(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"fetches": s.fetch.List(r.Context())})
}

// forgetFetch clears a finished job's row.
func (s *Server) forgetFetch(w http.ResponseWriter, r *http.Request) {
	repo := repoFromRequest(r)
	if err := s.fetch.Forget(r.Context(), repo); err != nil {
		if wantsHTML(r) {
			s.redirect(w, r, "/larder", err.Error(), true)
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if wantsHTML(r) {
		s.redirect(w, r, "/larder", "", false)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// fetchView is what the larder page needs about in-flight downloads.
type fetchView struct {
	Jobs []fetch.Job
	// Active is true while any download is running, so the page can poll only
	// while something can still change.
	Active bool
}
