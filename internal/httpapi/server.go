// Package httpapi exposes the UI and the JSON API on one listener.
//
// Every handler that takes an id validates it before touching a path, a
// container name or the store. Ids reach the filesystem and the Docker API, so
// rejection has to happen at the edge rather than being relied on downstream.
package httpapi

import (
	"html/template"
	"net/http"

	"github.com/codemug/sous/internal/auth"
	"github.com/codemug/sous/internal/catalog"
	"github.com/codemug/sous/internal/deploy"
	"github.com/codemug/sous/internal/sources"
	"github.com/codemug/sous/internal/ui"
)

type Server struct {
	mgr *deploy.Manager
	cat *catalog.Catalog
	tpl *template.Template

	pool float64
	// hubDir is the HuggingFace cache under the model directory. The larder
	// scans it per request: the disk is the source of truth, and caching it
	// would drift from reality the first time anything is deleted by hand.
	hubDir string

	// src mirrors recipe repositories. Fetch is always explicit: nothing is
	// pulled on a timer and nothing is ever deployed by a fetch.
	src *sources.Manager

	mux *http.ServeMux
}

func New(m *deploy.Manager, c *catalog.Catalog, poolGiB float64, hubDir, sourcesDir string,
	guard auth.Config) (http.Handler, error) {
	tpl, err := ui.Templates()
	if err != nil {
		return nil, err
	}
	s := &Server{mgr: m, cat: c, tpl: tpl, pool: poolGiB, hubDir: hubDir,
		src: &sources.Manager{Root: sourcesDir}, mux: http.NewServeMux()}

	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	s.mux.HandleFunc("GET /api/recipes", s.listRecipes)
	s.mux.HandleFunc("POST /api/recipes/sync", s.syncRecipes)
	s.mux.HandleFunc("GET /api/deployments", s.listDeployments)
	s.mux.HandleFunc("GET /api/plan/{id}", s.plan)
	s.mux.HandleFunc("POST /api/deploy/{id}", s.deploy)
	s.mux.HandleFunc("POST /api/undeploy/{id}", s.undeploy)
	s.mux.HandleFunc("GET /api/larder", s.listLarder)
	s.mux.HandleFunc("POST /api/larder/delete", s.deleteWeights)
	s.mux.HandleFunc("GET /api/sources", s.listSources)
	s.mux.HandleFunc("POST /api/sources", s.addSource)
	s.mux.HandleFunc("POST /api/sources/fetch", s.fetchSources)
	s.mux.HandleFunc("GET /sources", s.pageSources)
	s.mux.HandleFunc("GET /deployments", s.pageDeployments)
	s.mux.HandleFunc("GET /larder", s.pageLarder)
	s.mux.HandleFunc("GET /", s.pageCatalog)
	// Auth wraps the WHOLE mux rather than being applied per route, so a
	// handler added later is protected by default. The alternative fails
	// open, and the route that gets forgotten is the one that creates
	// containers.
	return guard.Middleware(s.mux), nil
}
