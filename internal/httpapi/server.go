// Package httpapi exposes the UI and the JSON API on one listener.
//
// Every handler that takes an id validates it before touching a path, a
// container name or the store. Ids reach the filesystem and the Docker API, so
// rejection has to happen at the edge rather than being relied on downstream.
package httpapi

import (
	"html/template"
	"net/http"

	"github.com/codemug/sous/internal/catalog"
	"github.com/codemug/sous/internal/deploy"
	"github.com/codemug/sous/internal/ui"
)

type Server struct {
	mgr  *deploy.Manager
	cat  *catalog.Catalog
	tpl  *template.Template
	pool float64
	mux  *http.ServeMux
}

func New(m *deploy.Manager, c *catalog.Catalog, poolGiB float64) (http.Handler, error) {
	tpl, err := ui.Templates()
	if err != nil {
		return nil, err
	}
	s := &Server{mgr: m, cat: c, tpl: tpl, pool: poolGiB, mux: http.NewServeMux()}

	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	s.mux.HandleFunc("GET /api/recipes", s.listRecipes)
	s.mux.HandleFunc("GET /api/deployments", s.listDeployments)
	s.mux.HandleFunc("GET /api/plan/{id}", s.plan)
	s.mux.HandleFunc("POST /api/deploy/{id}", s.deploy)
	s.mux.HandleFunc("POST /api/undeploy/{id}", s.undeploy)
	s.mux.HandleFunc("GET /deployments", s.pageDeployments)
	s.mux.HandleFunc("GET /", s.pageCatalog)
	return s.mux, nil
}
