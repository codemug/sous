package grpcclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codemug/sous/internal/engine"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
	"github.com/codemug/sous/internal/ports"
	"github.com/codemug/sous/internal/recipe"
	"gopkg.in/yaml.v3"
)

// These tests are the production failure of 2026-09-11 and 2026-10-05: a node
// reboots (or souslet restarts), Docker's restart policy brings every model
// container back on the port it was created with, and the NEW souslet process
// - which used to keep every port only in memory - knows nothing about any of
// them. Each test builds its Handlers the way cmd/souslet does at startup:
// fresh, with no deploy ever having gone through it, against a runtime that
// already reports the containers the previous process left behind.

// dockerLikeRuntime is fakeRuntime with the one property these tests turn on:
// what it Starts it KEEPS, labels and port included, the way Docker keeps a
// container with a restart policy across a souslet restart - and what it Stops
// is gone. A second Handlers built on the same runtime sees exactly what the
// first one left behind, which is the restart under test.
type dockerLikeRuntime struct {
	fakeRuntime
	// onStates, when set, runs inside States after Docker's answer has been
	// read but before it is returned - the window in which a deploy or
	// undeploy can land while a reconciliation pass is still holding a now
	// stale answer.
	onStates func()
}

func (r *dockerLikeRuntime) Start(ctx context.Context, spec engine.Spec) (string, error) {
	id, err := r.fakeRuntime.Start(ctx, spec)
	if err != nil {
		return "", err
	}
	if r.states == nil {
		r.states = make(map[string]engine.ContainerState)
	}
	r.states[spec.Name] = engine.ContainerState{
		Name: spec.Name, Status: "running", HostPort: spec.HostPort, Labels: spec.Labels,
	}
	return id, nil
}

func (r *dockerLikeRuntime) Stop(ctx context.Context, name string) error {
	if err := r.fakeRuntime.Stop(ctx, name); err != nil {
		return err
	}
	delete(r.states, name)
	return nil
}

func (r *dockerLikeRuntime) States(ctx context.Context) (map[string]engine.ContainerState, error) {
	if r.statesErr != nil {
		return nil, r.statesErr
	}
	out := make(map[string]engine.ContainerState, len(r.states))
	for k, v := range r.states {
		out[k] = v
	}
	if r.onStates != nil {
		hook := r.onStates
		r.onStates = nil // once: the hook itself may call back into States
		hook()
	}
	return out, nil
}

// modelServer stands in for a model container that is up and serving: a real
// listener on 127.0.0.1, the address forwardToLocalContainer dials. hits counts
// what actually reached it, so a test can assert a request was NOT routed here.
func modelServer(t *testing.T, reply string) (port int, hits *atomic.Int32) {
	t.Helper()
	hits = new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	_, p, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split %s: %v", srv.URL, err)
	}
	port, err = strconv.Atoi(p)
	if err != nil {
		t.Fatalf("port %q: %v", p, err)
	}
	return port, hits
}

// proxy sends one proxied chat request for model through c the way
// handleProxyRequest does, returning the body the model answered with.
func proxy(t *testing.T, c *Client, model string) (string, error) {
	t.Helper()
	head := &pb.HTTPRequestHead{
		Method:  http.MethodPost,
		Path:    "/v1/chat/completions",
		Headers: map[string]string{"Content-Type": "application/json"},
	}
	resp, err := c.forwardToLocalContainer(context.Background(), head, []byte(`{"model":"`+model+`"}`))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read proxied response: %v", err)
	}
	return string(b), nil
}

func deploymentFor(t *testing.T, snap *pb.NodeSnapshot, recipeID string) *pb.DeploymentState {
	t.Helper()
	for _, d := range snap.Deployments {
		if d.RecipeId == recipeID {
			return d
		}
	}
	t.Fatalf("recipe %q missing from snapshot %v", recipeID, snap.Deployments)
	return nil
}

// (a) The snapshot reports the port Docker says the container publishes, not
// the 0 a process with an empty memory used to report.
func TestRestartedSousletSnapshotReportsThePortDockerPublishes(t *testing.T) {
	name := engine.ContainerName("dflash2")
	h := &Handlers{Runtime: &fakeRuntime{states: map[string]engine.ContainerState{
		name: {Name: name, Status: "running", HostPort: 18003},
	}}}

	d := deploymentFor(t, h.Snapshot(context.Background(), "asus-gx10", 121.6, 24), "dflash2")

	if d.HostPort != 18003 {
		t.Fatalf("snapshot HostPort = %d, want 18003 - the port Docker publishes for %s", d.HostPort, name)
	}
}

// (b) A proxied request reaches the container, with no snapshot having been
// taken first - the port is recovered lazily on the portFor miss, which is
// what the request path used to fail on with "no local deployment".
func TestRestartedSousletForwardsAProxiedRequestToTheRecoveredPort(t *testing.T) {
	port, hits := modelServer(t, "hello from dflash2")
	name := engine.ContainerName("dflash2")
	c := &Client{Handlers: &Handlers{Runtime: &fakeRuntime{states: map[string]engine.ContainerState{
		name: {Name: name, Status: "running", HostPort: port},
	}}}}

	body, err := proxy(t, c, "dflash2")
	if err != nil {
		t.Fatalf("proxied request failed: %v", err)
	}
	if body != "hello from dflash2" || hits.Load() != 1 {
		t.Fatalf("got %q with %d hits, want the model's own answer exactly once", body, hits.Load())
	}
	// And it is remembered, so the next request does not need Docker again.
	if p, ok := c.Handlers.portFor("dflash2"); !ok || p != port {
		t.Fatalf("portFor = (%d, %v), want (%d, true)", p, ok, port)
	}
}

// The connect-time snapshot is the other recovery point: it is the first
// thing a (re)connecting souslet sends, before any command or request can
// arrive on that connection, so the routing table is already whole by then.
func TestRestartedSousletRecoversThePortFromItsFirstSnapshot(t *testing.T) {
	name := engine.ContainerName("dflash2")
	h := &Handlers{Runtime: &fakeRuntime{states: map[string]engine.ContainerState{
		name: {Name: name, Status: "running", HostPort: 18003},
	}}}

	h.Snapshot(context.Background(), "asus-gx10", 121.6, 24)

	if p, ok := h.portFor("dflash2"); !ok || p != 18003 {
		t.Fatalf("portFor after the first snapshot = (%d, %v), want (18003, true)", p, ok)
	}
}

// (c) The allocator never hands out a port a recovered model holds - even
// when nothing is listening on it right now. A container Docker is restarting,
// or one that is stopped, holds no socket, so the bind probe alone reports its
// port free; the next deploy would take it, and the recovered model would then
// fail to bind when Docker brings it back (or, worse, come back first and have
// the NEW model's traffic routed to it).
func TestAllocatorSkipsPortsRecoveredModelsHoldEvenWithNothingListening(t *testing.T) {
	low := freePort(t)
	rt := &fakeRuntime{states: map[string]engine.ContainerState{
		engine.ContainerName("dflash2"): {Name: engine.ContainerName("dflash2"), Status: "running", HostPort: low},
		engine.ContainerName("kokoro"):  {Name: engine.ContainerName("kokoro"), Status: "restarting", HostPort: low + 1},
		engine.ContainerName("asr"):     {Name: engine.ContainerName("asr"), Status: "exited", HostPort: low + 2},
	}}
	h := &Handlers{
		Runtime: rt, ModelDir: t.TempDir(),
		Ports: ports.Allocator{Low: low, High: low + 10}, BindHost: "127.0.0.1",
	}
	// The premise: nothing is bound to any of the three, so binding alone
	// would hand out the first.
	for p := low; p <= low+2; p++ {
		if !h.Ports.IsFree("127.0.0.1", p) {
			t.Skipf("port %d is genuinely held on this machine; premise unavailable", p)
		}
	}

	res := h.HandleDeploy(context.Background(), &pb.DeployCommand{
		RecipeId: "qwen38", RecipeYaml: validRecipeYAML(t, "qwen38"),
	})
	if res.Error != "" {
		t.Fatalf("HandleDeploy: %s", res.Error)
	}
	if int(res.HostPort) >= low && int(res.HostPort) <= low+2 {
		t.Fatalf("allocated %d, which an existing model container holds", res.HostPort)
	}

	// An explicitly requested port is refused on the same grounds.
	res = h.HandleDeploy(context.Background(), &pb.DeployCommand{
		RecipeId: "qwen39", RecipeYaml: validRecipeYAML(t, "qwen39"), WantPort: int32(low + 1),
	})
	if res.Error == "" {
		t.Fatalf("deploying onto %d, which a restarting model holds, was accepted", low+1)
	}
}

// Without Docker's answer there is no way to know which ports existing models
// hold, so a deploy refuses rather than guessing - it would need Docker to
// start the container anyway.
func TestDeployRefusesWhenDockerCannotSayWhichPortsAreHeld(t *testing.T) {
	rt := &fakeRuntime{statesErr: errors.New("docker daemon unreachable")}
	h := &Handlers{Runtime: rt, ModelDir: t.TempDir()}

	res := h.HandleDeploy(context.Background(), &pb.DeployCommand{
		RecipeId: "dflash2", RecipeYaml: validRecipeYAML(t, "dflash2"),
	})
	if res.Error == "" {
		t.Fatal("deploy succeeded without knowing which ports existing models hold")
	}
	if len(rt.started) != 0 {
		t.Fatalf("Start called %d times, want 0", len(rt.started))
	}
}

// (d) Undeploying a recovered model forgets everything recovered for it.
func TestUndeployForgetsARecoveredModel(t *testing.T) {
	port, hits := modelServer(t, "should not be reached")
	name := engine.ContainerName("dflash2")
	rt := &dockerLikeRuntime{fakeRuntime: fakeRuntime{states: map[string]engine.ContainerState{
		name: {Name: name, Status: "running", HostPort: port, Labels: map[string]string{
			engine.ModelLabel: "Inferact/Qwen3.8-27B-NVFP4",
		}},
	}}}
	h := &Handlers{Runtime: rt, ModelDir: t.TempDir()}
	c := &Client{Handlers: h}
	h.Snapshot(context.Background(), "asus-gx10", 121.6, 24) // recovers

	if res := h.HandleUndeploy(context.Background(), &pb.UndeployCommand{RecipeId: "dflash2"}); res.Error != "" {
		t.Fatalf("HandleUndeploy: %s", res.Error)
	}

	if p, ok := h.portFor("dflash2"); ok {
		t.Fatalf("portFor still returns %d after undeploy", p)
	}
	if h.repoIsDeployed("Inferact/Qwen3.8-27B-NVFP4") {
		t.Fatal("the recovered model repo is still guarded against deletion after undeploy")
	}
	if _, err := proxy(t, c, "dflash2"); err == nil || !strings.Contains(err.Error(), "no local deployment") {
		t.Fatalf("proxy after undeploy: err = %v, want no local deployment", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("an undeployed model's port received %d requests", hits.Load())
	}
	if len(h.Snapshot(context.Background(), "asus-gx10", 121.6, 24).Deployments) != 0 {
		t.Fatal("snapshot still lists the undeployed model")
	}
}

// A reconciliation pass reads Docker without holding the lock, so an undeploy
// can complete while it holds an answer that still lists the container. It
// must not write that stale answer back over the undeploy.
func TestRecoveryDoesNotResurrectAModelUndeployedWhileDockerWasBeingRead(t *testing.T) {
	name := engine.ContainerName("dflash2")
	rt := &dockerLikeRuntime{fakeRuntime: fakeRuntime{states: map[string]engine.ContainerState{
		name: {Name: name, Status: "running", HostPort: 18003, Labels: map[string]string{
			engine.ModelLabel: "Inferact/Qwen3.8-27B-NVFP4",
		}},
	}}}
	h := &Handlers{Runtime: rt, ModelDir: t.TempDir()}
	rt.onStates = func() {
		if res := h.HandleUndeploy(context.Background(), &pb.UndeployCommand{RecipeId: "dflash2"}); res.Error != "" {
			t.Errorf("HandleUndeploy: %s", res.Error)
		}
	}

	h.Snapshot(context.Background(), "asus-gx10", 121.6, 24)

	if p, ok := h.portFor("dflash2"); ok {
		t.Fatalf("portFor = %d - a stale Docker answer resurrected an undeployed model", p)
	}
	if h.repoIsDeployed("Inferact/Qwen3.8-27B-NVFP4") {
		t.Fatal("a stale Docker answer re-guarded an undeployed model's repo")
	}
}

// (e) A container created by this souslet carries its declared footprint and
// model repo as labels, so a restarted souslet reports the same capacity
// figures the one that deployed it did, and keeps guarding its weights.
func TestRestartedSousletRecoversFootprintAndModelFromLabels(t *testing.T) {
	rt := &dockerLikeRuntime{}
	before := &Handlers{Runtime: rt, ModelDir: t.TempDir(), BindHost: "127.0.0.1"}
	rec := recipe.Recipe{
		ID: "dflash2", Kind: recipe.KindVLLM, Modality: recipe.ModalityText,
		Model: "Inferact/Qwen3.8-27B-NVFP4", Image: "vllm/vllm-openai:latest",
		Declared: recipe.Footprint{WeightsGiB: 24.5, KVGiB: 6.25},
	}
	recipeYAML, err := yaml.Marshal(rec)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	deployed := before.HandleDeploy(context.Background(), &pb.DeployCommand{RecipeId: "dflash2", RecipeYaml: string(recipeYAML)})
	if deployed.Error != "" {
		t.Fatalf("HandleDeploy: %s", deployed.Error)
	}

	// The restart: a new process, same Docker.
	after := &Handlers{Runtime: rt, ModelDir: before.ModelDir}
	d := deploymentFor(t, after.Snapshot(context.Background(), "asus-gx10", 121.6, 24), "dflash2")

	if d.WeightsGib != 24.5 || d.KvGib != 6.25 {
		t.Fatalf("WeightsGib/KvGib = %v/%v, want 24.5/6.25 from the container's labels", d.WeightsGib, d.KvGib)
	}
	if d.HostPort != deployed.HostPort {
		t.Fatalf("HostPort = %d, want %d", d.HostPort, deployed.HostPort)
	}
	if !after.repoIsDeployed("Inferact/Qwen3.8-27B-NVFP4") {
		t.Fatal("the restarted souslet would let this running model's weights be deleted")
	}
}

// (e, continued) A container created by an older souslet has no such labels.
// Its port is still recovered - Docker always knew that - but its footprint
// stays the honest zero "unknown" it has always been, never a made-up figure.
func TestRestartedSousletRecoversAnUnlabelledContainersPortButNotAFootprint(t *testing.T) {
	name := engine.ContainerName("dflash2")
	h := &Handlers{Runtime: &fakeRuntime{states: map[string]engine.ContainerState{
		name: {Name: name, Status: "running", HostPort: 18003},
	}}, ModelDir: t.TempDir()}

	d := deploymentFor(t, h.Snapshot(context.Background(), "asus-gx10", 121.6, 24), "dflash2")

	if d.HostPort != 18003 {
		t.Fatalf("HostPort = %d, want 18003", d.HostPort)
	}
	if d.WeightsGib != 0 || d.KvGib != 0 {
		t.Fatalf("WeightsGib/KvGib = %v/%v, want 0/0 for a container with no footprint labels", d.WeightsGib, d.KvGib)
	}
	if p, ok := h.portFor("dflash2"); !ok || p != 18003 {
		t.Fatalf("portFor = (%d, %v), want (18003, true)", p, ok)
	}
}

// A container that exists but is not running is reported (Phase says what it
// is) and its port stays reserved, but no request is routed to it: a port
// nothing of ours is listening on may be one something else is. Here it is
// another model's - the exact state the old allocator could leave behind, a
// stopped container still CONFIGURED for a port a later deploy was given.
func TestAStoppedOrRestartingModelIsNotRoutedTo(t *testing.T) {
	port, hits := modelServer(t, "this is kokoro, not dflash2")
	rt := &dockerLikeRuntime{fakeRuntime: fakeRuntime{states: map[string]engine.ContainerState{
		engine.ContainerName("dflash2"): {Name: engine.ContainerName("dflash2"), Status: "restarting", HostPort: port},
		engine.ContainerName("asr"):     {Name: engine.ContainerName("asr"), Status: "exited", HostPort: port},
		engine.ContainerName("kokoro"):  {Name: engine.ContainerName("kokoro"), Status: "running", HostPort: port},
	}}}
	h := &Handlers{Runtime: rt}
	c := &Client{Handlers: h}

	snap := h.Snapshot(context.Background(), "asus-gx10", 121.6, 24)
	if d := deploymentFor(t, snap, "dflash2"); d.Phase != "restarting" || d.HostPort != int32(port) {
		t.Fatalf("dflash2 reported as %s on %d, want restarting on %d", d.Phase, d.HostPort, port)
	}

	for _, id := range []string{"dflash2", "asr"} {
		_, err := proxy(t, c, id)
		if err == nil {
			t.Fatalf("a request for %s was routed although its container is not running", id)
		}
		if !strings.Contains(err.Error(), "not running") {
			t.Fatalf("proxy %s: err = %v, want it to say the model is not running", id, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("%d requests for a stopped model reached another model's port", hits.Load())
	}
	if body, err := proxy(t, c, "kokoro"); err != nil || body != "this is kokoro, not dflash2" {
		t.Fatalf("proxy kokoro = (%q, %v), want the running model to answer", body, err)
	}

	// Once Docker has it running again, the next request finds it.
	rt.states[engine.ContainerName("kokoro")] = engine.ContainerState{Name: engine.ContainerName("kokoro"), Status: "exited", HostPort: port}
	rt.states[engine.ContainerName("dflash2")] = engine.ContainerState{Name: engine.ContainerName("dflash2"), Status: "running", HostPort: port}
	if _, err := proxy(t, c, "dflash2"); err != nil {
		t.Fatalf("proxy dflash2 once running: %v", err)
	}
}

// A running container that publishes no port has nowhere to route to; that is
// reported as such rather than dialled as 127.0.0.1:0.
func TestAContainerPublishingNoPortIsReportedAsUnknownAndNotRoutedTo(t *testing.T) {
	name := engine.ContainerName("dflash2")
	c := &Client{Handlers: &Handlers{Runtime: &fakeRuntime{states: map[string]engine.ContainerState{
		name: {Name: name, Status: "running"},
	}}}}

	d := deploymentFor(t, c.Handlers.Snapshot(context.Background(), "asus-gx10", 121.6, 24), "dflash2")
	if d.HostPort != 0 {
		t.Fatalf("HostPort = %d, want 0 for a container that publishes none", d.HostPort)
	}
	_, err := proxy(t, c, "dflash2")
	if err == nil || !strings.Contains(err.Error(), "publishes no host port") {
		t.Fatalf("proxy: err = %v, want it to say no host port is published", err)
	}
}

// Docker briefly unavailable when souslet starts: nothing crashes, the
// snapshot is the same empty one it always was, a request fails with an error
// rather than a panic - and the next attempt, once Docker answers, recovers.
func TestRecoveryRetriesAfterDockerWasUnavailable(t *testing.T) {
	port, _ := modelServer(t, "hello from dflash2")
	name := engine.ContainerName("dflash2")
	rt := &fakeRuntime{statesErr: errors.New("Cannot connect to the Docker daemon"), states: map[string]engine.ContainerState{
		name: {Name: name, Status: "running", HostPort: port},
	}}
	h := &Handlers{Runtime: rt}
	c := &Client{Handlers: h}

	if snap := h.Snapshot(context.Background(), "asus-gx10", 121.6, 24); len(snap.Deployments) != 0 {
		t.Fatalf("Deployments = %v while Docker is down, want none", snap.Deployments)
	}
	if _, err := proxy(t, c, "dflash2"); err == nil {
		t.Fatal("a request was answered while Docker could not be asked where the model is")
	}

	rt.statesErr = nil
	if d := deploymentFor(t, h.Snapshot(context.Background(), "asus-gx10", 121.6, 24), "dflash2"); d.HostPort != int32(port) {
		t.Fatalf("HostPort = %d once Docker is back, want %d", d.HostPort, port)
	}
	if body, err := proxy(t, c, "dflash2"); err != nil || body != "hello from dflash2" {
		t.Fatalf("proxy once Docker is back = (%q, %v)", body, err)
	}
}

// A REMEMBERED PORT IS ONLY GOOD WHILE ITS CONTAINER IS RUNNING. Adopted or
// deployed, a port stayed in the table for as long as the process lived; if the
// container behind it exited and another container came up on the same port -
// possible for any pair created under the old allocator, which tested a port
// only by binding it - requests for the first model were answered by the
// second. Every snapshot pass now drops the port of a model that is not
// running, so the request finds out from Docker instead.
func TestARememberedPortIsForgottenWhenItsContainerStopsRunning(t *testing.T) {
	port, hits := modelServer(t, "hello from dflash2")
	kokoro, dflash2 := engine.ContainerName("kokoro"), engine.ContainerName("dflash2")
	rt := &dockerLikeRuntime{fakeRuntime: fakeRuntime{states: map[string]engine.ContainerState{
		kokoro: {Name: kokoro, Status: "running", HostPort: port},
	}}}
	h := &Handlers{Runtime: rt, ModelDir: t.TempDir()}
	c := &Client{Handlers: h}
	h.Snapshot(context.Background(), "asus-gx10", 121.6, 24)
	if p, ok := h.portFor("kokoro"); !ok || p != port {
		t.Fatalf("portFor(kokoro) = (%d, %v), want it recovered on %d", p, ok, port)
	}

	// kokoro exits; a second container created for the same port takes it.
	rt.states[kokoro] = engine.ContainerState{Name: kokoro, Status: "exited", HostPort: port}
	rt.states[dflash2] = engine.ContainerState{Name: dflash2, Status: "running", HostPort: port}
	h.Snapshot(context.Background(), "asus-gx10", 121.6, 24)

	body, err := proxy(t, c, "kokoro")
	if err == nil {
		t.Fatalf("a request for kokoro, which has exited, was answered: %q", body)
	}
	if !strings.Contains(err.Error(), "not running") {
		t.Errorf("error = %v, want it to say kokoro is not running", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("a request for kokoro reached another model's server %d time(s)", hits.Load())
	}
	// The model that IS on that port is still served.
	if body, err := proxy(t, c, "dflash2"); err != nil || body != "hello from dflash2" {
		t.Fatalf("dflash2: %q, %v", body, err)
	}
}

// A lookup that raced an undeploy must not forward on the answer it read
// before the undeploy landed: the port it names is being torn down, or is
// already someone else's.
func TestALookupThatRacedAnUndeployDoesNotForward(t *testing.T) {
	port, hits := modelServer(t, "hello from dflash2")
	name := engine.ContainerName("dflash2")
	rt := &dockerLikeRuntime{fakeRuntime: fakeRuntime{states: map[string]engine.ContainerState{
		name: {Name: name, Status: "running", HostPort: port},
	}}}
	h := &Handlers{Runtime: rt, ModelDir: t.TempDir()}
	c := &Client{Handlers: h}
	rt.onStates = func() {
		if res := h.HandleUndeploy(context.Background(), &pb.UndeployCommand{RecipeId: "dflash2"}); res.Error != "" {
			t.Errorf("HandleUndeploy: %s", res.Error)
		}
	}

	if body, err := proxy(t, c, "dflash2"); err == nil {
		t.Fatalf("a request that raced the model's undeploy was forwarded and answered: %q", body)
	}
	if hits.Load() != 0 {
		t.Fatalf("the undeployed model's port was dialled %d time(s)", hits.Load())
	}
}

// ---- a third-party image's own port ---------------------------------------

// exposingRuntime is fakeRuntime whose images declare a port, as kokoro's does.
type exposingRuntime struct {
	fakeRuntime
	exposed map[string]int
}

func (r *exposingRuntime) ImageExposedPort(_ context.Context, ref string) (int, error) {
	if p, ok := r.exposed[ref]; ok {
		return p, nil
	}
	return 0, fmt.Errorf("image %s exposes no port", ref)
}

// A `container` RECIPE'S IMAGE LISTENS WHERE ITS AUTHOR CHOSE. The node deploy
// path published every model's host port to container port 8000 - right for
// the kinds Sous writes the command for, wrong for kokoro, which serves on
// 8880: the container started, warmed up, reported running, and every request
// to its published port was a connection reset. The old single-node path read
// the recipe's container_port, then the image's EXPOSE; this path never did.
func TestDeployPublishesAContainerRecipesOwnPort(t *testing.T) {
	const img = "ghcr.io/remsky/kokoro-fastapi-gpu:latest"
	deploy := func(t *testing.T, rt *exposingRuntime, yaml string) engine.Spec {
		t.Helper()
		h := &Handlers{Runtime: rt, ModelDir: t.TempDir()}
		res := h.HandleDeploy(context.Background(), &pb.DeployCommand{RecipeId: "x", RecipeYaml: yaml, WantPort: 18001})
		if res.Error != "" {
			t.Fatalf("HandleDeploy: %s", res.Error)
		}
		if len(rt.started) != 1 {
			t.Fatalf("started %d containers, want 1", len(rt.started))
		}
		return rt.started[0]
	}
	kokoro := "id: kokoro\nkind: container\nmodality: tts\nimage: " + img + "\n"

	t.Run("the recipe's container_port wins", func(t *testing.T) {
		spec := deploy(t, &exposingRuntime{exposed: map[string]int{img: 9999}}, kokoro+"container_port: 8880\n")
		if spec.ContainerPort != 8880 || spec.HostPort != 18001 {
			t.Fatalf("published %d -> %d, want 18001 -> 8880", spec.HostPort, spec.ContainerPort)
		}
	})
	t.Run("otherwise the image's own exposed port", func(t *testing.T) {
		spec := deploy(t, &exposingRuntime{exposed: map[string]int{img: 8880}}, kokoro)
		if spec.ContainerPort != 8880 {
			t.Fatalf("container port = %d, want the image's 8880", spec.ContainerPort)
		}
	})
	t.Run("an image that exposes nothing still deploys, on 8000", func(t *testing.T) {
		spec := deploy(t, &exposingRuntime{}, kokoro)
		if spec.ContainerPort != 8000 {
			t.Fatalf("container port = %d, want the 8000 default", spec.ContainerPort)
		}
	})
	t.Run("a vLLM recipe is always 8000, whatever it or its image says", func(t *testing.T) {
		vimg := "vllm/vllm-openai:v0.28.0"
		spec := deploy(t, &exposingRuntime{exposed: map[string]int{vimg: 9000}},
			"id: qwen\nkind: vllm\nmodality: text\nmodel: Qwen/Qwen3.8-27B\nimage: "+vimg+"\ncontainer_port: 8880\ndeclared: {weights_gib: 1}\n")
		if spec.ContainerPort != 8000 {
			t.Fatalf("container port = %d, want 8000: Sous commands vLLM to listen there", spec.ContainerPort)
		}
	})
}

// The change counter is one for the whole node, so a lookup for one model can
// be invalidated by ANOTHER model's deploy landing while Docker is being read.
// That request has done nothing wrong and its model has not changed; failing
// it would turn every deploy into a chance of a 502 for whatever else was
// being looked up at that instant. The lookup reads Docker again instead.
func TestALookupThatRacedAnotherModelsDeployStillForwards(t *testing.T) {
	port, hits := modelServer(t, "hello from dflash2")
	name := engine.ContainerName("dflash2")
	rt := &dockerLikeRuntime{fakeRuntime: fakeRuntime{states: map[string]engine.ContainerState{
		name: {Name: name, Status: "running", HostPort: port},
	}}}
	h := &Handlers{Runtime: rt, ModelDir: t.TempDir()}
	c := &Client{Handlers: h}
	rt.onStates = func() {
		res := h.HandleDeploy(context.Background(), &pb.DeployCommand{
			RecipeId:   "asr",
			RecipeYaml: "id: asr\nkind: vllm\nmodality: text\nmodel: some/model\nimage: img\ndeclared: {weights_gib: 1}\n",
		})
		if res.Error != "" {
			t.Errorf("HandleDeploy(asr): %s", res.Error)
		}
	}

	body, err := proxy(t, c, "dflash2")
	if err != nil {
		t.Fatalf("a request for dflash2 failed because asr was deployed at the same moment: %v", err)
	}
	if body != "hello from dflash2" || hits.Load() != 1 {
		t.Fatalf("got %q with %d hits, want dflash2's own answer exactly once", body, hits.Load())
	}
}
