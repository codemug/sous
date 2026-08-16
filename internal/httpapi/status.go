package httpapi

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/codemug/sous/internal/engine"
	"github.com/codemug/sous/internal/observe"
)

// ModelStatus is one deployed model as a dashboard needs it.
//
// Running is the field that matters most and the one the old UI could not
// show. A deployment RECORD and a live container are different things, and
// they drift: a container can be killed by the OOM reaper, by `docker rm`, or
// by a crash loop that exhausted its restarts, and Sous's record survives all
// three. A dashboard that lists records and calls them models is lying in
// exactly the situation an operator most needs the truth.
type ModelStatus struct {
	RecipeID string `json:"recipe_id"`
	Port     int    `json:"port"`
	Running  bool   `json:"running"`
	// Drifted marks a record whose container is gone. Named separately from
	// !Running so a UI can style it as a fault rather than as "off".
	Drifted bool `json:"drifted"`

	ContainerID string    `json:"container_id,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	UptimeSec   float64   `json:"uptime_seconds"`

	Modality string   `json:"modality,omitempty"`
	ServedAs []string `json:"served_as,omitempty"`
	Archived bool     `json:"archived,omitempty"`

	// Declared is what the recipe promised; Observed is what the boot log
	// actually reported. Showing both is the point - the gap between them is
	// how a capacity plan silently becomes wrong.
	DeclaredGiB float64              `json:"declared_gib"`
	Observed    *observe.Observation `json:"observed,omitempty"`
	ObservedGiB float64              `json:"observed_gib,omitempty"`
}

// NodeStatus is the whole-node summary the dashboard opens with.
type NodeStatus struct {
	PoolGiB      float64 `json:"pool_gib"`
	ReserveGiB   float64 `json:"reserve_gib"`
	CommittedGiB float64 `json:"committed_gib"`
	MarginGiB    float64 `json:"margin_gib"`

	RecipeCount   int `json:"recipe_count"`
	ArchivedCount int `json:"archived_count"`
	DeployedCount int `json:"deployed_count"`
	RunningCount  int `json:"running_count"`
	DriftedCount  int `json:"drifted_count"`

	Models []ModelStatus `json:"models"`

	// PortRange is shown because "which port is this on" is the question the
	// dashboard exists to answer, and an allocation that runs out is otherwise
	// only visible as a deploy failure.
	PortLow  int `json:"port_low"`
	PortHigh int `json:"port_high"`
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	out, err := s.nodeStatus(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// nodeStatus is shared by the JSON endpoint and the Node page, so the number
// the dashboard draws and the number a script reads can never disagree.
func (s *Server) nodeStatus(r *http.Request) (NodeStatus, error) {
	recipes, err := s.cat.List()
	if err != nil {
		return NodeStatus{}, err
	}
	byID := map[string]int{} // id -> index
	for i, rec := range recipes {
		byID[rec.ID] = i
	}

	ds, err := s.mgr.List()
	if err != nil {
		return NodeStatus{}, err
	}

	// One call, not one per model: asking the runtime per deployment turns a
	// dashboard refresh into N Docker round trips.
	live := map[string]bool{}
	liveKnown := false
	if names, err := s.mgr.Runtime.Running(r.Context()); err == nil {
		liveKnown = true
		for _, n := range names {
			live[n] = true
		}
	}

	out := NodeStatus{
		PoolGiB: s.pool, ReserveGiB: s.mgr.Planner.ReserveGiB,
		PortLow: s.mgr.Ports.Low, PortHigh: s.mgr.Ports.High,
		RecipeCount: len(recipes),
		Models:      make([]ModelStatus, 0, len(ds)),
	}
	for _, rec := range recipes {
		if rec.Archived {
			out.ArchivedCount++
		}
	}

	for _, d := range ds {
		m := ModelStatus{
			RecipeID: d.RecipeID, Port: d.HostPort, ContainerID: d.ContainerID,
			StartedAt: d.StartedAt, UptimeSec: time.Since(d.StartedAt).Seconds(),
		}
		if i, ok := byID[d.RecipeID]; ok {
			rec := recipes[i]
			m.Modality = string(rec.Modality)
			m.ServedAs = rec.ServedAs
			m.Archived = rec.Archived
			m.DeclaredGiB = rec.Declared.WeightsGiB + rec.Declared.KVGiB
		}
		// A zero-valued observation means the boot log was never parsed, which
		// is different from one that reported zero.
		if d.Observation.WeightsGiB > 0 || d.Observation.KVGiB > 0 {
			obs := d.Observation
			m.Observed = &obs
			m.ObservedGiB = obs.WeightsGiB + obs.KVGiB
		}

		// Unknown liveness is NOT reported as drift. If the runtime could not
		// be reached, the honest answer is "we do not know", and flagging every
		// model as faulty on a transient Docker hiccup would train the operator
		// to ignore the indicator.
		if liveKnown {
			m.Running = live[engine.ContainerName(d.RecipeID)]
			m.Drifted = !m.Running
		}

		out.Models = append(out.Models, m)
		out.DeployedCount++
		if m.Running {
			out.RunningCount++
		}
		if m.Drifted {
			out.DriftedCount++
		}
		out.CommittedGiB += max(m.ObservedGiB, m.DeclaredGiB)
	}
	out.MarginGiB = s.pool - out.ReserveGiB - out.CommittedGiB
	return out, nil
}

// pageNode renders the Node dashboard.
func (s *Server) pageNode(w http.ResponseWriter, r *http.Request) {
	// "GET /" is a CATCH-ALL in Go's ServeMux: without this guard every
	// unmatched path renders the dashboard with a 200, so a typo'd URL and a
	// working one are indistinguishable, and a mistyped API call looks like it
	// succeeded. This guard moved here with the route; pageCatalog used to own
	// it.
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	s.page(w, r, "node", "Node", func(d *pageData) error {
		n, err := s.nodeStatus(r)
		if err != nil {
			return err
		}
		d.Node = &n
		return nil
	})
}

// logs returns a container's recent output.
//
// Tail-limited by default. vLLM boot logs run to thousands of lines and a
// dashboard panel that loads all of them is slower and less useful than one
// showing the end, which is where a failure is.
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	id, ok := id(r, w)
	if !ok {
		return
	}
	tail := 200
	if v := r.URL.Query().Get("tail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 5000 {
			writeErr(w, http.StatusBadRequest, "tail must be 1-5000")
			return
		}
		tail = n
	}

	rc, err := s.mgr.Runtime.Logs(r.Context(), engine.ContainerName(id))
	if err != nil {
		writeErr(w, http.StatusNotFound,
			fmt.Sprintf("no logs for %s: %v", id, err))
		return
	}
	defer rc.Close()

	// Read with a ceiling. A container that has been up for days holds more
	// output than is worth buffering to answer a UI panel.
	const maxRead = 4 << 20
	buf, err := io.ReadAll(io.LimitReader(rc, maxRead))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(lastLines(buf, tail))
}

// lastLines returns the final n lines of b without splitting the whole buffer
// into a slice of strings, which for megabytes of logs is most of the cost.
func lastLines(b []byte, n int) []byte {
	if n <= 0 || len(b) == 0 {
		return b
	}
	count := 0
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] != '\n' {
			continue
		}
		// Ignore a trailing newline so it does not consume one of the n.
		if i == len(b)-1 {
			continue
		}
		count++
		if count == n {
			return b[i+1:]
		}
	}
	return b
}
