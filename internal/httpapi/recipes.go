package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/codemug/sous/internal/recipe"
)

// maxRecipeBody caps an upload. Recipes are a few KiB; anything far larger is
// a mistake or an attempt to exhaust memory, and neither should be read.
const maxRecipeBody = 512 << 10

// decodeRecipe accepts JSON or YAML.
//
// Both, because the two entry points differ: the UI posts JSON from a form,
// while IMPORT means pasting or uploading a recipe file, and every recipe file
// this project publishes is YAML. Requiring conversion before import would
// make the feature useless for the artefacts it exists to accept.
//
// YAML is a superset of JSON, so one decoder would technically do - but a
// malformed JSON body then produces a YAML error, and "did not find expected
// node content" is a poor message for a bad JSON field.
func decodeRecipe(r *http.Request) (recipe.Recipe, error) {
	var rec recipe.Recipe
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRecipeBody+1))
	if err != nil {
		return rec, fmt.Errorf("reading body: %w", err)
	}
	if len(body) > maxRecipeBody {
		return rec, fmt.Errorf("recipe body over %d bytes", maxRecipeBody)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return rec, errors.New("empty body")
	}

	ct := r.Header.Get("Content-Type")

	// A browser <form> posts application/x-www-form-urlencoded, so the recipe
	// arrives percent-encoded inside a "body" field rather than as the request
	// body. Without this the UI's import drawer would send YAML that parses as
	// one long key and fails with a message about nothing recognisable.
	if strings.Contains(ct, "x-www-form-urlencoded") {
		vals, err := url.ParseQuery(string(body))
		if err != nil {
			return rec, fmt.Errorf("invalid form body: %w", err)
		}
		field := strings.TrimSpace(vals.Get("body"))
		if field == "" {
			return rec, errors.New("the recipe field was empty")
		}
		body = []byte(field)
		ct = "" // fall through to sniffing below
	}

	if strings.Contains(ct, "json") || strings.HasPrefix(strings.TrimSpace(string(body)), "{") {
		if err := json.Unmarshal(body, &rec); err != nil {
			return rec, fmt.Errorf("invalid JSON: %w", err)
		}
		return rec, nil
	}
	if err := yaml.Unmarshal(body, &rec); err != nil {
		return rec, fmt.Errorf("invalid YAML: %w", err)
	}
	return rec, nil
}

// createRecipe adds a recipe that does not exist yet.
//
// Refusing to overwrite is the whole difference from update. A create that
// silently replaced would make "import" a way to lose a tuned recipe by
// pasting an older copy of it.
func (s *Server) createRecipe(w http.ResponseWriter, r *http.Request) {
	rec, err := decodeRecipe(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !recipe.ValidID(rec.ID) {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("invalid or missing recipe id %q", rec.ID))
		return
	}
	if _, err := s.cat.Get(rec.ID); err == nil {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": fmt.Sprintf("recipe %q already exists", rec.ID),
			"id":    rec.ID,
			"hint":  "PUT /api/recipes/" + rec.ID + " to update it",
		})
		return
	} else if !errors.Is(err, fs.ErrNotExist) {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.cat.Save(rec); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if wantsHTML(r) {
		s.redirect(w, r, "/catalog", "created "+rec.ID, false)
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}

// updateRecipe replaces a recipe and reports what changed.
//
// The diff comes back in the RESPONSE rather than only from the preview
// endpoint, because the useful question after saving is whether the running
// container now disagrees with its recipe. needs_restart answers it, and
// restart_fields says which change is responsible.
func (s *Server) updateRecipe(w http.ResponseWriter, r *http.Request) {
	id, ok := id(r, w)
	if !ok {
		return
	}
	old, err := s.cat.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	rec, err := decodeRecipe(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// The path is authoritative. A body naming a different id would otherwise
	// write one recipe while the caller believed it had edited another.
	if rec.ID == "" {
		rec.ID = id
	}
	if rec.ID != id {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("body id %q does not match path id %q", rec.ID, id))
		return
	}
	if err := s.cat.Save(rec); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	d := recipe.Compare(old, rec)
	deployed := s.isDeployed(id)
	if wantsHTML(r) {
		msg := fmt.Sprintf("updated %s — %d change(s)", id, len(d.Changes))
		if deployed && d.NeedsRestart() {
			msg += "; redeploy to apply " + strings.Join(d.RestartFields(), ", ")
		}
		s.redirect(w, r, "/model/"+id, msg, false)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"recipe":         rec,
		"changes":        d.Changes,
		"needs_restart":  d.NeedsRestart(),
		"restart_fields": d.RestartFields(),
		// Only meaningful together: a restart-forcing change to a recipe that
		// is not running costs nothing to apply.
		"deployed":         deployed,
		"redeploy_advised": deployed && d.NeedsRestart(),
	})
}

// diffRecipe previews a change without saving it.
func (s *Server) diffRecipe(w http.ResponseWriter, r *http.Request) {
	id, ok := id(r, w)
	if !ok {
		return
	}
	old, err := s.cat.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	proposed, err := decodeRecipe(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if proposed.ID == "" {
		proposed.ID = id
	}
	d := recipe.Compare(old, proposed)
	writeJSON(w, http.StatusOK, map[string]any{
		"changes":          d.Changes,
		"needs_restart":    d.NeedsRestart(),
		"restart_fields":   d.RestartFields(),
		"deployed":         s.isDeployed(id),
		"redeploy_advised": s.isDeployed(id) && d.NeedsRestart(),
	})
}

// deleteRecipe removes a recipe, and refuses while it is deployed.
//
// Refusing is the default because the two halves of "delete a running model"
// are separable and only one is reversible. Undeploying stops a service;
// deleting the recipe destroys the configuration that could restart it. Doing
// both silently on one click means a mistyped id takes a model down AND
// removes the means to bring it back.
//
// force does both deliberately, which is the same split deploy and larder
// deletion already use: the dangerous path exists and has to be named.
func (s *Server) deleteRecipe(w http.ResponseWriter, r *http.Request) {
	id, ok := id(r, w)
	if !ok {
		return
	}
	if _, err := s.cat.Get(id); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	// Typed, because a recipe carries measurements taken on this node - load
	// times, KV per token, the attention backend actually chosen - and none of
	// it is recoverable from the image.
	if wantsHTML(r) && !s.requireConfirm(w, r, id, "/model/"+id) {
		return
	}
	force := r.URL.Query().Get("force") == "true"

	if s.isDeployed(id) && !force {
		msg := fmt.Sprintf("%s is deployed; undeploy it first, or pass ?force=true to stop it and delete the recipe", id)
		if wantsHTML(r) {
			s.redirect(w, r, "/model/"+id, msg, true)
			return
		}
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": msg, "id": id, "deployed": true,
		})
		return
	}

	undeployed := false
	if s.isDeployed(id) {
		if err := s.mgr.Undeploy(r.Context(), id); err != nil {
			writeErr(w, http.StatusInternalServerError,
				fmt.Sprintf("stopping %s failed, recipe NOT deleted: %v", id, err))
			return
		}
		undeployed = true
	}
	if err := s.cat.Delete(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if wantsHTML(r) {
		msg := "deleted " + id
		if undeployed {
			msg = "stopped and deleted " + id
		}
		s.redirect(w, r, "/catalog", msg, false)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "undeployed": undeployed})
}

// isDeployed reports whether a recipe currently has a deployment record.
//
// An error listing deployments is treated as DEPLOYED, not as not-deployed.
// The consequence of guessing wrong runs one way: assuming it is idle deletes
// a recipe out from under a running container, while assuming it is busy only
// asks the operator to try again.
func (s *Server) isDeployed(id string) bool {
	ds, err := s.mgr.List()
	if err != nil {
		return true
	}
	for _, d := range ds {
		if d.RecipeID == id {
			return true
		}
	}
	return false
}
