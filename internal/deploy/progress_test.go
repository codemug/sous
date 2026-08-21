package deploy

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// logRuntime serves a canned boot log.
type logRuntime struct {
	*fakeRuntime
	log string
}

func (l *logRuntime) Logs(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(l.log)), nil
}

func progressFor(t *testing.T, log string, rec Record) *Progress {
	t.Helper()
	m := &Manager{Runtime: &logRuntime{fakeRuntime: newFake(), log: log}}
	return m.Progress(context.Background(), rec, "vllm")
}

func stageNamed(p *Progress, name string) Stage {
	for _, s := range p.Stages {
		if s.Name == name {
			return s
		}
	}
	return Stage{}
}

// A model that has finished loading and is compiling should say exactly that -
// not "starting" for ten minutes with no indication of where it has got to.
func TestStagesAdvanceThroughTheBootLog(t *testing.T) {
	p := progressFor(t, `
INFO Starting to load model Qwen/Qwen3.6-35B-A3B-FP8
INFO Model loading took 35.02 GiB memory and 229.22 seconds
INFO Using FLASH_ATTN attention backend out of potential backends
INFO Compiling a graph for compile range (1, 2048)
`, Record{RecipeID: "qwen36", StartedAt: time.Now().Add(-5 * time.Minute)})

	if p == nil {
		t.Fatal("no progress derived")
	}
	if got := stageNamed(p, "load weights"); got.State != StageDone {
		t.Errorf("load weights = %q, want done", got.State)
	} else if got.Seconds != 229.22 {
		t.Errorf("load seconds = %v, want 229.22", got.Seconds)
	}
	if got := stageNamed(p, "compile"); got.State != StageRunning {
		t.Errorf("compile = %q, want running", got.State)
	}
	if got := stageNamed(p, "capture CUDA graphs"); got.State != StagePending {
		t.Errorf("graphs = %q, want pending", got.State)
	}
	if p.Current() != "compile" {
		t.Errorf("current = %q, want compile", p.Current())
	}
}

// The happy case logs NOTHING for a pull or a download: the image was already
// there and the weights were cached. What proves they are finished is a later
// stage having started.
func TestCachedImageAndWeightsReadAsSkippedNotPending(t *testing.T) {
	p := progressFor(t, `
INFO Starting to load model
INFO Model loading took 35.02 GiB memory and 229.22 seconds
`, Record{RecipeID: "x", StartedAt: time.Now()})

	for _, name := range []string{"pull image", "download weights"} {
		s := stageNamed(p, name)
		if s.State != StageSkipped {
			t.Errorf("%s = %q, want skipped - it never had to happen", name, s.State)
		}
		if s.Note == "" {
			t.Errorf("%s gives no reason for being skipped", name)
		}
	}
}

// A download in flight must read as running, or a twenty-minute weight fetch
// looks identical to a model that is nearly up.
func TestDownloadInFlightReadsAsRunning(t *testing.T) {
	p := progressFor(t, `
Fetching 16 files
 44%|####4     | 7/16 [01:12<01:33, 10.4s/it]
`, Record{RecipeID: "x", StartedAt: time.Now()})

	if got := stageNamed(p, "download weights"); got.State != StageRunning {
		t.Errorf("download = %q, want running", got.State)
	}
	if got := stageNamed(p, "load weights"); got.State != StagePending {
		t.Errorf("load = %q, want pending while weights are still arriving", got.State)
	}
}

func TestCompletedStagesCarryTheirMeasurements(t *testing.T) {
	p := progressFor(t, `
INFO Model loading took 35.02 GiB memory and 229.22 seconds
INFO Using FLASH_ATTN attention backend
INFO torch.compile took 31.66 s in total
INFO Profiling CUDA graph memory: PIECEWISE=5 FULL=3
`, Record{RecipeID: "x", StartedAt: time.Now()})

	load := stageNamed(p, "load weights")
	if !strings.Contains(load.Note, "35.02") {
		t.Errorf("load note = %q, want the GiB", load.Note)
	}
	comp := stageNamed(p, "compile")
	if comp.State != StageDone || comp.Seconds != 31.66 {
		t.Errorf("compile = %+v, want done at 31.66s", comp)
	}
	if comp.Note != "FLASH_ATTN" {
		t.Errorf("compile note = %q, want the chosen backend", comp.Note)
	}
	g := stageNamed(p, "capture CUDA graphs")
	if g.State != StageDone || !strings.Contains(g.Note, "PIECEWISE=5") {
		t.Errorf("graphs = %+v", g)
	}
}

// THE HONESTY RULE. No percentage and no ETA: on this hardware weights load at
// disk speed and compilation does not, so any single rate is wrong by minutes.
// The last successful start is the only defensible thing to say.
func TestProgressOffersNoPercentageOrETA(t *testing.T) {
	p := progressFor(t, "INFO Starting to load model\n",
		Record{RecipeID: "x", StartedAt: time.Now()})

	s := p.Summary()
	for _, forbidden := range []string{"%", "ETA", "eta", "remaining", "left"} {
		if strings.Contains(s, forbidden) {
			t.Errorf("summary %q contains %q - it cannot know that", s, forbidden)
		}
	}
}

func TestSummaryQuotesTheLastSuccessfulStart(t *testing.T) {
	rec := Record{RecipeID: "x", StartedAt: time.Now()}
	rec.Observation.LoadSeconds = 461
	p := progressFor(t, "INFO Starting to load model\n", rec)

	if !strings.Contains(p.Summary(), "last start took") {
		t.Errorf("summary = %q, want the previous start's duration", p.Summary())
	}
	if !strings.Contains(p.Summary(), "7m 41s") {
		t.Errorf("summary = %q, want 461s rendered as 7m 41s", p.Summary())
	}
}

// A kind whose boot log has none of these stages gets NO stepper. Five greyed
// steps for a service with two implies knowledge that is not there.
func TestNonVLLMKindsGetNoStepper(t *testing.T) {
	m := &Manager{Runtime: &logRuntime{fakeRuntime: newFake(), log: "whatever"}}
	for _, kind := range []string{"container", "", "tei"} {
		if got := m.Progress(context.Background(), Record{RecipeID: "x"}, kind); got != nil {
			t.Errorf("kind %q got a stepper with %d stages", kind, len(got.Stages))
		}
	}
}

func TestElapsedComesFromTheRecord(t *testing.T) {
	p := progressFor(t, "INFO Starting to load model\n",
		Record{RecipeID: "x", StartedAt: time.Now().Add(-90 * time.Second)})
	if p.ElapsedSec < 85 || p.ElapsedSec > 95 {
		t.Errorf("elapsed = %v, want about 90", p.ElapsedSec)
	}
}
