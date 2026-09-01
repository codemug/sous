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
type fakeFetchRuntime struct {
	startErr error
	states   map[string]engine.ContainerState
}

var _ fetch.Runtime = (*fakeFetchRuntime)(nil)

func (f *fakeFetchRuntime) StartJob(context.Context, engine.JobSpec) (string, error) {
	if f.startErr != nil {
		return "", f.startErr
	}
	return "fake-job-id", nil
}

func (f *fakeFetchRuntime) JobStates(context.Context) (map[string]engine.ContainerState, error) {
	return f.states, nil
}

func (f *fakeFetchRuntime) RemoveJob(context.Context, string) error { return nil }

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

	result := h.HandleDeploy(context.Background(), &pb.DeployCommand{
		RecipeId:   "dflash2",
		RecipeYaml: validRecipeYAML(t, "dflash2"),
		WantPort:   8123,
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
	if result.HostPort != 8123 {
		t.Fatalf("HostPort = %d, want 8123", result.HostPort)
	}
	if len(rt.started) != 1 {
		t.Fatalf("Start called %d times, want 1", len(rt.started))
	}
	if got, want := rt.started[0].Name, engine.ContainerName("dflash2"); got != want {
		t.Fatalf("started container name = %q, want %q", got, want)
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
}

func TestSnapshotToleratesADockerErrorAndReportsNoDeployments(t *testing.T) {
	rt := &fakeRuntime{statesErr: errors.New("docker daemon unreachable")}
	h := &Handlers{Runtime: rt}

	snap := h.Snapshot(context.Background(), "node-a", 80, 8)

	if len(snap.Deployments) != 0 {
		t.Fatalf("Deployments = %v, want none", snap.Deployments)
	}
}
