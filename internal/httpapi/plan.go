package httpapi

import (
	"net/http"

	"github.com/codemug/sous/internal/capacity"
	"github.com/codemug/sous/internal/deploy"
)

// PlanPage is a capacity decision as a page rather than a sentence.
//
// A refusal carries a margin, a list of what to stop, and a force path. None of
// that fits in "?msg=…&err=1", which is what it used to be reduced to - so an
// operator was told no and had to go and work out the rest themselves.
type PlanPage struct {
	ModelID  string
	Incoming ModelView
	Result   capacity.Result

	// Projected is the bar WITH the incoming model in it, so the refusal is
	// something you look at rather than something you read.
	Projected PoolBar
	// MustFree are the residents the planner named, as full views so each can
	// carry a Stop button and its own size.
	MustFree []ModelView
	// Scale is the bar's denominator. When the projection overflows the pool it
	// exceeds PoolGiB, so the overflow is drawn AS overflow rather than being
	// clipped into a bar that looks merely full.
	Scale float64
	// PoolMarkPct is where the real pool ends on that scale, for the dashed
	// rule.
	PoolMarkPct float64
}

// plan builds the projection for deploying one model.
func (s *Server) buildPlan(r *http.Request, id string) (*PlanPage, error) {
	views, err := s.models(r)
	if err != nil {
		return nil, err
	}

	var incoming ModelView
	var found bool
	resident := []capacity.Entry{}
	for _, v := range views {
		if v.Recipe.ID == id {
			incoming, found = v, true
			continue
		}
		// Only what actually holds memory. A failed record counted here would
		// make the node look full and refuse a deployment that fits.
		if v.Resident() {
			resident = append(resident, capacity.Entry{ID: v.Recipe.ID, GiB: v.FootprintGiB()})
		}
	}
	if !found {
		return nil, errNotFound(id)
	}

	p := &PlanPage{ModelID: id, Incoming: incoming}
	p.Result = s.mgr.Planner.Plan(resident, capacity.Entry{
		ID: id, GiB: incoming.FootprintGiB(),
	})

	must := map[string]bool{}
	for _, m := range p.Result.MustFree {
		must[m] = true
	}
	for _, v := range views {
		if must[v.Recipe.ID] {
			p.MustFree = append(p.MustFree, v)
		}
	}

	// The projected bar: every resident, then the incoming model as its own
	// segment so its share is visible rather than inferred from a number.
	proj := make([]ModelView, 0, len(views))
	for _, v := range views {
		if v.Recipe.ID != id && v.Resident() {
			proj = append(proj, v)
		}
	}
	incomingSeg := incoming
	if incomingSeg.Phase == "" {
		// Borrow starting so it draws hatched: claimed, not yet serving - which
		// is exactly what a projection is.
		incomingSeg.Phase = deploy.PhaseStarting
	}
	proj = append(proj, incomingSeg)

	p.Scale = s.pool
	if need := p.Result.CommittedGiB + s.mgr.Planner.ReserveGiB; need > p.Scale {
		p.Scale = need
	}
	p.Projected = poolBar(proj, p.Scale, s.mgr.Planner.ReserveGiB)
	// The bar's own margin is against the scaled denominator; the planner's is
	// against the real pool, and that is the one a decision rests on.
	p.Projected.MarginGiB = p.Result.MarginGiB
	p.PoolMarkPct = pct(s.pool, p.Scale)
	return p, nil
}

type notFoundErr string

func errNotFound(id string) error { return notFoundErr(id) }
func (e notFoundErr) Error() string {
	return "no recipe " + string(e)
}

// pagePlan answers GET /model/{id}/plan. No side effects: it is a question.
func (s *Server) pagePlan(w http.ResponseWriter, r *http.Request) {
	v, ok := id(r, w)
	if !ok {
		return
	}
	s.page(w, r, "plan", "Deploy "+v, func(d *pageData) error {
		p, err := s.buildPlan(r, v)
		if err != nil {
			return err
		}
		d.Plan = p
		return nil
	})
}

// renderPlanRefusal answers a deploy that will not fit.
//
// 409 AND THE PLAN PAGE, not a redirect with a message. The margin, the list of
// what to stop and the force path are the whole answer, and a query string can
// carry none of them.
func (s *Server) renderPlanRefusal(w http.ResponseWriter, r *http.Request, id string, capErr error) {
	if !wantsHTML(r) {
		// A script gets the structured refusal it can act on, unchanged.
		writeErr(w, http.StatusConflict, capErr.Error())
		return
	}
	w.WriteHeader(http.StatusConflict)
	s.pageBody(w, r, "plan", "Deploy "+id, func(d *pageData) error {
		p, err := s.buildPlan(r, id)
		if err != nil {
			return err
		}
		d.Plan = p
		d.Message = capErr.Error()
		d.IsError = true
		return nil
	})
}
