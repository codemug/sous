package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/codemug/sous/internal/deploy"
	"github.com/codemug/sous/internal/recipe"
)

type pageData struct {
	Title       string
	PoolGiB     float64
	ReserveGiB  float64
	DeployCount int
	Recipes     []recipe.Recipe
	Deployments []deploy.Record
	Message     string
	IsError     bool
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// id validates before anything downstream sees the value.
func id(r *http.Request, w http.ResponseWriter) (string, bool) {
	v := r.PathValue("id")
	if !recipe.ValidID(v) {
		writeErr(w, http.StatusBadRequest, "invalid recipe id")
		return "", false
	}
	return v, true
}

func (s *Server) listRecipes(w http.ResponseWriter, _ *http.Request) {
	rs, err := s.cat.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rs)
}

func (s *Server) listDeployments(w http.ResponseWriter, _ *http.Request) {
	ds, err := s.mgr.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ds)
}

func (s *Server) plan(w http.ResponseWriter, r *http.Request) {
	v, ok := id(r, w)
	if !ok {
		return
	}
	res, err := s.mgr.Plan(v)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) deploy(w http.ResponseWriter, r *http.Request) {
	v, ok := id(r, w)
	if !ok {
		return
	}
	force := r.URL.Query().Get("force") == "true"
	rec, err := s.mgr.Deploy(r.Context(), v, 0, force)
	if err != nil {
		// A capacity refusal is not a server fault and must carry the margin
		// and the way out, not just a failure.
		var ce *deploy.CapacityError
		if errors.As(err, &ce) {
			if wantsHTML(r) {
				s.redirect(w, r, "/", ce.Error(), true)
				return
			}
			writeJSON(w, http.StatusConflict, ce.Result)
			return
		}
		if wantsHTML(r) {
			s.redirect(w, r, "/", err.Error(), true)
			return
		}
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if wantsHTML(r) {
		s.redirect(w, r, "/deployments", "", false)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) undeploy(w http.ResponseWriter, r *http.Request) {
	v, ok := id(r, w)
	if !ok {
		return
	}
	if err := s.mgr.Undeploy(r.Context(), v); err != nil {
		if wantsHTML(r) {
			s.redirect(w, r, "/deployments", err.Error(), true)
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if wantsHTML(r) {
		s.redirect(w, r, "/deployments", "", false)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// wantsHTML distinguishes a form post from an API call, so the same routes
// serve both without a second set of handlers.
func wantsHTML(r *http.Request) bool {
	return r.Header.Get("Content-Type") == "application/x-www-form-urlencoded"
}

func (s *Server) redirect(w http.ResponseWriter, r *http.Request, to, msg string, isErr bool) {
	if msg != "" {
		q := "?msg=" + urlEscape(msg)
		if isErr {
			q += "&err=1"
		}
		to += q
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (s *Server) page(w http.ResponseWriter, r *http.Request, name, title string, fill func(*pageData) error) {
	ds, _ := s.mgr.List()
	d := pageData{
		Title: title, PoolGiB: s.pool, ReserveGiB: s.mgr.Planner.ReserveGiB,
		DeployCount: len(ds), Deployments: ds,
		Message: r.URL.Query().Get("msg"),
		IsError: r.URL.Query().Get("err") == "1",
	}
	if fill != nil {
		if err := fill(&d); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, name, d); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) pageCatalog(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	s.page(w, r, "catalog", "Catalog", func(d *pageData) error {
		rs, err := s.cat.List()
		if err != nil {
			return err
		}
		d.Recipes = rs
		return nil
	})
}

func (s *Server) pageDeployments(w http.ResponseWriter, r *http.Request) {
	s.page(w, r, "deployments", "Deployments", nil)
}
