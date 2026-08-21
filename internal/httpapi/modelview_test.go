package httpapi

import (
	"testing"

	"github.com/codemug/sous/internal/deploy"
	"github.com/codemug/sous/internal/observe"
	"github.com/codemug/sous/internal/recipe"
)

func view(id string, phase deploy.Phase, declared float64) ModelView {
	return ModelView{
		Recipe:      recipe.Recipe{ID: id},
		Phase:       phase,
		DeclaredGiB: declared,
	}
}

// THE LIVE INCONSISTENCY THIS REPLACES. The bar coloured segments from a binary
// Drifted flag while the cards below used five phases, so one model could read
// amber above and green below on the same screen. Both now come from Phase.
func TestSegmentsCarryThePhase(t *testing.T) {
	b := poolBar([]ModelView{
		view("a", deploy.PhaseReady, 10),
		view("b", deploy.PhaseStarting, 20),
	}, 100, 24)

	got := map[string]deploy.Phase{}
	for _, s := range b.Segments {
		if s.ID != "" {
			got[s.ID] = s.Phase
		}
	}
	if got["a"] != deploy.PhaseReady || got["b"] != deploy.PhaseStarting {
		t.Fatalf("segment phases = %v", got)
	}
}

// FAILED AND GONE HOLD NO MEMORY. Drawing them as segments would show the node
// as full when it is empty, and would make the bar disagree with the planner.
func TestOrphansAreNotSegments(t *testing.T) {
	b := poolBar([]ModelView{
		view("live", deploy.PhaseReady, 10),
		view("dead", deploy.PhaseFailed, 40),
		view("ghost", deploy.PhaseGone, 40),
	}, 100, 24)

	for _, s := range b.Segments {
		if s.ID == "dead" || s.ID == "ghost" {
			t.Errorf("%q has no container but was drawn in the bar", s.ID)
		}
	}
	if len(b.Orphans) != 2 {
		t.Fatalf("orphans = %d, want 2", len(b.Orphans))
	}
	// 100 pool - 24 reserve - 10 resident = 66 free. The 80 GiB of dead
	// records must not have been counted.
	if b.MarginGiB != 66 {
		t.Errorf("margin = %v, want 66 - dead records were counted against the pool", b.MarginGiB)
	}
}

// starting and stopping DO hold memory: the allocation is claimed the moment
// the container starts and is not back until it is gone.
func TestStartingAndStoppingCountAgainstThePool(t *testing.T) {
	b := poolBar([]ModelView{
		view("s", deploy.PhaseStarting, 30),
		view("t", deploy.PhaseStopping, 20),
	}, 100, 24)
	if b.MarginGiB != 26 {
		t.Errorf("margin = %v, want 26 (100-24-30-20)", b.MarginGiB)
	}
	if len(b.Orphans) != 0 {
		t.Errorf("a resident phase was treated as an orphan")
	}
}

// The bar must draw what the planner counts. If the picture used declared while
// the arithmetic used observed, the two would disagree - and the picture is the
// one people trust.
func TestSegmentWidthPrefersTheObservation(t *testing.T) {
	v := view("a", deploy.PhaseReady, 10)
	obs := observe.Observation{WeightsGiB: 30, KVGiB: 5}
	v.Observed = &obs
	v.ObservedGiB = 35

	if got := v.FootprintGiB(); got != 35 {
		t.Fatalf("footprint = %v, want the observed 35", got)
	}
	b := poolBar([]ModelView{v}, 100, 24)
	if b.MarginGiB != 41 {
		t.Errorf("margin = %v, want 41 (100-24-35) - the declared figure was used", b.MarginGiB)
	}
}

// A recipe with nothing deployed is not a phase and not a segment.
func TestUndeployedRecipesAreNeitherSegmentNorOrphan(t *testing.T) {
	b := poolBar([]ModelView{
		{Recipe: recipe.Recipe{ID: "shelf"}, DeclaredGiB: 40},
	}, 100, 24)
	for _, s := range b.Segments {
		if s.ID == "shelf" {
			t.Error("an undeployed recipe was drawn in the bar")
		}
	}
	if len(b.Orphans) != 0 {
		t.Error("an undeployed recipe was listed as an orphan")
	}
	if b.MarginGiB != 76 {
		t.Errorf("margin = %v, want 76 - a shelved recipe was counted", b.MarginGiB)
	}
}

// The reserve is always drawn, and free only when there is any.
func TestReserveIsAlwaysDrawnAndFreeOnlyWhenPositive(t *testing.T) {
	full := poolBar([]ModelView{view("big", deploy.PhaseReady, 76)}, 100, 24)
	var sawReserve, sawFree bool
	for _, s := range full.Segments {
		sawReserve = sawReserve || s.Reserve
		sawFree = sawFree || s.Free
	}
	if !sawReserve {
		t.Error("no reserve segment")
	}
	if sawFree {
		t.Error("a free segment was drawn with zero margin")
	}
	if full.MarginGiB != 0 {
		t.Errorf("margin = %v, want 0", full.MarginGiB)
	}
}

// An over-committed pool must not produce a segment wider than the bar, or it
// pushes every later segment out of view and the picture stops being readable.
func TestOvercommitDoesNotOverflowASegment(t *testing.T) {
	b := poolBar([]ModelView{view("huge", deploy.PhaseReady, 500)}, 100, 24)
	for _, s := range b.Segments {
		if s.Pct > 100 {
			t.Errorf("segment %q is %v%% of the bar", s.ID, s.Pct)
		}
	}
	if b.MarginGiB >= 0 {
		t.Errorf("margin = %v, want negative - the refusal has to be visible", b.MarginGiB)
	}
}

func TestCountsAreByPhase(t *testing.T) {
	b := poolBar([]ModelView{
		view("a", deploy.PhaseReady, 5), view("b", deploy.PhaseReady, 5),
		view("c", deploy.PhaseStarting, 5), view("d", deploy.PhaseFailed, 5),
	}, 100, 24)
	if b.Counts[string(deploy.PhaseReady)] != 2 {
		t.Errorf("ready = %d, want 2", b.Counts[string(deploy.PhaseReady)])
	}
	if b.Counts[string(deploy.PhaseStarting)] != 1 || b.Counts[string(deploy.PhaseFailed)] != 1 {
		t.Errorf("counts = %v", b.Counts)
	}
}

// The ruler has to span the pool, so a segment can be read against a number.
func TestRulerSpansThePool(t *testing.T) {
	b := poolBar(nil, 121.6, 24)
	if len(b.Ticks) != 5 {
		t.Fatalf("ticks = %v", b.Ticks)
	}
	if b.Ticks[0] != 0 || b.Ticks[4] != 121.6 {
		t.Errorf("ruler runs %v..%v, want 0..121.6", b.Ticks[0], b.Ticks[4])
	}
}

// A narrow segment cannot carry a legible label; the tooltip carries it
// instead. Rendering one produces a truncated smear.
func TestNarrowSegmentsDropTheirLabel(t *testing.T) {
	b := poolBar([]ModelView{
		view("tiny", deploy.PhaseReady, 1),
		view("wide", deploy.PhaseReady, 50),
	}, 100, 24)
	for _, s := range b.Segments {
		if s.ID == "tiny" && s.Label != "" {
			t.Errorf("a 1%% segment carries the label %q", s.Label)
		}
		if s.ID == "wide" && s.Label == "" {
			t.Error("a 50% segment has no label")
		}
	}
}
