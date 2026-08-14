// Package capacity answers "does this fit" with a margin rather than a
// boolean, because a 2 GiB pass and a 30 GiB pass call for different decisions
// and a bare yes hides which one you have.
//
// THE RESERVE IS A CALIBRATION, NOT A FORMULA. Overhead between model total and
// observed `used` ran 10.8, 17.4, 19.1, 21.5 and 22.0 GiB across five measured
// combinations on asus-gx10. An earlier attempt to explain it by vLLM process
// count was falsified by the next measurement: the two single-vLLM sets
// produced both the lowest and the highest overhead, bracketing every
// two-vLLM figure. So the reserve simply sits above the observed maximum. It
// will refuse some sets that would in fact fit; force is the escape hatch, and
// accumulating observations is the only thing that improves it.
package capacity

import "fmt"

type Entry struct {
	ID  string
	GiB float64
}

type Planner struct {
	PoolGiB     float64
	ReserveGiB  float64
	WarnFreeGiB float64
}

type Result struct {
	Fits         bool     `json:"fits"`
	CommittedGiB float64  `json:"committed_gib"`
	MarginGiB    float64  `json:"margin_gib"`
	Warning      string   `json:"warning,omitempty"`
	MustFree     []string `json:"must_free,omitempty"`
}

func (p Planner) Plan(resident []Entry, incoming Entry) Result {
	committed := incoming.GiB
	for _, e := range resident {
		committed += e.GiB
	}
	usable := p.PoolGiB - p.ReserveGiB
	res := Result{CommittedGiB: committed, MarginGiB: usable - committed}
	res.Fits = res.MarginGiB >= 0

	if !res.Fits {
		// Name what to free, largest first: the shortest route back to a
		// configuration that fits.
		need := -res.MarginGiB
		freed := 0.0
		for _, e := range byLargest(resident) {
			if freed >= need {
				break
			}
			res.MustFree = append(res.MustFree, e.ID)
			freed += e.GiB
		}
		return res
	}
	// Projected free memory is the MARGIN, not PoolGiB-committed. The reserve
	// is what stands in for overhead - OS, containers, CUDA contexts - and
	// ignoring it overstates free memory by 10-22 GiB.
	//
	// Checked against reality: the 91.0 GiB FP8 configuration gives a margin of
	// 121.6 - 24 - 91.0 = 6.6 GiB, and that deployment was measured leaving
	// 8 GiB available with 4.2 GiB of swap engaged. Computing free as
	// PoolGiB-committed would have predicted 30.6 GiB and warned about nothing.
	if res.MarginGiB < p.WarnFreeGiB {
		res.Warning = fmt.Sprintf(
			"only ~%.1f GiB projected free; both configurations that engaged swap "+
				"on this node sat below %.0f GiB", res.MarginGiB, p.WarnFreeGiB)
	}
	return res
}

func byLargest(in []Entry) []Entry {
	out := append([]Entry(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].GiB > out[j-1].GiB; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
