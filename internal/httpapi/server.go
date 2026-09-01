// Package httpapi exposes the UI and the JSON API on one listener.
//
// Every handler that takes an id validates it before touching a path, a
// container name or the store. Ids reach the filesystem and the Docker API, so
// rejection has to happen at the edge rather than being relied on downstream.
package httpapi

import (
	"github.com/codemug/sous/internal/alias"
	"github.com/codemug/sous/internal/apikey"
	"github.com/codemug/sous/internal/fetch"
	"github.com/codemug/sous/internal/gateway"
	"github.com/codemug/sous/internal/grpcserver"
	"github.com/codemug/sous/internal/hf"
	"github.com/codemug/sous/internal/nodecatalog"
	"github.com/codemug/sous/internal/reqlog"
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
	alias *alias.Manager
	hf      *hf.Store
	fetch   *fetch.Manager
	reqLogW *reqlog.Writer
	reqLogR *reqlog.RetentionStore
	tpl     *template.Template

	// gsrv and nodes are the multi-node deploy path added alongside mgr
	// during the migration described in docs/superpowers/specs/2026-09-01-
	// sous-multinode-design.md: deploy/undeploy/plan requests that carry a
	// node ID route through gsrv to a specific connected souslet instead of
	// mgr's local deploy.Manager. Both fields are optional (nil is valid):
	// New only registers the node-scoped routes that dereference them when
	// both are non-nil (see New's route registration below), so a caller
	// that passes nil here (no test in this package currently does, but
	// nothing requires them to be set) never has a request reach code that
	// would nil-panic on them - the routes simply don't exist. cmd/sous-api,
	// the only production caller of New today, always passes both non-nil.
	gsrv  *grpcserver.Server
	nodes *nodecatalog.Catalog

	pool float64
	// hubDir is the HuggingFace cache under the model directory, as it was
	// scanned per-request by the retired internal/larder page (removed in
	// Task 14 of the multi-node plan). Kept as a field/constructor parameter
	// rather than removed outright, since nothing currently reads it but
	// removing it would mean changing New's signature (and every test call
	// site) for no functional gain.
	hubDir string

	// src mirrors recipe repositories. Fetch is always explicit: nothing is
	// pulled on a timer and nothing is ever deployed by a fetch.
	src *sources.Manager

	mux *http.ServeMux

	// guard is held so the login handlers can verify a password and mint a
	// session with the same configuration the middleware checks against.
	guard auth.Config
}

// gsrv and nodes are the multi-node deploy path (see the Server.gsrv/nodes
// doc comment): pass nil for both from a caller with no souslet fleet to
// talk to, since only the new node-scoped routes ever dereference them.
func New(m *deploy.Manager, c *catalog.Catalog, keys *apikey.Manager, fx *fetch.Manager,
	hfs *hf.Store, rl *reqlog.Writer, rs *reqlog.RetentionStore,
	poolGiB float64, hubDir, sourcesDir string, guard auth.Config,
	gsrv *grpcserver.Server, nodes *nodecatalog.Catalog) (http.Handler, error) {
	tpl, err := ui.Templates()
	if err != nil {
		return nil, err
	}
	s := &Server{mgr: m, cat: c, keys: keys, fetch: fx, hf: hfs, reqLogW: rl, reqLogR: rs, tpl: tpl,
		pool: poolGiB, hubDir: hubDir, guard: guard, gsrv: gsrv, nodes: nodes,
		src: &sources.Manager{Root: sourcesDir}, mux: http.NewServeMux()}

	// The OpenAI-compatible surface. Every deployed model behind one endpoint,
	// chosen by name, so a client never has to know which port anything landed
	// on - and never has to discover a model is still loading by getting a
	// connection refused.
	// Aliases are a LOCAL routing decision, stored beside the deployments
	// rather than on the recipes - a recipe travels to other nodes and one
	// node's naming should not travel with it.
	al := &alias.Manager{Store: m.Store, Cat: c}
	s.alias = al
	// Nodes/GRPC are what make the OpenAI surface work for a model running on
	// another machine: without them set, gateway.Proxy's multi-node branch is
	// unreachable in the shipped binary and every inference request for a
	// model on a connected node falls through to the local deploy.Manager and
	// 404s. They are the same instances the node-scoped deploy routes below
	// use. Passing them through unconditionally is safe: nil in means nil on
	// the Gateway, which is exactly the single-node configuration the
	// local-forward path already expects.
	gw := &gateway.Gateway{Res: m, Cat: c, Alias: al, ReqLog: reqLog(rl), Host: m.BindHost,
		Nodes: nodes, GRPC: gsrv}
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
	// The panel's one static asset (Task 13): dragdrop.js, served straight
	// from the embedded FS rather than through html/template - it is
	// unchanging JS, not a page. Registered unconditionally (not gated
	// behind gsrv/nodes != nil like the node-scoped routes below) because
	// the script itself is harmless to load on a single-node server too -
	// it just never finds a [data-node-id] drop target to attach to there.
	s.mux.Handle("GET /static/dragdrop.js", http.FileServerFS(ui.StaticFS()))
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
	s.mux.HandleFunc("GET /api/fetch/logs", s.fetchLogs)
	s.mux.HandleFunc("POST /api/fetch/forget", s.forgetFetch)
	// 0.17.0 compatibility redirects, not tied to the retired larder page or
	// package (internal/larder) at all - these map straight to the fetch/
	// hf-token handlers by URL prefix, same as the two /larder/hf-token
	// routes below. Left in place because something bookmarked or scripted
	// against them keeps working rather than 404ing, which is the same
	// reasoning that kept them when the Larder page itself was still live.
	s.mux.HandleFunc("POST /larder/fetch", s.startFetch)
	// The HuggingFace token. Gated repos tie licence acceptance to an
	// ACCOUNT, so accepting an agreement in a browser does not make an
	// anonymous download work - it still answers 401.
	// Aliases: extra names a deployed model answers to, kept off the recipe
	// because a recipe travels to other nodes and this naming should not.
	s.mux.HandleFunc("GET /api/aliases", s.getAliases)
	s.mux.HandleFunc("PUT /api/aliases/{id}", s.setAliases)
	// Forms can only GET and POST.
	s.mux.HandleFunc("POST /model/{id}/aliases", s.setAliases)
	s.mux.HandleFunc("GET /api/hf-token", s.getHFToken)
	// Request log retention: how many days of chat-completion audit
	// entries this node keeps before periodic cleanup removes them.
	s.mux.HandleFunc("GET /api/reqlog", s.getReqLog)
	s.mux.HandleFunc("POST /admin/reqlog-retention", s.setRetention)
	s.mux.HandleFunc("PUT /api/hf-token", s.setHFToken)
	s.mux.HandleFunc("DELETE /api/hf-token", s.clearHFToken)
	// Forms can only GET and POST, so the browser paths are their own.
	s.mux.HandleFunc("POST /admin/hf-token", s.setHFToken)
	s.mux.HandleFunc("POST /admin/hf-token/clear", s.clearHFToken)
	// The token lived on the Larder page in 0.17.0. Anything bookmarked or
	// scripted against that path keeps working rather than 404ing.
	s.mux.HandleFunc("POST /larder/hf-token", s.setHFToken)
	s.mux.HandleFunc("POST /larder/hf-token/clear", s.clearHFToken)
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
	// POST /api/deploy/{id} (no node dimension) is NOT retired here, despite
	// Task 14's brief calling for its removal "now that nothing calls it" -
	// see the Task 14 report for the full reasoning. In short: the brief's
	// premise held for the UI (Task 13's drag-and-drop and Task 12's
	// dashboard both already call the node-scoped route below instead), but
	// this route is still the ONLY way to drive s.mgr's legacy
	// deploy.Manager path at all, in production (the model-page form,
	// POST /model/{id}/deploy, falls through to the exact same branch of
	// s.deploy) and in this package's own test suite (dozens of tests across
	// handlers_test.go/status_test.go/screens_test.go/plan_test.go/
	// port_test.go/render_test.go/events_test.go/reqlog_test.go/recipes_test.go
	// use it to set up a deployed fixture - removing it turned 31 passing
	// tests into 405s, not zero). deploy.Manager itself could not be removed
	// in this pass either (again, see the report), so removing its sole
	// entry point while keeping it live behind the scenes would leave the
	// module in a worse state than before, not a cleaner one.
	s.mux.HandleFunc("POST /api/deploy/{id}", s.deploy)
	s.mux.HandleFunc("POST /api/undeploy/{id}", s.undeploy)
	// Node-scoped routes for the multi-node rollout, alongside the
	// single-node routes above rather than replacing them. {id}/{nodeID} is
	// unambiguous against {id} alone - different segment counts, so the
	// mux never has to choose between them.
	//
	// Registered ONLY when gsrv and nodes are both non-nil - see the
	// Server.gsrv/nodes doc comment above. grpcserver.Server.Send and
	// nodecatalog.Catalog.Node both start by locking an embedded
	// sync.RWMutex field, which nil-panics on a nil receiver. So these
	// routes must not exist at all on a server built without a souslet
	// fleet to talk to, rather than exist and crash the process on the
	// first request that reaches one: net/http's ServeMux answers an
	// unregistered path with a normal 404 or 405 instead (see
	// deploy_grpc_test.go's
	// TestNodeScopedRoutesReturnCleanErrorsWhenGRPCIsNotConfigured for
	// exactly which, and why - it depends on this package's own "GET /"
	// catch-all), never a panic.
	if gsrv != nil && nodes != nil {
		s.mux.HandleFunc("GET /api/plan/{id}/{nodeID}", s.plan)
		s.mux.HandleFunc("POST /api/deploy/{id}/{nodeID}", s.deploy)
		s.mux.HandleFunc("POST /api/undeploy/{id}/{nodeID}", s.undeploy)
		// The recipe-card cleanup action (Task 11 of the multi-node plan):
		// clear a (recipe, node) pair's cached weights from that node's
		// disk. Same nil-guard reasoning as the three routes above -
		// deleteWeightsOnNode dereferences s.gsrv, so it must not exist at
		// all on a server built with nil gsrv/nodes.
		s.mux.HandleFunc("POST /api/weights/{recipeID}/{nodeID}/delete", s.deleteWeightsOnNode)
	}
	s.mux.HandleFunc("GET /api/sources", s.listSources)
	s.mux.HandleFunc("POST /api/sources", s.addSource)
	s.mux.HandleFunc("POST /api/sources/fetch", s.fetchSources)
	s.mux.HandleFunc("GET /sources", s.pageSources)
	// RETIRED. Deployments listed the running set, which is what the Node page
	// already shows with more context. Two lists of the same objects is how the
	// pool bar and the cards came to disagree in the first place.
	s.mux.HandleFunc("GET /deployments", redirectTo("/"))
	s.mux.HandleFunc("GET /admin", s.pageAdmin)
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
