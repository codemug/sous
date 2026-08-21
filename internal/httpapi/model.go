package httpapi

import (
	"net/http"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/codemug/sous/internal/engine"
)

// modelPage is everything the drill-down shows for one recipe.
//
// IT EMBEDS ModelView, like every other screen. This page was the last one left
// on the pre-redesign ModelStatus, which knew two states - running and drifted -
// and so contradicted the rest of the UI on precisely the point the redesign
// existed to fix: a model that was starting read as "running" here and
// "starting" everywhere else, and a failed one read as "running" too.
type modelPage struct {
	ModelView
	YAML string

	Logs   string
	LogErr string
}

// pageModel renders config, telemetry and logs for a single recipe.
//
// One page rather than three, because the questions an operator actually asks
// interleave: "what is it configured to do", "what did it actually do on boot",
// and "what is it saying now" are the same investigation. Splitting them across
// tabs makes the operator hold state the page could just show.
func (s *Server) pageModel(w http.ResponseWriter, r *http.Request) {
	id, ok := id(r, w)
	if !ok {
		return
	}
	s.page(w, r, "model", "Model", func(d *pageData) error {
		rec, err := s.cat.Get(id)
		if err != nil {
			return err
		}
		m := &modelPage{ModelView: ModelView{Recipe: rec}}

		// The edit box is YAML, not JSON: it is the format the recipes are
		// published in, and it is the one a person can read a long Notes field
		// in without escaped newlines.
		if b, err := yaml.Marshal(rec); err == nil {
			m.YAML = string(b)
		}

		// The same view builder the other pages use, so the phase, the port and
		// the progress here are the ones the pool bar and the cards drew.
		if views, err := s.models(r); err == nil {
			for _, v := range views {
				if v.Recipe.ID == id {
					m.ModelView = v
					break
				}
			}
		}

		// Logs are best-effort. A recipe that has never been deployed has no
		// container and therefore no logs, and that is not an error worth
		// failing the whole page over.
		if m.Deployed() {
			if rc, err := s.mgr.Runtime.Logs(r.Context(), engine.ContainerName(id)); err == nil {
				defer rc.Close()
				buf := make([]byte, 0, 256<<10)
				tmp := make([]byte, 32<<10)
				for len(buf) < 256<<10 {
					n, err := rc.Read(tmp)
					buf = append(buf, tmp[:n]...)
					if err != nil {
						break
					}
				}
				m.Logs = safeText(lastLines(buf, 120))
			} else {
				m.LogErr = err.Error()
			}
		}

		d.Title = rec.ID
		d.Model = m
		return nil
	})
}

// formUpdate and formDelete exist because an HTML form can only issue GET and
// POST. The REST verbs stay for API clients; these are the same handlers
// reached by a path a browser can actually submit to, rather than a hidden
// _method field that every caller then has to remember to honour.
func (s *Server) formUpdate(w http.ResponseWriter, r *http.Request) { s.updateRecipe(w, r) }

func (s *Server) formDelete(w http.ResponseWriter, r *http.Request) {
	// Forms cannot set a query string on submit without a hidden input, so the
	// force flag is read from either place.
	if r.URL.Query().Get("force") != "true" {
		_ = r.ParseForm()
		if strings.EqualFold(r.PostFormValue("force"), "true") {
			q := r.URL.Query()
			q.Set("force", "true")
			r.URL.RawQuery = q.Encode()
		}
	}
	s.deleteRecipe(w, r)
}

// formDeploy lets the model page start a service on a chosen port, which is
// what adoption needs: a service already on :8004 with clients pointed at it
// cannot move to a Sous-allocated port without breaking them.
func (s *Server) formDeploy(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if p := strings.TrimSpace(r.PostFormValue("port")); p != "" {
		q := r.URL.Query()
		q.Set("port", p)
		r.URL.RawQuery = q.Encode()
	}
	s.deploy(w, r)
}
