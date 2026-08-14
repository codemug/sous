package deploy

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/codemug/sous/internal/capacity"
	"github.com/codemug/sous/internal/catalog"
	"github.com/codemug/sous/internal/engine"
	"github.com/codemug/sous/internal/ports"
	"github.com/codemug/sous/internal/store"
)

// fakeRuntime records the order of operations so the stop-before-start rule
// can be asserted directly rather than inferred.
type fakeRuntime struct {
	mu       sync.Mutex
	events   []string
	running  map[string]bool
	logs     string
	startErr error
}

func newFake() *fakeRuntime {
	return &fakeRuntime{
		running: map[string]bool{},
		logs: "INFO Model loading took 24.87 GiB memory and 162.6 seconds\n" +
			"INFO Available KV cache memory: 45.67 GiB\n" +
			"INFO GPU KV cache size: 352,000 tokens\n",
	}
}

func (f *fakeRuntime) Start(_ context.Context, s engine.Spec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return "", f.startErr
	}
	f.events = append(f.events, "start:"+s.Name)
	f.running[s.Name] = true
	return "cid-" + s.Name, nil
}

func (f *fakeRuntime) Stop(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "stop:"+name)
	delete(f.running, name)
	return nil
}

func (f *fakeRuntime) Logs(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(f.logs)), nil
}

func (f *fakeRuntime) Running(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []string{}
	for k := range f.running {
		out = append(out, k)
	}
	return out, nil
}

func (f *fakeRuntime) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func newManager(t *testing.T, rt Runtime) *Manager {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := catalog.New(s)
	if _, err := c.SeedIfEmpty(); err != nil {
		t.Fatal(err)
	}
	return &Manager{
		Store:      s,
		Catalog:    c,
		Runtime:    rt,
		Planner:    capacity.Planner{PoolGiB: 121.6, ReserveGiB: 24, WarnFreeGiB: 12},
		Ports:      ports.Allocator{Low: 41100, High: 41200},
		BindHost:   "127.0.0.1",
		ModelDir:   "/models",
		DropCaches: func() error { return nil },
	}
}

func TestDeployAllocatesPortAndRecords(t *testing.T) {
	m := newManager(t, newFake())
	rec, err := m.Deploy(context.Background(), "kokoro", 0, false)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if rec.HostPort < 41100 || rec.HostPort > 41200 {
		t.Fatalf("port outside range: %d", rec.HostPort)
	}
	if rec.ContainerID == "" {
		t.Fatal("no container id recorded")
	}
	// The record must survive a restart of Sous.
	var back Record
	if err := m.Store.ReadYAML(store.KindDeployment, "kokoro", &back); err != nil {
		t.Fatalf("record not persisted: %v", err)
	}
	if back.HostPort != rec.HostPort {
		t.Fatal("persisted record disagrees with the returned record")
	}
}

// The capacity model must refuse rather than let the node swap.
func TestDeployRefusedWhenItDoesNotFit(t *testing.T) {
	m := newManager(t, newFake())
	if _, err := m.Deploy(context.Background(), "qwen38", 0, false); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	_, err := m.Deploy(context.Background(), "qwen36", 0, false)
	if err == nil {
		t.Fatal("admitted 70.54 + 61.02 GiB into a 121.6 GiB pool")
	}
	var ce *CapacityError
	if !errors.As(err, &ce) {
		t.Fatalf("want *CapacityError, got %T: %v", err, err)
	}
	if len(ce.Result.MustFree) == 0 {
		t.Fatal("refusal must name what to free")
	}
}

func TestForceOverridesCapacity(t *testing.T) {
	m := newManager(t, newFake())
	if _, err := m.Deploy(context.Background(), "qwen38", 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Deploy(context.Background(), "qwen36", 0, true); err != nil {
		t.Fatalf("force must proceed: %v", err)
	}
}

// Redeploying must stop the old container BEFORE starting the new one. The
// other order is how an outgoing model keeps its port and memory while the
// incoming one fails both the bind and the startup gate - and the logs then
// blame the wrong model.
func TestRedeployStopsBeforeStarting(t *testing.T) {
	f := newFake()
	m := newManager(t, f)
	ctx := context.Background()
	if _, err := m.Deploy(ctx, "kokoro", 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Deploy(ctx, "kokoro", 0, false); err != nil {
		t.Fatal(err)
	}
	want := []string{"start:sous-kokoro", "stop:sous-kokoro", "start:sous-kokoro"}
	got := f.seen()
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %v, want %v", got, want)
		}
	}
}

// A redeploy replaces itself and is not additional load; counting it twice
// would refuse every redeploy of a large model.
func TestRedeployDoesNotDoubleCountItsOwnFootprint(t *testing.T) {
	m := newManager(t, newFake())
	ctx := context.Background()
	if _, err := m.Deploy(ctx, "qwen38", 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Deploy(ctx, "qwen38", 0, false); err != nil {
		t.Fatalf("redeploy of a resident model refused: %v", err)
	}
}

func TestPortOverrideRejectedWhenTaken(t *testing.T) {
	m := newManager(t, newFake())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	held := ln.Addr().(*net.TCPAddr).Port
	if _, err := m.Deploy(context.Background(), "kokoro", held, false); err == nil {
		t.Fatalf("accepted port %d while it is bound", held)
	}
}

func TestDropCachesRunsBeforeAnyContainerStarts(t *testing.T) {
	f := newFake()
	m := newManager(t, f)
	called := false
	m.DropCaches = func() error {
		if len(f.seen()) != 0 {
			t.Error("drop_caches ran after a container had already started")
		}
		called = true
		return nil
	}
	if _, err := m.Deploy(context.Background(), "kokoro", 0, false); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("drop_caches was not called")
	}
}

func TestObservationWrittenAfterDeploy(t *testing.T) {
	m := newManager(t, newFake())
	if _, err := m.Deploy(context.Background(), "qwen38", 0, false); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := m.Store.ReadYAML(store.KindObservation, "qwen38", &got); err != nil {
		t.Fatalf("no observation written: %v", err)
	}
	if got["kv_tokens"] == nil {
		t.Fatalf("observation missing kv_tokens: %v", got)
	}
}

// Capacity must prefer measured truth over the author's estimate once it
// exists, which is the whole point of writing observations back.
func TestFootprintPrefersObservation(t *testing.T) {
	m := newManager(t, newFake())
	r, _ := m.Catalog.Get("kokoro")
	if got := m.footprint(r); got != 0 {
		t.Fatalf("declared footprint should be 0 for kokoro, got %.2f", got)
	}
	if err := m.Store.WriteYAML(store.KindObservation, "kokoro",
		map[string]any{"weights_gib": 3.5, "kv_gib": 1.5}); err != nil {
		t.Fatal(err)
	}
	if got := m.footprint(r); got != 5.0 {
		t.Fatalf("want measured 5.0, got %.2f", got)
	}
}

func TestUndeployStopsAndForgets(t *testing.T) {
	f := newFake()
	m := newManager(t, f)
	ctx := context.Background()
	if _, err := m.Deploy(ctx, "kokoro", 0, false); err != nil {
		t.Fatal(err)
	}
	if err := m.Undeploy(ctx, "kokoro"); err != nil {
		t.Fatalf("Undeploy: %v", err)
	}
	if err := m.Store.ReadYAML(store.KindDeployment, "kokoro", &Record{}); err == nil {
		t.Fatal("deployment record survived undeploy")
	}
	if got := f.seen(); got[len(got)-1] != "stop:sous-kokoro" {
		t.Fatalf("last event should be a stop, got %v", got)
	}
}

func TestUndeployRejectsBadID(t *testing.T) {
	m := newManager(t, newFake())
	if err := m.Undeploy(context.Background(), "../escape"); err == nil {
		t.Fatal("accepted a traversal id")
	}
}

func TestDeployUnknownRecipeErrors(t *testing.T) {
	m := newManager(t, newFake())
	if _, err := m.Deploy(context.Background(), "nosuchmodel", 0, false); err == nil {
		t.Fatal("deployed a recipe that does not exist")
	}
}

// A start failure must not leave a deployment record claiming success.
func TestFailedStartWritesNoRecord(t *testing.T) {
	f := newFake()
	f.startErr = errors.New("boom")
	m := newManager(t, f)
	if _, err := m.Deploy(context.Background(), "kokoro", 0, false); err == nil {
		t.Fatal("Deploy reported success despite a start failure")
	}
	if err := m.Store.ReadYAML(store.KindDeployment, "kokoro", &Record{}); err == nil {
		t.Fatal("record written for a deployment that never started")
	}
}

func TestListDeployments(t *testing.T) {
	m := newManager(t, newFake())
	ctx := context.Background()
	if _, err := m.Deploy(ctx, "kokoro", 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Deploy(ctx, "asr", 0, false); err != nil {
		t.Fatal(err)
	}
	all, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 deployments, got %d", len(all))
	}
}

func TestPlanIsAvailableWithoutDeploying(t *testing.T) {
	f := newFake()
	m := newManager(t, f)
	res, err := m.Plan("qwen38")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Fits {
		t.Fatalf("qwen38 alone should fit: %+v", res)
	}
	if len(f.seen()) != 0 {
		t.Fatal("Plan started a container")
	}
	_ = strconv.Itoa(0)
}
