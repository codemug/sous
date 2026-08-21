package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/codemug/sous/internal/fetch"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codemug/sous/internal/catalog"
	"github.com/codemug/sous/internal/deploy"
	"github.com/codemug/sous/internal/larder"
	"github.com/codemug/sous/internal/recipe"
	"github.com/codemug/sous/internal/sources"
)

type pageData struct {
	Title       string
	PoolGiB     float64
	ReserveGiB  float64
	DeployCount int
	Recipes     []recipe.Recipe
	Deployments []deploy.Record
	Larder      []larder.Entry
	LarderTotal string
	Reclaimable string
	Sources     []sources.Source
	Resolved    []catalog.Resolved
	Message     string
	IsError     bool
	Node        *NodeStatus
	Model       *modelPage
	Models      []ModelView
	Pool        *PoolBar
	Plan        *PlanPage
	Keys        *keysPage
	Fetches     *fetchView
	// BaseURL is this server as the BROWSER reached it, so a copyable example
	// works when pasted. Building it from the listen address would print the
	// bind host, which is frequently not the name anyone uses.
	BaseURL string
}

// larderView gathers what the larder needs: the catalog to know what is
// referenced, and the deployment list so a running model's weights can never
// read as stale even mid-edit.
func (s *Server) larderView() ([]larder.Entry, error) {
	recipes, err := s.cat.List()
	if err != nil {
		return nil, err
	}
	deployed := []string{}
	if ds, err := s.mgr.List(); err == nil {
		for _, d := range ds {
			deployed = append(deployed, d.RecipeID)
		}
	}
	return larder.Scan(s.hubDir, recipes, deployed)
}

// humanBytes renders at GiB, which is the unit every measurement in this
// project is quoted in.
func humanBytes(n int64) string {
	const gib = 1024 * 1024 * 1024
	if n >= gib {
		return strconv.FormatFloat(float64(n)/gib, 'f', 2, 64) + " GiB"
	}
	return strconv.FormatFloat(float64(n)/(1024*1024), 'f', 1, 64) + " MiB"
}

func (s *Server) listLarder(w http.ResponseWriter, _ *http.Request) {
	entries, err := s.larderView()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries":           entries,
		"total_bytes":       larder.Total(entries),
		"reclaimable_bytes": larder.Reclaimable(entries),
	})
}

func (s *Server) deleteWeights(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	force := r.URL.Query().Get("force") == "true"

	entries, err := s.larderView()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// TYPED, because this throws away tens of gigabytes that take twenty
	// minutes to fetch again - and unlike a stopped model, nothing brings it
	// back but the network. The repo id is what has to be typed, so the
	// confirmation names the exact thing going.
	if wantsHTML(r) && !s.requireConfirm(w, r, repo, "/larder") {
		return
	}
	freed, err := larder.Delete(s.hubDir, repo, entries, force)
	if err != nil {
		var ge *larder.GuardError
		if errors.As(err, &ge) {
			if wantsHTML(r) {
				s.redirect(w, r, "/larder", ge.Error(), true)
				return
			}
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": ge.Error(), "repo": ge.Repo, "reason": ge.Reason,
			})
			return
		}
		if wantsHTML(r) {
			s.redirect(w, r, "/larder", err.Error(), true)
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if wantsHTML(r) {
		s.redirect(w, r, "/larder", "freed "+humanBytes(freed), false)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"freed_bytes": freed})
}

func (s *Server) pageLarder(w http.ResponseWriter, r *http.Request) {
	s.page(w, r, "larder", "Larder", func(d *pageData) error {
		entries, err := s.larderView()
		if err != nil {
			return err
		}
		d.Larder = entries
		d.LarderTotal = humanBytes(larder.Total(entries))
		d.Reclaimable = humanBytes(larder.Reclaimable(entries))

		jobs := s.fetch.List(r.Context())
		fv := &fetchView{Jobs: jobs}
		for _, j := range jobs {
			if j.Phase == fetch.PhaseDownloading {
				fv.Active = true
				break
			}
		}
		d.Fetches = fv
		return nil
	})
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

// syncRecipes brings the catalog on disk up to date with the seeds compiled
// into the running binary.
//
// This is reachable over HTTP because the alternative was not. Correcting a
// seeded recipe on a node meant deleting files there by hand, which needs shell
// access to a box that may deliberately not grant it - and a node whose whole
// point is being driven remotely should not need a login to accept a recipe its
// own binary already carries.
//
// Without force, recipes an operator has edited are reported as kept and left
// exactly as they are.
func (s *Server) syncRecipes(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "true"

	res, err := s.cat.SyncSeeds(force)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if wantsHTML(r) {
		s.redirect(w, r, "/catalog", fmt.Sprintf(
			"catalog sync: %d added, %d updated, %d kept, %d already current",
			len(res.Added), len(res.Updated), len(res.Kept), len(res.Current)), false)
		return
	}
	writeJSON(w, http.StatusOK, res)
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

	// An explicit port is what makes ADOPTION possible: a service already
	// serving on :8000 with clients pointed at it cannot be migrated to Sous
	// if Sous insists on allocating a fresh port. 0 means "pick a free one",
	// which stays the default.
	port := 0
	if p := r.URL.Query().Get("port"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			writeErr(w, http.StatusBadRequest, "port must be 1-65535")
			return
		}
		port = n
	}

	rec, err := s.mgr.Deploy(r.Context(), v, port, force)
	if err != nil {
		// A capacity refusal is not a server fault and must carry the margin
		// and the way out, not just a failure.
		var ce *deploy.CapacityError
		if errors.As(err, &ce) {
			// THE REFUSAL IS A PAGE, not a sentence. It carries a margin, a
			// list of what to stop and a force path; a query string carries
			// none of those, so an operator was told no and left to work out
			// the rest themselves.
			if wantsHTML(r) {
				s.renderPlanRefusal(w, r, v, ce)
				return
			}
			writeJSON(w, http.StatusConflict, ce.Result)
			return
		}
		if wantsHTML(r) {
			s.redirect(w, r, "/models", err.Error(), true)
			return
		}
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if wantsHTML(r) {
		// Back to the Node page: the thing the operator now wants to watch is
		// the model coming up, which is where the stepper is.
		s.redirect(w, r, "/", "", false)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) undeploy(w http.ResponseWriter, r *http.Request) {
	v, ok := id(r, w)
	if !ok {
		return
	}
	// ASYNCHRONOUS ON PURPOSE. Docker's stop carries a 60 second grace period
	// and a 61 GiB model spends most of it releasing the pool. Doing that here
	// left the browser on a hanging POST for a minute with nothing to show for
	// it, which is indistinguishable from a dead button - so it got clicked
	// again, and the second click raced the first.
	//
	// The work continues in the background; the deployment reports
	// PhaseStopping until its container is actually gone.
	if err := s.mgr.UndeployAsync(v); err != nil {
		if wantsHTML(r) {
			s.redirect(w, r, "/deployments", err.Error(), true)
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if wantsHTML(r) {
		// Back to the Node page: the thing the operator now wants to watch is
		// the model coming up, which is where the stepper is.
		s.redirect(w, r, "/", "", false)
		return
	}
	// 202, not 204: the caller is being told this was accepted, not finished.
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"recipe_id": v, "phase": string(deploy.PhaseStopping),
	})
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
		BaseURL: baseURL(r),
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

// ---------- sources ----------

func (s *Server) listSources(w http.ResponseWriter, _ *http.Request) {
	srcs, err := s.cat.Sources()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, srcs)
}

func (s *Server) addSource(w http.ResponseWriter, r *http.Request) {
	src := sources.Source{
		Name: r.URL.Query().Get("name"),
		URL:  r.URL.Query().Get("url"),
		Ref:  r.URL.Query().Get("ref"),
	}
	if src.Ref == "" {
		src.Ref = "main"
	}
	if err := s.cat.SaveSource(src); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if wantsHTML(r) {
		s.redirect(w, r, "/sources", "added "+src.Name, false)
		return
	}
	writeJSON(w, http.StatusOK, src)
}

// fetchSources updates every mirror and reports what moved. It deploys
// nothing: updating what is on offer and changing what is running are separate
// acts, and conflating them is how a fetch silently alters a live model.
func (s *Server) fetchSources(w http.ResponseWriter, r *http.Request) {
	srcs, err := s.cat.Sources()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type result struct {
		Name    string `json:"name"`
		OldSHA  string `json:"old_sha,omitempty"`
		NewSHA  string `json:"new_sha,omitempty"`
		Changed bool   `json:"changed"`
		Error   string `json:"error,omitempty"`
	}
	out := make([]result, 0, len(srcs))
	for _, src := range srcs {
		res := result{Name: src.Name, OldSHA: src.LastSHA}
		sha, err := s.src.Fetch(r.Context(), src)
		if err != nil {
			res.Error = err.Error()
			out = append(out, res)
			continue
		}
		res.NewSHA = sha
		res.Changed = sha != src.LastSHA
		src.LastSHA = sha
		src.LastFetched = time.Now()
		_ = s.cat.SaveSource(src)
		out = append(out, res)
	}
	if wantsHTML(r) {
		s.redirect(w, r, "/sources", "fetched "+strconv.Itoa(len(out))+" source(s)", false)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) pageSources(w http.ResponseWriter, r *http.Request) {
	s.page(w, r, "sources", "Sources", func(d *pageData) error {
		srcs, err := s.cat.Sources()
		if err != nil {
			return err
		}
		d.Sources = srcs
		res, err := s.cat.Effective(s.src)
		if err == nil {
			d.Resolved = res
		}
		return nil
	})
}

// baseURL reconstructs how the BROWSER reached this server, so a copyable
// example works when pasted elsewhere. r.Host is what the client asked for;
// the listen address would print the bind host, which on this fleet is a
// tailnet IP nobody types.
func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// redirectTo permanently moves an old route.
//
// 301 rather than a second handler: a renamed page should stop existing, not
// quietly keep working - two live names for one page is how they drift.
func redirectTo(to string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, to, http.StatusMovedPermanently)
	}
}

// forgetRecord clears a deployment record whose container is gone.
func (s *Server) forgetRecord(w http.ResponseWriter, r *http.Request) {
	id, ok := id(r, w)
	if !ok {
		return
	}
	if err := s.mgr.ForgetRecord(id); err != nil {
		if wantsHTML(r) {
			s.redirect(w, r, "/", err.Error(), true)
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if wantsHTML(r) {
		s.redirect(w, r, "/", "", false)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// pageModels lists every recipe with whatever is happening to it.
func (s *Server) pageModels(w http.ResponseWriter, r *http.Request) {
	s.page(w, r, "models", "Models", func(d *pageData) error {
		vs, err := s.models(r)
		if err != nil {
			return err
		}
		d.Models = vs
		return nil
	})
}

// pageBody renders a page WITHOUT writing a status, so a caller that has
// already written one - a 409 carrying the plan, say - still gets a whole page
// rather than a bare code.
func (s *Server) pageBody(w http.ResponseWriter, r *http.Request, name, title string, fill func(*pageData) error) {
	ds, _ := s.mgr.List()
	d := pageData{
		Title: title, PoolGiB: s.pool, ReserveGiB: s.mgr.Planner.ReserveGiB,
		DeployCount: len(ds), Deployments: ds,
		BaseURL: baseURL(r),
	}
	if fill != nil {
		if err := fill(&d); err != nil {
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tpl.ExecuteTemplate(w, name, d)
}
