package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/codemug/sous/internal/deploy"
)

// eventModel is one deployment as the stream reports it.
//
// Deliberately flat and small: the script patches text and widths by id, so
// anything it cannot apply is weight on a wire that ticks every three seconds.
type eventModel struct {
	ID         string       `json:"id"`
	Phase      deploy.Phase `json:"phase"`
	Port       int          `json:"port,omitempty"`
	Pct        float64      `json:"pct"`
	GiB        float64      `json:"gib"`
	Detail     string       `json:"detail,omitempty"`
	ElapsedSec float64      `json:"elapsed_sec,omitempty"`
	Stage      string       `json:"stage,omitempty"`
}

type eventPayload struct {
	CommittedGiB float64      `json:"committed_gib"`
	MarginGiB    float64      `json:"margin_gib"`
	Models       []eventModel `json:"models"`
}

// tick is how often the stream sends.
//
// THREE SECONDS, matching probeTTL. A faster stream would outrun the readiness
// cache and turn one open dashboard into a health-check storm against every
// model port on the node - which is a real cost here, where a "health check" is
// an HTTP request to a process holding a GPU.
const tick = 3 * time.Second

// events streams node status as server-sent events.
//
// OPTIONAL BY CONSTRUCTION. Every page is already correct as served; this only
// replaces text the server rendered once. A browser with no EventSource, or a
// dropped connection, loses liveness and nothing else.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	// Proxies that buffer would defeat the point entirely.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func() bool {
		p, err := s.eventPayload(r)
		if err != nil {
			return true // a transient read failure is not a reason to hang up
		}
		b, err := json.Marshal(p)
		if err != nil {
			return true
		}
		if _, err := w.Write([]byte("event: status\ndata: " + string(b) + "\n\n")); err != nil {
			return false
		}
		fl.Flush()
		return true
	}

	// Send once immediately: a client that connects should not wait a full tick
	// to learn anything, and the first frame is what proves the stream works.
	if !send() {
		return
	}

	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
			if !send() {
				return
			}
		}
	}
}

func (s *Server) eventPayload(r *http.Request) (eventPayload, error) {
	views, err := s.models(r)
	if err != nil {
		return eventPayload{}, err
	}
	bar := poolBar(views, s.pool, s.mgr.Planner.ReserveGiB)

	p := eventPayload{MarginGiB: bar.MarginGiB}
	for _, v := range views {
		if !v.Deployed() {
			continue
		}
		m := eventModel{
			ID: v.Recipe.ID, Phase: v.Phase, Port: v.Port,
			GiB: v.FootprintGiB(), Detail: v.Detail,
		}
		if v.Resident() {
			m.Pct = pct(v.FootprintGiB(), s.pool)
			p.CommittedGiB += v.FootprintGiB()
		}
		if v.Progress != nil {
			m.ElapsedSec = v.Progress.ElapsedSec
			m.Stage = v.Progress.Current()
		}
		p.Models = append(p.Models, m)
	}
	return p, nil
}
