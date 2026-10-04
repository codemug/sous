package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/codemug/sous/internal/apikey"
)

// keysPage is the key list plus, exactly once, a freshly minted secret.
type keysPage struct {
	Keys []apikey.Key
	// Fresh is the plaintext of a key created by the request that rendered this
	// page. It exists for one render and is never stored, so a reload loses it -
	// which is the intended behaviour and what the page says.
	Fresh     string
	FreshName string
	// FreshAdmin says the key just shown carries the admin permission, so the
	// page can say what it is at the one moment someone is holding it.
	FreshAdmin bool
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.keys.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

// createKey mints a key and returns the secret ONCE.
func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	permission := r.FormValue("permission")
	var jsonModels []string
	if name == "" {
		var body struct {
			Name       string   `json:"name"`
			Models     []string `json:"models"`
			Permission string   `json:"permission"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err == nil {
			name = strings.TrimSpace(body.Name)
			jsonModels = body.Models
			permission = body.Permission
		}
	}

	// A scope is a comma-separated list from the form, or an array from JSON.
	// Empty means every model, which is what an unscoped key has always been.
	models := splitModels(r.FormValue("models"))
	if len(models) == 0 {
		models = jsonModels
	}

	k, secret, err := s.issueKey(name, permission, models)
	if err != nil {
		if wantsHTML(r) {
			s.redirect(w, r, "/keys", err.Error(), true)
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	if wantsHTML(r) {
		// Carried in the query string for ONE render. It never reaches the
		// store, so a reload loses it - which the page says plainly rather than
		// letting someone assume they can come back for it.
		s.page(w, r, "keys", "API keys", func(d *pageData) error {
			all, err := s.keys.List()
			if err != nil {
				return err
			}
			d.Keys = &keysPage{Keys: all, Fresh: secret, FreshName: k.Name, FreshAdmin: k.Admin()}
			return nil
		})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"key": k, "secret": secret})
}

// issueKey turns what a caller asked for into a key.
//
// Absent or "inference" is the key that has always been issued. "admin" is the
// only thing that produces an admin key, and it goes through CreateAdmin - the
// ordinary Create has no argument that could do it.
func (s *Server) issueKey(name, permission string, models []string) (apikey.Key, string, error) {
	perm, err := apikey.ParsePermission(permission)
	if err != nil {
		return apikey.Key{}, "", err
	}
	if perm != apikey.Admin {
		return s.keys.Create(name, models...)
	}
	// REFUSED, NOT QUIETLY DROPPED. An allowlist on a key that can deploy any
	// model promises a limit nothing enforces, and the caller who sent one
	// believes they asked for a narrower key than they would get.
	if len(models) > 0 {
		return apikey.Key{}, "", errors.New("an admin key reaches every model and the control plane, " +
			"so it cannot carry a model list. Leave models empty, or issue an inference key")
	}
	return s.keys.CreateAdmin(name)
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Confirmed: whoever holds this key starts getting 401 on their next
	// request, and there is no way to tell them beforehand.
	if wantsHTML(r) && !s.requireConfirm(w, r, id, "/keys") {
		return
	}
	var err error
	if r.URL.Query().Get("delete") == "true" || strings.EqualFold(r.FormValue("delete"), "true") {
		err = s.keys.Delete(id)
	} else {
		err = s.keys.Revoke(id)
	}
	if err != nil {
		if wantsHTML(r) {
			s.redirect(w, r, "/keys", err.Error(), true)
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if wantsHTML(r) {
		s.redirect(w, r, "/keys", "", false)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) pageKeys(w http.ResponseWriter, r *http.Request) {
	s.page(w, r, "keys", "API keys", func(d *pageData) error {
		all, err := s.keys.List()
		if err != nil {
			return err
		}
		d.Keys = &keysPage{Keys: all}
		return nil
	})
}

// splitModels parses the form's comma-separated scope.
func splitModels(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
