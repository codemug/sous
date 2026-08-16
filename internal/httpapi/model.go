package httpapi

import (
	"net/http"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/codemug/sous/internal/engine"
	"github.com/codemug/sous/internal/recipe"
)

// modelPage is everything the drill-down shows for one recipe.
type modelPage struct {
	Recipe recipe.Recipe
	YAML   string

	Deployed bool
	Status   *ModelStatus
	Logs     string
	LogErr   string
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
		m := &modelPage{Recipe: rec}

		// The edit box is YAML, not JSON: it is the format the recipes are
		// published in, and it is the one a person can read a long Notes field
		// in without escaped newlines.
		if b, err := yaml.Marshal(rec); err == nil {
			m.YAML = string(b)
		}

		if n, err := s.nodeStatus(r); err == nil {
			for i := range n.Models {
				if n.Models[i].RecipeID == id {
					m.Status = &n.Models[i]
					m.Deployed = true
					break
				}
			}
		}

		// Logs are best-effort. A recipe that has never been deployed has no
		// container and therefore no logs, and that is not an error worth
		// failing the whole page over.
		if m.Deployed {
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
