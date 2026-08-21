package httpapi

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/codemug/sous/internal/deploy"
	"github.com/codemug/sous/internal/engine"
	"github.com/codemug/sous/internal/observe"
	"github.com/codemug/sous/internal/recipe"
)

// ModelView is one recipe and whatever is happening to it.
//
// ONE TYPE FOR THREE PAGES, deliberately. Catalog listed configurations, Node
// and Deployments listed running things, and the same model appeared in all
// three wearing different clothes - which is why an operator had to hold in
// their head that a recipe and a deployment are the same object in two states.
//
// A recipe with nothing deployed has an empty Phase. Archived stays a recipe
// FLAG rather than becoming a phase: it describes the configuration, not what
// the container is doing, and folding it in would make "archived" and "running"
// mutually exclusive when they are not.
type ModelView struct {
	Recipe recipe.Recipe `json:"recipe"`

	Phase deploy.Phase `json:"phase,omitempty"`
	Port  int          `json:"port,omitempty"`

	UptimeSec   float64              `json:"uptime_seconds,omitempty"`
	DeclaredGiB float64              `json:"declared_gib"`
	Observed    *observe.Observation `json:"observed,omitempty"`
	ObservedGiB float64              `json:"observed_gib,omitempty"`

	// Detail is the failure sentence, verbatim. A red chip that does not say
	// why sends someone to the logs for something the runtime already reported.
	Detail string `json:"detail,omitempty"`
}

// Deployed reports whether anything is deployed for this recipe at all.
func (m ModelView) Deployed() bool { return m.Phase != "" }

// Resident reports whether this view is holding memory right now.
func (m ModelView) Resident() bool { return deploy.Resident(m.Phase) }

// FootprintGiB is the number the pool bar draws and the planner counts.
//
// Observation beats declaration when one exists - the same rule capacity uses.
// If the bar drew declared while the planner counted observed, the picture and
// the arithmetic would disagree, and the picture is the one people trust.
func (m ModelView) FootprintGiB() float64 {
	if m.ObservedGiB > 0 {
		return m.ObservedGiB
	}
	return m.DeclaredGiB
}

// models builds the view for every recipe, deployed or not.
func (s *Server) models(r *http.Request) ([]ModelView, error) {
	recipes, err := s.cat.List()
	if err != nil {
		return nil, err
	}
	ds, err := s.mgr.List()
	if err != nil {
		return nil, err
	}
	byID := make(map[string]deploy.Record, len(ds))
	for _, d := range ds {
		byID[d.RecipeID] = d
	}

	states := map[string]engine.ContainerState{}
	known := false
	if st, err := s.mgr.Runtime.States(r.Context()); err == nil {
		states, known = st, true
	}

	out := make([]ModelView, 0, len(recipes))
	for _, rec := range recipes {
		v := ModelView{
			Recipe:      rec,
			DeclaredGiB: rec.Declared.WeightsGiB + rec.Declared.KVGiB,
		}
		if d, ok := byID[rec.ID]; ok {
			st := states[engine.ContainerName(rec.ID)]
			v.Phase = s.mgr.Phase(r.Context(), d, st, known)
			v.Port = d.HostPort
			v.UptimeSec = uptime(d)
			v.Detail = phaseDetail(v.Phase, st)
			// A zero-valued observation means the boot log was never parsed,
			// which is different from one that reported zero.
			if d.Observation.WeightsGiB > 0 || d.Observation.KVGiB > 0 {
				obs := d.Observation
				v.Observed = &obs
				v.ObservedGiB = obs.WeightsGiB + obs.KVGiB
			}
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Recipe.ID < out[j].Recipe.ID })
	return out, nil
}

// Segment is one band of the pool bar.
type Segment struct {
	ID      string
	Pct     float64
	Phase   deploy.Phase
	GiB     float64
	Label   string
	Reserve bool
	Free    bool
}

// Residents is how many models are actually holding memory.
//
// NOT len(Segments): the reserve is always a segment and free usually is, so a
// segment count is never zero and an "empty node" state built on it can never
// appear.
func (b PoolBar) Residents() int {
	n := 0
	for _, s := range b.Segments {
		if s.ID != "" {
			n++
		}
	}
	return n
}

// PoolBar is the whole bar plus what sits under it.
type PoolBar struct {
	PoolGiB    float64
	ReserveGiB float64
	MarginGiB  float64
	Segments   []Segment
	// Orphans are records with no container. NOT segments: they hold no memory,
	// and drawing them in the bar would show the node as full when it is empty.
	Orphans []ModelView
	// Ticks are the ruler marks, so a segment can be read against a number.
	Ticks []float64
	// Counts is phase -> how many, for the legend.
	//
	// Keyed by STRING, not deploy.Phase. html/template cannot convert a string
	// literal to a named type, so {{index .Counts "ready"}} fails at render
	// time with "value has type string; should be deploy.Phase" - and it fails
	// PART WAY THROUGH, leaving a half-written page rather than an error.
	Counts map[string]int
}

// poolBar derives the bar from phases.
//
// This replaces a bar coloured from a binary Drifted flag while the cards below
// it used five phases - the same model could read amber above and green below
// on one screen. Both now come from the same field.
func poolBar(views []ModelView, pool, reserve float64) PoolBar {
	b := PoolBar{
		PoolGiB: pool, ReserveGiB: reserve,
		Counts: map[string]int{},
	}
	var committed float64
	for _, v := range views {
		if !v.Deployed() {
			continue
		}
		b.Counts[string(v.Phase)]++
		if !v.Resident() {
			b.Orphans = append(b.Orphans, v)
			continue
		}
		g := v.FootprintGiB()
		committed += g
		b.Segments = append(b.Segments, Segment{
			ID: v.Recipe.ID, Pct: pct(g, pool), Phase: v.Phase, GiB: g,
			// A label only fits above about a tenth of the bar; below that it
			// renders as a truncated smear and the tooltip carries it instead.
			Label: labelIf(v.Recipe.ID, pct(g, pool) > 11),
		})
	}
	b.MarginGiB = pool - reserve - committed
	b.Segments = append(b.Segments, Segment{
		Pct: pct(reserve, pool), GiB: reserve, Reserve: true, Label: "reserved",
	})
	if b.MarginGiB > 0 {
		b.Segments = append(b.Segments, Segment{
			Pct: pct(b.MarginGiB, pool), GiB: b.MarginGiB, Free: true,
			Label: labelIf(gib(b.MarginGiB)+" free", pct(b.MarginGiB, pool) > 11),
		})
	}
	for i := 0; i <= 4; i++ {
		b.Ticks = append(b.Ticks, pool*float64(i)/4)
	}
	return b
}

func pct(part, whole float64) float64 {
	if whole <= 0 {
		return 0
	}
	v := part / whole * 100
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func labelIf(s string, ok bool) string {
	if ok {
		return s
	}
	return ""
}

func uptime(d deploy.Record) float64 {
	if d.StartedAt.IsZero() {
		return 0
	}
	return time.Since(d.StartedAt).Seconds()
}

// gib formats a GiB figure for a label. One decimal: the pool is 121.6 and the
// difference between 24 and 24.9 GiB matters here, but nothing finer does.
func gib(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) }
