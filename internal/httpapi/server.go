// Package httpapi exposes the UI and the JSON API on one listener.
//
// Every handler that takes an id validates it before touching a path, a
// container name or the store. Ids reach the filesystem and the Docker API, so
// rejection has to happen at the edge rather than being relied on downstream.
package httpapi

import (
	"github.com/codemug/sous/internal/apikey"
	"github.com/codemug/sous/internal/fetch"
	"github.com/codemug/sous/internal/gateway"
	"html/template"
	"net/http"

	"github.com/codemug/sous/internal/auth"
	"github.com/codemug/sous/internal/catalog"
	"github.com/codemug/sous/internal/deploy"
	"github.com/codemug/sous/internal/sources"
	"github.com/codemug/sous/internal/ui"
)

type Server struct {
	mgr   *deploy.Manager
	cat   *catalog.Catalog
	keys  *apikey.Manager
	fetch *fetch.Manager
	tpl   *template.Template

	pool float64
	// hubDir is the HuggingFace cache under the model directory. The larder
	// scans it per request: the disk is the source of truth, and caching it
	// would drift from reality the first time anything is deleted by hand.
	hubDir string

	// src mirrors recipe repositories. Fetch is always explicit: nothing is
	// pulled on a timer and nothing is ever deployed by a fetch.
	src *sources.Manager

	mux *http.ServeMux

	// guard is held so the login handlers can verify a password and mint a
	// session with the same configuration the middleware checks against.
	guard auth.Config
}

func New(m *deploy.Manager, c *catalog.Catalog, keys *apikey.Manager, fx *fetch.Manager,
	poolGiB float64, hubDir, sourcesDir string, guard auth.Config) (http.Handler, error) {
	tpl, err := ui.Templates()
	if err != nil {
		return nil, err
	}
	s := &Server{mgr: m, cat: c, keys: keys, fetch: fx, tpl: tpl, pool: poolGiB, hubDir: hubDir, guard: guard,
		src: &sources.Manager{Root: sourcesDir}, mux: http.NewServeMux()}

	// The OpenAI-compatible surface. Every deployed model behind one endpoint,
	// chosen by name, so a client never has to know which port anything landed
	// on - and never has to discover a model is still loading by getting a
	// connection refused.
	gw := &gateway.Gateway{Res: m, Cat: c, Host: m.BindHost}
	s.mux.HandleFunc("GET /v1/models", gw.ListModels)
	for _, p := range []string{
		"POST /v1/chat/completions",
		"POST /v1/completions",
		"POST /v1/embeddings",
		"POST /v1/rerank",
		// vLLM exposes these and they carry the model in the body like every
		// other path here, so routing them costs nothing. /v1/messages is the
		// Anthropic-shaped surface; Envoy AI Gateway serves the same thing
		// under /anthropic/v1/messages, but there is no second provider here to
		// disambiguate from, so it keeps the path the upstream actually uses.
		"POST /v1/responses",
		"POST /v1/messages",
		// The audio paths matter here: this node serves ASR and TTS through
		// Sous too, and they were the clients most tied to hardcoded ports.
		"POST /v1/audio/transcriptions",
		"POST /v1/audio/translations",
		"POST /v1/audio/speech",
	} {
		s.mux.HandleFunc(p, gw.Proxy)
	}

	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	s.mux.HandleFunc("GET /api/recipes", s.listRecipes)
	s.mux.HandleFunc("POST /api/recipes/sync", s.syncRecipes)
	s.mux.HandleFunc("POST /api/recipes", s.createRecipe)
	s.mux.HandleFunc("PUT /api/recipes/{id}", s.updateRecipe)
	s.mux.HandleFunc("DELETE /api/recipes/{id}", s.deleteRecipe)
	s.mux.HandleFunc("POST /api/recipes/{id}/diff", s.diffRecipe)
	// API KEYS. Inference-only credentials, so a client can call a model
	// without holding something that could undeploy one.
	// FETCH. Give it a HuggingFace repo id and it downloads the weights into
	// the same cache deployments read from. Separate from deploying, because
	// the alternative is a model server downloading 37 GiB silently during a
	// window where something else was stopped to make room for it.
	s.mux.HandleFunc("POST /api/fetch", s.startFetch)
	s.mux.HandleFunc("GET /api/fetch", s.listFetches)
	s.mux.HandleFunc("POST /api/fetch/forget", s.forgetFetch)
	s.mux.HandleFunc("POST /larder/fetch", s.startFetch)
	s.mux.HandleFunc("POST /larder/fetch/forget", s.forgetFetch)

	s.mux.HandleFunc("GET /api/keys", s.listKeys)
	s.mux.HandleFunc("POST /api/keys", s.createKey)
	s.mux.HandleFunc("DELETE /api/keys/{id}", s.revokeKey)
	s.mux.HandleFunc("POST /keys/{id}/revoke", s.revokeKey)
	s.mux.HandleFunc("POST /keys", s.createKey)
	s.mux.HandleFunc("GET /keys", s.pageKeys)

	s.mux.HandleFunc("GET /api/deployments", s.listDeployments)
	s.mux.HandleFunc("GET /api/status", s.status)
	// Live status. Optional by construction: every page is already correct as
	// served, and this only replaces text the server rendered once.
	s.mux.HandleFunc("GET /events", s.events)
	s.mux.HandleFunc("GET /api/logs/{id}", s.logs)
	s.mux.HandleFunc("GET /api/plan/{id}", s.plan)
	s.mux.HandleFunc("POST /api/deploy/{id}", s.deploy)
	s.mux.HandleFunc("POST /api/undeploy/{id}", s.undeploy)
	s.mux.HandleFunc("GET /api/larder", s.listLarder)
	s.mux.HandleFunc("POST /api/larder/delete", s.deleteWeights)
	s.mux.HandleFunc("GET /api/sources", s.listSources)
	s.mux.HandleFunc("POST /api/sources", s.addSource)
	s.mux.HandleFunc("POST /api/sources/fetch", s.fetchSources)
	s.mux.HandleFunc("GET /sources", s.pageSources)
	// RETIRED. Deployments listed the running set, which is what the Node page
	// already shows with more context. Two lists of the same objects is how the
	// pool bar and the cards came to disagree in the first place.
	s.mux.HandleFunc("GET /deployments", redirectTo("/"))
	s.mux.HandleFunc("GET /larder", s.pageLarder)
	// The Node dashboard is the landing page: the first question on opening
	// this panel is "what is running and is it healthy", not "what could I
	// run next".
	s.mux.HandleFunc("GET /", s.pageNode)
	// MODELS, not Catalog. One list of recipes carrying phase, because a recipe
	// and a deployment are the same object in two states and splitting them
	// across two pages made an operator hold that distinction themselves.
	s.mux.HandleFunc("GET /models", s.pageModels)
	// 301, not a duplicate handler: the old name should stop existing rather
	// than quietly keep working and drift.
	s.mux.HandleFunc("GET /catalog", redirectTo("/models"))
	s.mux.HandleFunc("GET /login", s.pageLogin)
	s.mux.HandleFunc("POST /login", s.doLogin)
	s.mux.HandleFunc("POST /logout", s.doLogout)
	s.mux.HandleFunc("GET /model/{id}", s.pageModel)
	// A question, not an action: GET with no side effects, so it can be linked,
	// refreshed and returned to after stopping something.
	s.mux.HandleFunc("GET /model/{id}/plan", s.pagePlan)
	// An HTML form can only issue GET and POST. The REST verbs above stay
	// for API clients; these are the same handlers on paths a browser can
	// actually submit to.
	s.mux.HandleFunc("POST /model/{id}/update", s.formUpdate)
	s.mux.HandleFunc("POST /model/{id}/delete", s.formDelete)
	// Clearing a record is NOT stopping a container - the container is already
	// gone, which is what makes the record an orphan.
	s.mux.HandleFunc("POST /model/{id}/forget", s.forgetRecord)
	s.mux.HandleFunc("POST /model/{id}/deploy", s.formDeploy)
	s.mux.HandleFunc("POST /model/{id}/undeploy", s.undeploy)
	// Auth wraps the WHOLE mux rather than being applied per route, so a
	// handler added later is protected by default. The alternative fails
	// open, and the route that gets forgotten is the one that creates
	// containers.
	return guard.Middleware(s.mux), nil
}
