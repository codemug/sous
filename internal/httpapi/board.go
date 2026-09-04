package httpapi

import (
	"net/http"
	"sort"
)

// boardData feeds the "board" template - the fleet board at "/". It is
// rendered server-side so the page works with no JavaScript (every model
// carries a real "Deploy to…" form per connected node); board.js then
// enhances the same DOM with drag-and-drop and live polling of
// GET /api/nodes.
type boardData struct {
	Title       string
	Message     string
	IsError     bool
	MaxScaleGiB float64 // the shared ruler's extent: widest pool or model, rounded up
	Nodes       []nodeJSON
	Models      []boardModel
}

type boardModel struct {
	ID           string
	Repo         string
	Kind         string
	Modality     string
	FootprintGiB float64
	Archived     bool
	OnNode       string // node currently running it, "" if none
	OnStatus     string // that deployment's raw docker status
	Cached       []string
	Fits         []nodeFit // one per connected node, for the no-JS Deploy-to menu
}

type nodeFit struct {
	NodeID         string
	Fits           bool
	MarginAfterGiB float64
}

// pageBoard serves "GET /". Falls back to the legacy single-node view when
// no gRPC fleet is wired (cmd/sous), which has no node catalog to draw.
func (s *Server) pageBoard(w http.ResponseWriter, r *http.Request) {
	// "GET /" is a catch-all in Go's ServeMux; without this a typo'd path
	// renders the board with 200 and looks like it worked.
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if s.nodes == nil {
		// Single-node build: no fleet board to draw. Keep the old node page.
		s.pageNode(w, r)
		return
	}

	nodes := s.fleetView()

	recipes, err := s.cat.List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	maxScale := 16.0
	for _, n := range nodes {
		if n.PoolGiB > maxScale {
			maxScale = n.PoolGiB
		}
	}

	models := make([]boardModel, 0, len(recipes))
	for _, rec := range recipes {
		foot := rec.Declared.TotalGiB()
		if foot > maxScale {
			maxScale = foot
		}
		bm := boardModel{
			ID: rec.ID, Repo: rec.Model, Kind: string(rec.Kind),
			Modality: string(rec.Modality), FootprintGiB: foot, Archived: rec.Archived,
		}
		for _, n := range nodes {
			for _, d := range n.Deployments {
				if d.RecipeID == rec.ID {
					bm.OnNode, bm.OnStatus = n.NodeID, d.DockerStatus
				}
			}
			if rec.Model != "" {
				for _, repo := range n.CachedWeightRepos {
					if repo == rec.Model {
						bm.Cached = append(bm.Cached, n.NodeID)
					}
				}
			}
			if n.Connected && !rec.Archived {
				after := n.MarginGiB - foot
				bm.Fits = append(bm.Fits, nodeFit{NodeID: n.NodeID, Fits: after >= 0, MarginAfterGiB: after})
			}
		}
		models = append(models, bm)
	}
	// Deployed first, then library, then archived; alpha within each group -
	// the operator's eye goes to what is running before what could run.
	sort.SliceStable(models, func(i, j int) bool {
		a, b := models[i], models[j]
		ra, rb := rank(a), rank(b)
		if ra != rb {
			return ra < rb
		}
		return a.ID < b.ID
	})

	d := boardData{
		Title:       "Sous — Fleet",
		Message:     r.URL.Query().Get("msg"),
		IsError:     r.URL.Query().Get("err") == "1",
		MaxScaleGiB: ceilTo(maxScale, 20),
		Nodes:       nodes,
		Models:      models,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, "board", d); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func rank(m boardModel) int {
	switch {
	case m.Archived:
		return 2
	case m.OnNode != "":
		return 0
	default:
		return 1
	}
}

// ceilTo rounds up to the next multiple of step, so the ruler ends on a
// labelled tick rather than mid-gap.
func ceilTo(v, step float64) float64 {
	if v <= 0 {
		return step
	}
	n := int((v-0.0001)/step) + 1
	return float64(n) * step
}
