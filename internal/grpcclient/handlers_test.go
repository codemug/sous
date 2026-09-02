package grpcclient

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/codemug/sous/internal/deploy"
	"github.com/codemug/sous/internal/engine"
	"github.com/codemug/sous/internal/fetch"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
	"github.com/codemug/sous/internal/recipe"
	"gopkg.in/yaml.v3"
)

// fakeRuntime is the same shape as deploy.Runtime (internal/deploy/deploy.go)
// - a minimal in-memory double so these tests need no real Docker daemon.
// The compile-time assertion below is what actually proves it satisfies the
// interface; nothing here is asserted "by inspection".
type fakeRuntime struct {
	started  []engine.Spec
	startErr error
	startID  string

	stopped []string
	stopErr error

	states    map[string]engine.ContainerState
	statesErr error
}

var _ deploy.Runtime = (*fakeRuntime)(nil)

func (f *fakeRuntime) Start(_ context.Context, spec engine.Spec) (string, error) {
	f.started = append(f.started, spec)
	if f.startErr != nil {
		return "", f.startErr
	}
	id := f.startID
	if id == "" {
		id = "fake-container-id"
	}
	return id, nil
}

func (f *fakeRuntime) Stop(_ context.Context, name string) error {
	f.stopped = append(f.stopped, name)
	return f.stopErr
}

func (f *fakeRuntime) Logs(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (f *fakeRuntime) Running(context.Context) ([]string, error) { return nil, nil }

func (f *fakeRuntime) States(context.Context) (map[string]engine.ContainerState, error) {
	if f.statesErr != nil {
		return nil, f.statesErr
	}
	return f.states, nil
}

func (f *fakeRuntime) ImageExposedPort(context.Context, string) (int, error) { return 0, nil }

// fakeFetchRuntime is the same shape as fetch.Runtime (internal/fetch/fetch.go).
//
// startCalls/removedJobs record every StartJob/RemoveJob invocation (not
// just whether one happened) so a test can assert Start's destructive
// "remove and restart" path was never reached at all - the exact thing
// HandleFetch must avoid once a job has already finished, done or failed
// (see HandleFetch's own doc comment).
type fakeFetchRuntime struct {
	startErr    error
	startCalls  int
	removedJobs []string

	states map[string]engine.ContainerState
}

var _ fetch.Runtime = (*fakeFetchRuntime)(nil)

func (f *fakeFetchRuntime) StartJob(context.Context, engine.JobSpec) (string, error) {
	f.startCalls++
	if f.startErr != nil {
		return "", f.startErr
	}
	return "fake-job-id", nil
}

func (f *fakeFetchRuntime) JobStates(context.Context) (map[string]engine.ContainerState, error) {
	return f.states, nil
}

func (f *fakeFetchRuntime) RemoveJob(_ context.Context, name string) error {
	f.removedJobs = append(f.removedJobs, name)
	return nil
}

func (f *fakeFetchRuntime) Logs(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

// validRecipeYAML returns a recipe that passes recipe.Validate() - Image and
// Modality are both required there, which the plan's illustrative recipe
// literal (ID/Kind/Model only) omits.
func validRecipeYAML(t *testing.T, id string) string {
	t.Helper()
	rec := recipe.Recipe{
		ID:       id,
		Kind:     recipe.KindVLLM,
		Modality: recipe.ModalityText,
		Model:    "Inferact/Qwen3.8-27B-NVFP4",
		Image:    "vllm/vllm-openai:latest",
	}
	out, err := yaml.Marshal(rec)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	return string(out)
}

func TestHandleDeployStartsTheContainerFromTheEmbeddedRecipeYAML(t *testing.T) {
	rt := &fakeRuntime{}
	h := &Handlers{Runtime: rt, ModelDir: t.TempDir()}

	// An explicitly requested port has to be genuinely free, since
	// HandleDeploy now checks (mirroring deploy.Manager.Deploy's own rule for
	// the -port query parameter). Asking the OS for one and releasing it is
	// how this test names a port that is free on any machine, rather than
	// hardcoding one and hoping.
	wantPort := freePort(t)
	result := h.HandleDeploy(context.Background(), &pb.DeployCommand{
		RecipeId:   "dflash2",
		RecipeYaml: validRecipeYAML(t, "dflash2"),
		WantPort:   int32(wantPort),
	})

	if result.Error != "" {
		t.Fatalf("unexpected error: %s", result.Error)
	}
	if result.RecipeId != "dflash2" {
		t.Fatalf("RecipeId = %q, want dflash2", result.RecipeId)
	}
	if result.ContainerId != "fake-container-id" {
		t.Fatalf("ContainerId = %q, want fake-container-id", result.ContainerId)
	}
	if result.HostPort != int32(wantPort) {
		t.Fatalf("HostPort = %d, want %d", result.HostPort, wantPort)
	}
	if len(rt.started) != 1 {
		t.Fatalf("Start called %d times, want 1", len(rt.started))
	}
	if got, want := rt.started[0].Name, engine.ContainerName("dflash2"); got != want {
		t.Fatalf("started container name = %q, want %q", got, want)
	}
}

func TestHandleDeployRunsDropCachesBeforeStartingTheContainer(t *testing.T) {
	rt := &fakeRuntime{}
	h := &Handlers{Runtime: rt, ModelDir: t.TempDir()}
	called := false
	h.DropCaches = func() error {
		if len(rt.started) != 0 {
			t.Error("DropCaches ran after the container had already started")
		}
		called = true
		return nil
	}

	result := h.HandleDeploy(context.Background(), &pb.DeployCommand{
		RecipeId:   "dflash2",
		RecipeYaml: validRecipeYAML(t, "dflash2"),
		WantPort:   int32(freePort(t)),
	})

	if result.Error != "" {
		t.Fatalf("unexpected error: %s", result.Error)
	}
	if !called {
		t.Fatal("DropCaches was not called")
	}
}

func TestHandleDeploySurfacesDropCachesError(t *testing.T) {
	rt := &fakeRuntime{}
	h := &Handlers{Runtime: rt, ModelDir: t.TempDir()}
	h.DropCaches = func() error { return errors.New("permission denied") }

	result := h.HandleDeploy(context.Background(), &pb.DeployCommand{
		RecipeId:   "dflash2",
		RecipeYaml: validRecipeYAML(t, "dflash2"),
		WantPort:   int32(freePort(t)),
	})

	if result.Error == "" {
		t.Fatal("expected an error, got none")
	}
	if len(rt.started) != 0 {
		t.Fatal("container was started despite DropCaches failing")
	}
}

func TestHandleDeployWithNoDropCachesStillDeploys(t *testing.T) {
	// DropCaches is optional (nil is a no-op) so a Handlers built without
	// one - every existing test before this one, plus any environment that
	// cannot write /proc/sys - still deploys successfully.
	rt := &fakeRuntime{}
	h := &Handlers{Runtime: rt, ModelDir: t.TempDir()}

	result := h.HandleDeploy(context.Background(), &pb.DeployCommand{
		RecipeId:   "dflash2",
		RecipeYaml: validRecipeYAML(t, "dflash2"),
		WantPort:   int32(freePort(t)),
	})

	if result.Error != "" {
		t.Fatalf("unexpected error: %s", result.Error)
	}
	if len(rt.started) != 1 {
		t.Fatalf("Start called %d times, want 1", len(rt.started))
	}
}

func TestHandleDeployReportsInvalidRecipeYAML(t *testing.T) {
	rt := &fakeRuntime{}
	h := &Handlers{Runtime: rt, ModelDir: t.TempDir()}

	result := h.HandleDeploy(context.Background(), &pb.DeployCommand{
		RecipeId:   "dflash2",
		RecipeYaml: "not: [valid: yaml",
	})

	if result.Error == "" {
		t.Fatal("expected an error for malformed YAML, got none")
	}
	if result.RecipeId != "dflash2" {
		t.Fatalf("RecipeId = %q, want dflash2", result.RecipeId)
	}
	if len(rt.started) != 0 {
		t.Fatalf("Start called %d times, want 0", len(rt.started))
	}
}

func TestHandleDeployReportsRecipeValidationFailure(t *testing.T) {
	rt := &fakeRuntime{}
	h := &Handlers{Runtime: rt, ModelDir: t.TempDir()}

	// No Image: engine.BuildSpec calls recipe.Validate(), which requires one.
	rec := recipe.Recipe{ID: "dflash2", Kind: recipe.KindVLLM, Modality: recipe.ModalityText}
	out, err := yaml.Marshal(rec)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}

	result := h.HandleDeploy(context.Background(), &pb.DeployCommand{
		RecipeId:   "dflash2",
		RecipeYaml: string(out),
	})

	if result.Error == "" {
		t.Fatal("expected a validation error, got none")
	}
	if len(rt.started) != 0 {
		t.Fatalf("Start called %d times, want 0", len(rt.started))
	}
}

func TestHandleDeploySurfacesRuntimeStartError(t *testing.T) {
	rt := &fakeRuntime{startErr: errors.New("no capacity")}
	h := &Handlers{Runtime: rt, ModelDir: t.TempDir()}

	result := h.HandleDeploy(context.Background(), &pb.DeployCommand{
		RecipeId:   "dflash2",
		RecipeYaml: validRecipeYAML(t, "dflash2"),
	})

	if result.Error != "no capacity" {
		t.Fatalf("Error = %q, want %q", result.Error, "no capacity")
	}
}

func TestHandleUndeployStopsTheContainerByRecipeID(t *testing.T) {
	rt := &fakeRuntime{}
	h := &Handlers{Runtime: rt}

	result := h.HandleUndeploy(context.Background(), &pb.UndeployCommand{RecipeId: "dflash2"})

	if result.Error != "" {
		t.Fatalf("unexpected error: %s", result.Error)
	}
	if len(rt.stopped) != 1 || rt.stopped[0] != engine.ContainerName("dflash2") {
		t.Fatalf("stopped = %v, want [%s]", rt.stopped, engine.ContainerName("dflash2"))
	}
}

func TestHandleUndeploySurfacesRuntimeStopError(t *testing.T) {
	rt := &fakeRuntime{stopErr: errors.New("docker daemon unreachable")}
	h := &Handlers{Runtime: rt}

	result := h.HandleUndeploy(context.Background(), &pb.UndeployCommand{RecipeId: "dflash2"})

	if result.Error != "docker daemon unreachable" {
		t.Fatalf("Error = %q, want %q", result.Error, "docker daemon unreachable")
	}
}

func TestHandleFetchStartsADownloadAndReportsItsPhase(t *testing.T) {
	frt := &fakeFetchRuntime{}
	h := &Handlers{Fetch: &fetch.Manager{Runtime: frt, ModelDir: t.TempDir(), Image: "vllm/vllm-openai:latest"}}

	progress := h.HandleFetch(context.Background(), &pb.FetchCommand{Repo: "Inferact/Qwen3.8-27B-NVFP4"})

	if progress.Repo != "Inferact/Qwen3.8-27B-NVFP4" {
		t.Fatalf("Repo = %q, want the requested repo", progress.Repo)
	}
	if progress.Phase != string(fetch.PhaseDownloading) {
		t.Fatalf("Phase = %q, want %q", progress.Phase, fetch.PhaseDownloading)
	}
}

func TestHandleFetchReportsFailedPhaseOnInvalidRepo(t *testing.T) {
	frt := &fakeFetchRuntime{}
	h := &Handlers{Fetch: &fetch.Manager{Runtime: frt, ModelDir: t.TempDir(), Image: "vllm/vllm-openai:latest"}}

	// Not a well-formed "owner/name" HuggingFace repo id.
	progress := h.HandleFetch(context.Background(), &pb.FetchCommand{Repo: "not-a-valid-repo"})

	if progress.Phase != string(fetch.PhaseFailed) {
		t.Fatalf("Phase = %q, want %q", progress.Phase, fetch.PhaseFailed)
	}
}

// TestHandleFetchReportsDoneWithoutRestartingAnAlreadyCompletedJob guards
// the fix that makes repeated FetchCommand polling actually safe to observe
// completion with: a poll landing after a download has already finished
// successfully must report "done" straight away, not silently wipe out the
// finished job and restart the whole download. fetch.Manager.Start alone
// cannot make this distinction - its own logic treats ANY non-running job
// of the same name, success or failure alike, as stale leftover to remove
// and replace (see Start's doc comment) - so this only works because
// HandleFetch checks Status first and never reaches Start at all here.
//
// The fake job container here (keyed by fetch.Name(repo), the same name
// fetch.Manager.Start/Status derive internally) represents exactly the
// state a real completed download leaves behind: Status "exited", ExitCode
// 0 - distinct from the zero-value/absent-from-the-map state the other
// HandleFetch tests exercise for "never attempted."
func TestHandleFetchReportsDoneWithoutRestartingAnAlreadyCompletedJob(t *testing.T) {
	const repo = "Inferact/Qwen3.8-27B-NVFP4"
	frt := &fakeFetchRuntime{states: map[string]engine.ContainerState{
		fetch.Name(repo): {Status: "exited", ExitCode: 0},
	}}
	h := &Handlers{Fetch: &fetch.Manager{Runtime: frt, ModelDir: t.TempDir(), Image: "vllm/vllm-openai:latest"}}

	progress := h.HandleFetch(context.Background(), &pb.FetchCommand{Repo: repo})

	if progress.Phase != string(fetch.PhaseDone) {
		t.Fatalf("Phase = %q, want %q", progress.Phase, fetch.PhaseDone)
	}
	if frt.startCalls != 0 {
		t.Fatalf("StartJob called %d times, want 0 - a completed job must never be restarted by a poll", frt.startCalls)
	}
	if len(frt.removedJobs) != 0 {
		t.Fatalf("RemoveJob called for %v, want none - a completed job's container must be left alone", frt.removedJobs)
	}
}

// TestHandleFetchReportsFailedWithoutRestartingAFailedJob mirrors the "done"
// case above for a job that finished but failed: a poll must report
// "failed" directly from Status, not silently retry it via Start. A
// deliberate retry after failure is a separate, operator-initiated action -
// the single-node dashboard's own POST /api/fetch calls Start directly for
// that - not something a passive status poll should decide on its own.
func TestHandleFetchReportsFailedWithoutRestartingAFailedJob(t *testing.T) {
	const repo = "Inferact/Qwen3.8-27B-NVFP4"
	frt := &fakeFetchRuntime{states: map[string]engine.ContainerState{
		fetch.Name(repo): {Status: "exited", ExitCode: 1},
	}}
	h := &Handlers{Fetch: &fetch.Manager{Runtime: frt, ModelDir: t.TempDir(), Image: "vllm/vllm-openai:latest"}}

	progress := h.HandleFetch(context.Background(), &pb.FetchCommand{Repo: repo})

	if progress.Phase != string(fetch.PhaseFailed) {
		t.Fatalf("Phase = %q, want %q", progress.Phase, fetch.PhaseFailed)
	}
	if frt.startCalls != 0 {
		t.Fatalf("StartJob called %d times, want 0 - a failed job must not be silently restarted by a poll", frt.startCalls)
	}
}

// TestHandleFetchReportsDownloadingWithoutCallingStartAgain proves the
// still-in-progress case also short-circuits from Status rather than ever
// touching Start: Start's own fast path for a still-running job happens to
// be harmless (it just returns "already downloading"), but answering
// directly from Status means a poll against an in-flight download never has
// to reach Start - and its destructive remove-and-restart branch - at all
// unless the job is genuinely absent.
func TestHandleFetchReportsDownloadingWithoutCallingStartAgain(t *testing.T) {
	const repo = "Inferact/Qwen3.8-27B-NVFP4"
	frt := &fakeFetchRuntime{states: map[string]engine.ContainerState{
		fetch.Name(repo): {Status: "running"},
	}}
	h := &Handlers{Fetch: &fetch.Manager{Runtime: frt, ModelDir: t.TempDir(), Image: "vllm/vllm-openai:latest"}}

	progress := h.HandleFetch(context.Background(), &pb.FetchCommand{Repo: repo})

	if progress.Phase != string(fetch.PhaseDownloading) {
		t.Fatalf("Phase = %q, want %q", progress.Phase, fetch.PhaseDownloading)
	}
	if frt.startCalls != 0 {
		t.Fatalf("StartJob called %d times, want 0 - a poll against a still-running job should never reach Start", frt.startCalls)
	}
}

func TestHandleDeleteWeightsReturnsTheNotImplementedPlaceholder(t *testing.T) {
	h := &Handlers{ModelDir: t.TempDir()}

	result := h.HandleDeleteWeights(context.Background(), &pb.DeleteWeightsCommand{Repo: "Inferact/Qwen3.8-27B-NVFP4"})

	if result.Error == "" {
		t.Fatal("expected the deleteWeights placeholder to report an error, got none")
	}
	if result.BytesFreed != 0 {
		t.Fatalf("BytesFreed = %d, want 0", result.BytesFreed)
	}
}

func TestSnapshotReportsNodeIdentityAndLiveDeploymentsFromDocker(t *testing.T) {
	rt := &fakeRuntime{states: map[string]engine.ContainerState{
		engine.ContainerName("dflash2"): {Name: engine.ContainerName("dflash2"), Status: "running"},
	}}
	h := &Handlers{Runtime: rt}

	snap := h.Snapshot(context.Background(), "node-a", 80, 8)

	if snap.NodeId != "node-a" {
		t.Fatalf("NodeId = %q, want node-a", snap.NodeId)
	}
	if snap.PoolGib != 80 || snap.ReserveGib != 8 {
		t.Fatalf("PoolGib/ReserveGib = %v/%v, want 80/8", snap.PoolGib, snap.ReserveGib)
	}
	if len(snap.Deployments) != 1 {
		t.Fatalf("Deployments = %v, want 1 entry", snap.Deployments)
	}
	d := snap.Deployments[0]
	if d.RecipeId != "dflash2" {
		t.Fatalf("RecipeId = %q, want dflash2", d.RecipeId)
	}
	if d.Phase != "running" {
		t.Fatalf("Phase = %q, want running", d.Phase)
	}
	// This container was never deployed through h.HandleDeploy in this
	// process, so its footprint is genuinely unknown to it - 0 here is the
	// honest "unknown", not a claim the deployment has no footprint.
	if d.WeightsGib != 0 || d.KvGib != 0 {
		t.Fatalf("WeightsGib/KvGib = %v/%v, want 0/0 for a recipe never deployed through this handler", d.WeightsGib, d.KvGib)
	}
}

func TestSnapshotReportsTheDeclaredFootprintOfARecipeDeployedThroughThisHandler(t *testing.T) {
	name := engine.ContainerName("dflash2")
	rt := &fakeRuntime{states: map[string]engine.ContainerState{
		name: {Name: name, Status: "running"},
	}}
	h := &Handlers{Runtime: rt, ModelDir: t.TempDir()}

	rec := recipe.Recipe{
		ID: "dflash2", Kind: recipe.KindVLLM, Modality: recipe.ModalityText,
		Model: "Inferact/Qwen3.8-27B-NVFP4", Image: "vllm/vllm-openai:latest",
		Declared: recipe.Footprint{WeightsGiB: 24.5, KVGiB: 6},
	}
	recipeYAML, err := yaml.Marshal(rec)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}

	deployResult := h.HandleDeploy(context.Background(), &pb.DeployCommand{
		RecipeId:   "dflash2",
		RecipeYaml: string(recipeYAML),
	})
	if deployResult.Error != "" {
		t.Fatalf("HandleDeploy: unexpected error: %s", deployResult.Error)
	}

	snap := h.Snapshot(context.Background(), "node-a", 80, 8)

	if len(snap.Deployments) != 1 {
		t.Fatalf("Deployments = %v, want 1 entry", snap.Deployments)
	}
	d := snap.Deployments[0]
	if d.WeightsGib != 24.5 {
		t.Fatalf("WeightsGib = %v, want 24.5", d.WeightsGib)
	}
	if d.KvGib != 6 {
		t.Fatalf("KvGib = %v, want 6", d.KvGib)
	}
}

func TestSnapshotForgetsTheDeclaredFootprintAfterUndeploy(t *testing.T) {
	name := engine.ContainerName("dflash2")
	rt := &fakeRuntime{states: map[string]engine.ContainerState{
		name: {Name: name, Status: "running"},
	}}
	h := &Handlers{Runtime: rt, ModelDir: t.TempDir()}

	rec := recipe.Recipe{
		ID: "dflash2", Kind: recipe.KindVLLM, Modality: recipe.ModalityText,
		Model: "Inferact/Qwen3.8-27B-NVFP4", Image: "vllm/vllm-openai:latest",
		Declared: recipe.Footprint{WeightsGiB: 24.5, KVGiB: 6},
	}
	recipeYAML, err := yaml.Marshal(rec)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	if result := h.HandleDeploy(context.Background(), &pb.DeployCommand{
		RecipeId:   "dflash2",
		RecipeYaml: string(recipeYAML),
	}); result.Error != "" {
		t.Fatalf("HandleDeploy: unexpected error: %s", result.Error)
	}

	// Deliberately leave the container in rt.states across the undeploy
	// call, simulating Docker not having caught up yet: this isolates the
	// assertion to "did HandleUndeploy forget the cache entry" rather than
	// "did the container disappear from the deployment list", which would
	// be true either way.
	if result := h.HandleUndeploy(context.Background(), &pb.UndeployCommand{RecipeId: "dflash2"}); result.Error != "" {
		t.Fatalf("HandleUndeploy: unexpected error: %s", result.Error)
	}

	snap := h.Snapshot(context.Background(), "node-a", 80, 8)

	if len(snap.Deployments) != 1 {
		t.Fatalf("Deployments = %v, want 1 entry (container still present in Docker state)", snap.Deployments)
	}
	d := snap.Deployments[0]
	if d.WeightsGib != 0 || d.KvGib != 0 {
		t.Fatalf("WeightsGib/KvGib = %v/%v, want 0/0 - HandleUndeploy should have forgotten the cached footprint", d.WeightsGib, d.KvGib)
	}
}

func TestSnapshotToleratesADockerErrorAndReportsNoDeployments(t *testing.T) {
	rt := &fakeRuntime{statesErr: errors.New("docker daemon unreachable")}
	h := &Handlers{Runtime: rt}

	snap := h.Snapshot(context.Background(), "node-a", 80, 8)

	if len(snap.Deployments) != 0 {
		t.Fatalf("Deployments = %v, want none", snap.Deployments)
	}
}
