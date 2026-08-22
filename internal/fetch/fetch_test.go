package fetch

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/codemug/sous/internal/engine"
)

type fakeRT struct {
	started  []engine.JobSpec
	states   map[string]engine.ContainerState
	removed  []string
	logs     string
	startErr error
}

func (f *fakeRT) StartJob(_ context.Context, s engine.JobSpec) (string, error) {
	if f.startErr != nil {
		return "", f.startErr
	}
	f.started = append(f.started, s)
	return "cid", nil
}
func (f *fakeRT) JobStates(context.Context) (map[string]engine.ContainerState, error) {
	if f.states == nil {
		return map[string]engine.ContainerState{}, nil
	}
	return f.states, nil
}
func (f *fakeRT) RemoveJob(_ context.Context, name string) error {
	f.removed = append(f.removed, name)
	return nil
}
func (f *fakeRT) Logs(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(f.logs)), nil
}

func mgr(f *fakeRT) *Manager {
	return &Manager{Runtime: f, ModelDir: "/models", Image: "vllm:test"}
}

func TestStartRunsADownloadForTheRepo(t *testing.T) {
	f := &fakeRT{}
	j, err := mgr(f).Start(context.Background(), "ornith-ai/Ornith-1.5-35B-A3B-FP8")
	if err != nil {
		t.Fatal(err)
	}
	if j.Phase != PhaseDownloading {
		t.Errorf("phase = %q", j.Phase)
	}
	if len(f.started) != 1 {
		t.Fatalf("started %d jobs", len(f.started))
	}
	s := f.started[0]
	if s.Image != "vllm:test" {
		t.Errorf("image = %q", s.Image)
	}
	// The cache must be the SAME directory deployments mount, or the weights
	// land somewhere no model will look.
	if len(s.Binds) != 1 || !strings.HasPrefix(s.Binds[0], "/models:") {
		t.Errorf("binds = %v", s.Binds)
	}
	// The image's entrypoint is a model server; without overriding it the
	// command below arrives as arguments to vLLM and the container dies with
	// "Failed to infer device type" instead of downloading anything.
	if len(s.Entrypoint) == 0 {
		t.Error("no entrypoint override; the image's own server would run instead")
	}
	// The repo must arrive as its own argv entry, never spliced into a string.
	last := s.Cmd[len(s.Cmd)-1]
	if last != "ornith-ai/Ornith-1.5-35B-A3B-FP8" {
		t.Errorf("repo passed as %q", last)
	}
}

// A value that reaches a container command line must not be able to carry shell
// or path syntax, even though it is passed as argv rather than interpolated.
func TestMalformedRepoIsRefused(t *testing.T) {
	f := &fakeRT{}
	for _, bad := range []string{
		"", "no-slash", "/leading", "trailing/", "a/b/c",
		"owner/name; rm -rf /", "owner/$(whoami)", "../../etc/passwd",
		"owner/name && curl evil", "owner name", "owner/na me",
	} {
		if _, err := mgr(f).Start(context.Background(), bad); err == nil {
			t.Errorf("accepted repo id %q", bad)
		}
	}
	if len(f.started) != 0 {
		t.Fatalf("a malformed repo started %d containers", len(f.started))
	}
}

func TestValidRepoShapes(t *testing.T) {
	for _, ok := range []string{
		"Qwen/Qwen3.6-35B-A3B-FP8", "ornith-ai/Ornith-1.5-35B-A3B-FP8",
		"incoai/Qwen3.8-27B-DFlash2", "nvidia/nemotron-3.5-asr-streaming-0.6b",
		"a/b", "user_name/model_name",
	} {
		if !ValidRepo(ok) {
			t.Errorf("rejected valid repo %q", ok)
		}
	}
}

// Asking twice must join the running job, not start a second container writing
// into the same directory - which is a corrupted cache waiting to happen.
func TestStartIsIdempotentWhileRunning(t *testing.T) {
	repo := "Qwen/Qwen3.6-35B-A3B-FP8"
	f := &fakeRT{states: map[string]engine.ContainerState{
		Name(repo): {Name: Name(repo), Status: "running"},
	}}
	j, err := mgr(f).Start(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if j.Phase != PhaseDownloading {
		t.Errorf("phase = %q", j.Phase)
	}
	if len(f.started) != 0 {
		t.Fatal("a second container was started for a repo already downloading")
	}
}

// A finished job of the same name blocks a new container, so a retry has to
// clear it - otherwise a failed download can only be retried by hand.
func TestRetryClearsAFinishedJob(t *testing.T) {
	repo := "Qwen/Qwen3.6-35B-A3B-FP8"
	f := &fakeRT{states: map[string]engine.ContainerState{
		Name(repo): {Name: Name(repo), Status: "exited", ExitCode: 1},
	}}
	if _, err := mgr(f).Start(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	if len(f.removed) != 1 {
		t.Fatal("the failed job was not cleared, so the retry cannot create its container")
	}
	if len(f.started) != 1 {
		t.Fatal("the retry did not start")
	}
}

func TestStatusMapsContainerStateToPhase(t *testing.T) {
	repo := "a/b"
	cases := []struct {
		st   engine.ContainerState
		want Phase
	}{
		{engine.ContainerState{Name: Name(repo), Status: "running"}, PhaseDownloading},
		{engine.ContainerState{Name: Name(repo), Status: "exited", ExitCode: 0}, PhaseDone},
		{engine.ContainerState{Name: Name(repo), Status: "exited", ExitCode: 1}, PhaseFailed},
		{engine.ContainerState{Name: Name(repo), Status: "dead"}, PhaseFailed},
	}
	for _, c := range cases {
		f := &fakeRT{states: map[string]engine.ContainerState{Name(repo): c.st}}
		if got := mgr(f).Status(context.Background(), repo).Phase; got != c.want {
			t.Errorf("%+v -> %q, want %q", c.st, got, c.want)
		}
	}
	// No container at all is absent, which says nothing about the weights.
	f := &fakeRT{}
	if got := mgr(f).Status(context.Background(), repo).Phase; got != PhaseAbsent {
		t.Errorf("no job -> %q, want %q", got, PhaseAbsent)
	}
}

// A failure has to say WHY. A repo that does not exist and a disk that filled
// up both exit non-zero and are otherwise indistinguishable.
func TestFailureCarriesTheReason(t *testing.T) {
	repo := "a/b"
	f := &fakeRT{
		states: map[string]engine.ContainerState{
			Name(repo): {Name: Name(repo), Status: "exited", ExitCode: 1},
		},
		logs: "Fetching 3 files\nOSError: No space left on device\n",
	}
	j := mgr(f).Status(context.Background(), repo)
	if j.Phase != PhaseFailed {
		t.Fatalf("phase = %q", j.Phase)
	}
	if !strings.Contains(j.Detail, "No space") {
		t.Errorf("detail = %q, want the reason from the log", j.Detail)
	}
}

func TestFailureWithNoUsableLogStillExplainsItself(t *testing.T) {
	repo := "a/b"
	f := &fakeRT{
		states: map[string]engine.ContainerState{
			Name(repo): {Name: Name(repo), Status: "exited", ExitCode: 7},
		},
		logs: "",
	}
	j := mgr(f).Status(context.Background(), repo)
	if !strings.Contains(j.Detail, "7") {
		t.Errorf("detail = %q, want the exit code when the log says nothing", j.Detail)
	}
}

func TestProgressIsReportedWhileDownloading(t *testing.T) {
	repo := "a/b"
	f := &fakeRT{
		states: map[string]engine.ContainerState{
			Name(repo): {Name: Name(repo), Status: "running"},
		},
		// tqdm redraws with carriage returns, which is why the reader splits on
		// them as well as newlines.
		logs: "Fetching 16 files\r 12%|█▏ | 2/16 [00:31<03:12, 13.7s/it]\r 44%|████▍ | 7/16 [01:12<01:33, 10.4s/it]",
	}
	j := mgr(f).Status(context.Background(), repo)
	if !strings.Contains(j.Detail, "44%") {
		t.Errorf("detail = %q, want the LATEST progress line", j.Detail)
	}
}

// The container name has to survive a repo id containing characters Docker
// rejects, and stay recognisable in `docker ps`.
func TestNameIsContainerSafeAndReadable(t *testing.T) {
	n := Name("Qwen/Qwen3.6-35B-A3B-FP8")
	if strings.Contains(n, "/") {
		t.Errorf("name %q contains a slash", n)
	}
	if !strings.HasPrefix(n, engine.JobPrefix) {
		t.Errorf("name %q lacks the job prefix, so it could be mistaken for a deployment", n)
	}
	if !strings.Contains(n, "qwen") {
		t.Errorf("name %q is not recognisable", n)
	}
}

// A job container must never be mistaken for a deployment - the capacity
// planner counts deployments against the GPU pool, and a downloader uses none.
func TestJobPrefixIsDistinctFromDeployments(t *testing.T) {
	if strings.HasPrefix(engine.ContainerName("x"), engine.JobPrefix) {
		t.Fatal("a deployment name matches the job prefix")
	}
	if strings.HasPrefix(Name("a/b"), engine.ContainerName("")) &&
		!strings.HasPrefix(Name("a/b"), engine.JobPrefix) {
		t.Fatal("a job name would be listed as a deployment")
	}
}

// A HuggingFace id is case-sensitive and a Docker name is not, so the id has to
// travel in a label. Recovering it from the name produced
// "qwen/qwen3.6-35b-a3b-fp8" - which looks right, lists wrong, and 404s if
// anyone copies it out of the dashboard.
func TestRepoIDSurvivesWithItsCasing(t *testing.T) {
	repo := "Qwen/Qwen3.6-35B-A3B-FP8"
	f := &fakeRT{}
	if _, err := mgr(f).Start(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	if got := f.started[0].Labels[RepoLabel]; got != repo {
		t.Fatalf("label = %q, want %q", got, repo)
	}

	// And the listing must report it as typed.
	f2 := &fakeRT{states: map[string]engine.ContainerState{
		Name(repo): {Name: Name(repo), Status: "running", Labels: map[string]string{RepoLabel: repo}},
	}}
	jobs := mgr(f2).List(context.Background())
	if len(jobs) != 1 {
		t.Fatalf("listed %d jobs", len(jobs))
	}
	if jobs[0].Repo != repo {
		t.Errorf("listed as %q, want %q - the casing was lost", jobs[0].Repo, repo)
	}
}

type fakeSecrets struct{ env []string }

func (f fakeSecrets) Env() []string { return f.env }

// THE WHOLE REASON THIS FEATURE EXISTS. A gated repo ties licence acceptance to
// an ACCOUNT, so an anonymous download answers 401 no matter how many times the
// agreement was accepted in a browser.
func TestFetchJobCarriesTheToken(t *testing.T) {
	f := &fakeRT{}
	m := &Manager{Runtime: f, ModelDir: "/models", Image: "img",
		Secrets: fakeSecrets{env: []string{"HF_TOKEN=hf_secret"}}}

	if _, err := m.Start(context.Background(), "org/Gated-Model"); err != nil {
		t.Fatal(err)
	}
	if len(f.started) != 1 {
		t.Fatalf("jobs started = %d", len(f.started))
	}
	got := strings.Join(f.started[0].Env, " ")
	if !strings.Contains(got, "HF_TOKEN=hf_secret") {
		t.Errorf("download job env = %q, missing the token", got)
	}
	// The cache location must survive the append.
	if !strings.Contains(got, "HF_HOME=/root/.cache/huggingface") {
		t.Errorf("download job env = %q, lost HF_HOME", got)
	}
}

// Public repos are the normal case and must not need a token.
func TestFetchWithoutSecretsStillWorks(t *testing.T) {
	f := &fakeRT{}
	m := &Manager{Runtime: f, ModelDir: "/models", Image: "img"}
	if _, err := m.Start(context.Background(), "org/Public-Model"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(f.started[0].Env, " "), "HF_TOKEN") {
		t.Error("a token appeared with no secret store configured")
	}
}

// A STALL MUST READ AS A STALL. "3.3 of 28.8 GiB" beside a transfer that has
// not moved in two minutes reads as progress, which is exactly how a download
// stalling in 128 MiB bursts looked healthy for hours.
func TestDescribeSaysStalledWhenNothingIsMoving(t *testing.T) {
	got := describe(3_500_000_000, 30_900_000_000, 0)
	if !strings.Contains(got, "stalled") {
		t.Errorf("describe() = %q, want it to say stalled", got)
	}
	// And no ETA, because there is no rate to project from.
	if strings.Contains(got, "left") {
		t.Errorf("describe() = %q offers an ETA at zero rate", got)
	}
}

func TestDescribeCarriesTheNumbersThatMatter(t *testing.T) {
	got := describe(3_221_225_472, 30_923_764_531, 6_710_886)
	for _, want := range []string{"3.0 GiB", "28.8 GiB", "10%", "6 MiB/s", "left"} {
		if !strings.Contains(got, want) {
			t.Errorf("describe() = %q, missing %q", got, want)
		}
	}
}

// A repo whose size could not be fetched still reports what has landed.
func TestDescribeWithoutATotal(t *testing.T) {
	got := describe(1_048_576, 0, 1024)
	if !strings.Contains(got, "1 MiB") {
		t.Errorf("describe() = %q", got)
	}
	if strings.Contains(got, "%") {
		t.Errorf("describe() = %q invents a percentage with no total", got)
	}
}

// The structured line wins over any tqdm-looking noise in the same log.
func TestProgressPrefersTheStructuredLine(t *testing.T) {
	f := &fakeRT{
		states: map[string]engine.ContainerState{
			Name("org/m"): {Name: Name("org/m"), Status: "running"},
		},
		logs: "Fetching 20 files:  10%|#         | 2/20 [00:30<04:00]\n" +
			"sous-progress 3221225472 30923764531 6710886 10.4%\n",
	}
	j := mgr(f).Status(context.Background(), "org/m")
	if !strings.Contains(j.Detail, "3.0 GiB") || !strings.Contains(j.Detail, "left") {
		t.Errorf("Detail = %q, want the structured progress", j.Detail)
	}
	if strings.Contains(j.Detail, "|") {
		t.Errorf("Detail = %q fell back to the tqdm line", j.Detail)
	}
}

// A job started by an older Sous cannot be asked to print anything new, so the
// old heuristics have to keep working for as long as its container runs.
func TestProgressStillFallsBackToTqdm(t *testing.T) {
	f := &fakeRT{
		states: map[string]engine.ContainerState{
			Name("org/m"): {Name: Name("org/m"), Status: "running"},
		},
		logs: "Fetching 20 files:  10%|#         | 2/20 [00:30<04:00, 1.2MB/s]\n",
	}
	j := mgr(f).Status(context.Background(), "org/m")
	if !strings.Contains(j.Detail, "%|") {
		t.Errorf("Detail = %q, want the tqdm line for a pre-upgrade job", j.Detail)
	}
}
