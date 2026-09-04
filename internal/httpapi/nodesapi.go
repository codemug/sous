package httpapi

import (
	"net/http"
	"sort"
	"time"
)

// nodeJSON is the fleet's live state as the board UI consumes it: one entry
// per node in the catalog, with the same committed/margin arithmetic the
// server-rendered node cards use (status.go's nodeCards), so the JSON board
// and any HTML fallback can never disagree about whether a model fits.
//
// This is the ONLY machine-readable view of the fleet - everything else
// (node.html, models.html weight chips) is html/template output. The board
// polls this a few times a minute; it is a pure in-memory read of the
// catalog, so a short poll is cheap.
type nodeJSON struct {
	NodeID       string           `json:"node_id"`
	Connected    bool             `json:"connected"`
	SnapshotAgeS float64          `json:"snapshot_age_s"` // seconds since this node's last snapshot; the UI shows freshness and flags staleness even while Connected
	PoolGiB      float64          `json:"pool_gib"`
	ReserveGiB   float64          `json:"reserve_gib"`
	CommittedGiB float64          `json:"committed_gib"`
	MarginGiB    float64          `json:"margin_gib"` // pool - reserve - committed, EXACTLY as capacity.Planner and planOnNode compute it
	Deployments  []deploymentJSON `json:"deployments"`
	// CachedWeightRepos are HF repo ids ("Org/Name") whose weights are on
	// this node's disk - the exact string recipe.Model holds, so the UI can
	// tell "weights here, not running" from "not on this node".
	CachedWeightRepos []string `json:"cached_weight_repos"`
}

type deploymentJSON struct {
	RecipeID string `json:"recipe_id"`
	HostPort int32  `json:"host_port"`
	// DockerStatus is Docker's RAW status word ("running", "exited",
	// "restarting", "created", "paused", "dead") - NOT a readiness verdict.
	// On the node path nothing probes the model's health yet, so "running"
	// is true for the whole multi-minute vLLM load; the UI must render this
	// as an honest "running (docker)" and never a green "ready". See
	// grpcclient.Handlers.Snapshot for why this is all souslet reports.
	DockerStatus string  `json:"docker_status"`
	WeightsGiB   float64 `json:"weights_gib"`
	KvGiB        float64 `json:"kv_gib"`
}

// TotalGiB is the deployment's committed memory - what the board draws its
// segment to. A method so the board template can size the bar without an
// "add" helper in the funcmap.
func (d deploymentJSON) TotalGiB() float64 { return d.WeightsGiB + d.KvGiB }

// apiNodes serves GET /api/nodes. Registered only when gsrv&&nodes are wired
// (see server.go's gate) so it never touches a nil catalog.
func (s *Server) apiNodes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.fleetView())
}

// fleetView is the single source of the fleet's live state: the JSON API
// (GET /api/nodes) and the server-rendered board (pageBoard) both read it,
// so they cannot disagree about margins or what is deployed where. The
// committed/margin arithmetic here is identical to planOnNode and
// capacity.Planner, so a fit the board shows is a fit the deploy accepts.
func (s *Server) fleetView() []nodeJSON {
	now := time.Now()
	views := s.nodes.All()
	out := make([]nodeJSON, 0, len(views))
	for _, v := range views {
		var committed float64
		deps := make([]deploymentJSON, 0, len(v.Deployments))
		for _, d := range v.Deployments {
			committed += d.WeightsGib + d.KvGib
			deps = append(deps, deploymentJSON{
				RecipeID:     d.RecipeId,
				HostPort:     d.HostPort,
				DockerStatus: d.Phase,
				WeightsGiB:   d.WeightsGib,
				KvGiB:        d.KvGib,
			})
		}
		sort.Slice(deps, func(i, j int) bool { return deps[i].RecipeID < deps[j].RecipeID })

		repos := make([]string, 0, len(v.CachedWeightRepos))
		for repo := range v.CachedWeightRepos {
			repos = append(repos, repo)
		}
		sort.Strings(repos)

		// age is only meaningful once a snapshot has actually landed; a
		// zero LastSnapshot (should not happen for a catalog entry, but be
		// safe) reports 0 rather than ~55 years.
		age := 0.0
		if !v.LastSnapshot.IsZero() {
			age = now.Sub(v.LastSnapshot).Seconds()
		}
		out = append(out, nodeJSON{
			NodeID:            v.NodeID,
			Connected:         v.Connected,
			SnapshotAgeS:      age,
			PoolGiB:           v.PoolGiB,
			ReserveGiB:        v.ReserveGiB,
			CommittedGiB:      committed,
			MarginGiB:         v.PoolGiB - v.ReserveGiB - committed,
			Deployments:       deps,
			CachedWeightRepos: repos,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}
