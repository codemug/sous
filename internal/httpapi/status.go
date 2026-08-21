package httpapi

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/codemug/sous/internal/deploy"
	"github.com/codemug/sous/internal/engine"
	"github.com/codemug/sous/internal/observe"
)

// phaseDetail turns a phase into the sentence an operator needs next. A red
// badge that does not say why sends someone to the logs for something the
// runtime already reported.
func phaseDetail(p deploy.Phase, st engine.ContainerState) string {
	switch p {
	case deploy.PhaseStarting:
		return "loading - the container is up but the port is not answering yet"
	case deploy.PhaseStopping:
		return "stopping - waiting for the container to release the pool"
	case deploy.PhaseGone:
		return "no container behind this record"
	case deploy.PhaseFailed:
		switch {
		case st.OOMKilled:
			return "OOM-killed - it asked for more memory than the node had"
		case st.Status == "restarting":
			return fmt.Sprintf("crash-looping (%d restarts)", st.Restarts)
		case st.Status == "exited":
			return fmt.Sprintf("exited with code %d", st.ExitCode)
		}
		return "failed"
	}
	return ""
}

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
	// Phase is what the deployment is DOING - starting, ready, failed,
	// stopping, gone. Running only ever said whether a container existed,
	// which is true for the eight to ten minutes a vLLM model spends loading
	// and is exactly the window a caller must not send traffic in.
	Phase deploy.Phase `json:"phase"`
	// Detail explains a phase that needs explaining, so a red badge is not the
	// end of the story.
	Detail string `json:"detail,omitempty"`
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
	// Counted separately from Running because "up" and "usable" are different
	// questions and the dashboard is asked the second one.
	ReadyCount    int `json:"ready_count"`
	StartingCount int `json:"starting_count"`
	FailedCount   int `json:"failed_count"`
	// Transitioning is true while any phase can still change on its own. The
	// dashboard polls only while it holds, so watching a model load does not
	// require reloading by hand and a settled page is left alone.
	Transitioning bool `json:"transitioning"`

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
	// States, not Running: a name list cannot separate loading from serving or
	// a clean stop from a crash loop, and the dashboard has to show all four.
	states := map[string]engine.ContainerState{}
	liveKnown := false
	if st, err := s.mgr.Runtime.States(r.Context()); err == nil {
		liveKnown = true
		states = st
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
		st := states[engine.ContainerName(d.RecipeID)]
		if liveKnown {
			m.Running = st.Running()
			m.Drifted = !m.Running
		}
		m.Phase = s.mgr.Phase(r.Context(), d, st, liveKnown)
		m.Detail = phaseDetail(m.Phase, st)

		out.Models = append(out.Models, m)
		out.DeployedCount++
		if m.Running {
			out.RunningCount++
		}
		if m.Drifted {
			out.DriftedCount++
		}
		switch m.Phase {
		case deploy.PhaseReady:
			out.ReadyCount++
		case deploy.PhaseStarting:
			out.StartingCount++
		case deploy.PhaseFailed:
			out.FailedCount++
		}
		if m.Phase == deploy.PhaseStarting || m.Phase == deploy.PhaseStopping {
			out.Transitioning = true
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
	_, _ = io.WriteString(w, safeText(lastLines(buf, tail)))
}

// safeText makes container output safe to embed in a page.
//
// The demultiplexer in engine.Logs removes Docker's framing, which was the
// cause of the corruption. This is the belt: a container may legitimately emit
// binary - a progress bar, a truncated multi-byte character at a read
// boundary, a stray NUL - and none of that should be able to break the
// document around it. Invalid sequences become U+FFFD rather than being
// dropped, so the log still shows that SOMETHING was there.
func safeText(b []byte) string {
	if utf8.Valid(b) {
		return strings.ReplaceAll(string(b), "\x00", "")
	}
	return strings.ReplaceAll(strings.ToValidUTF8(string(b), "\uFFFD"), "\x00", "")
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
