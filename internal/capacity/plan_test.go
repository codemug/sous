package capacity

import "testing"

// Numbers below are measured on asus-gx10, not invented. See the capacity
// table in the Sous design spec.
func planner() Planner {
	return Planner{PoolGiB: 121.6, ReserveGiB: 24, WarnFreeGiB: 12}
}

func TestFitsWithMargin(t *testing.T) {
	p := planner()
	// qwen3.6 (61.02) resident, adding asr (1.19).
	got := p.Plan([]Entry{{ID: "qwen36", GiB: 61.02}}, Entry{ID: "asr", GiB: 1.19})
	if !got.Fits {
		t.Fatalf("asr should fit alongside qwen3.6: %+v", got)
	}
	if got.MarginGiB <= 0 {
		t.Fatalf("want positive margin, got %.2f", got.MarginGiB)
	}
}

// The question actually asked on 2026-08-15: can qwen3.6 and qwen3.8
// co-reside? 61.02 + 70.54 = 131.56 against a 121.6 pool. It cannot, and the
// planner must say so rather than let the node swap into it.
func TestRefusesQwen36PlusQwen38(t *testing.T) {
	p := planner()
	got := p.Plan([]Entry{{ID: "qwen38", GiB: 70.54}}, Entry{ID: "qwen36", GiB: 61.02})
	if got.Fits {
		t.Fatal("planner admitted a 131.6 GiB pair into a 121.6 GiB pool")
	}
	if len(got.MustFree) == 0 {
		t.Fatal("a refusal must name what to free")
	}
	if got.MarginGiB >= 0 {
		t.Fatalf("refusal must report a negative margin, got %.2f", got.MarginGiB)
	}
}

// A bare boolean is not enough: a 2 GiB pass must look different from a 30 GiB
// one. 91.0 GiB is the FP8 configuration that engaged 4.2 GiB of swap.
func TestWarnsWhenFreeMemoryIsThin(t *testing.T) {
	p := planner()
	got := p.Plan(nil, Entry{ID: "qwen38-fp8", GiB: 91.0})
	if !got.Fits {
		t.Fatalf("91.0 GiB should fit in 121.6 - 24: %+v", got)
	}
	if got.Warning == "" {
		t.Fatal("a configuration leaving under 12 GiB free must warn")
	}
}

func TestComfortableFitDoesNotWarn(t *testing.T) {
	p := planner()
	got := p.Plan(nil, Entry{ID: "asr", GiB: 1.19})
	if got.Warning != "" {
		t.Fatalf("a 1.19 GiB deployment must not warn: %q", got.Warning)
	}
}

func TestCommittedIncludesResident(t *testing.T) {
	p := planner()
	got := p.Plan([]Entry{{ID: "a", GiB: 10}, {ID: "b", GiB: 5}}, Entry{ID: "c", GiB: 2})
	if got.CommittedGiB != 17 {
		t.Fatalf("want committed 17, got %.2f", got.CommittedGiB)
	}
}

// MustFree is ordered largest-first so the operator is offered the shortest
// route back to a fitting configuration.
func TestMustFreeIsLargestFirst(t *testing.T) {
	p := planner()
	got := p.Plan([]Entry{{ID: "small", GiB: 5}, {ID: "big", GiB: 80}}, Entry{ID: "x", GiB: 40})
	if got.Fits {
		t.Fatal("125 GiB should not fit")
	}
	if got.MustFree[0] != "big" {
		t.Fatalf("want largest first, got %v", got.MustFree)
	}
}
