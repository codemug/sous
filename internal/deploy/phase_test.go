package deploy

import (
	"context"
	"github.com/codemug/sous/internal/store"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/codemug/sous/internal/engine"
)

func running(name string) engine.ContainerState {
	return engine.ContainerState{Name: name, Status: "running"}
}

// THE DISTINCTION THE WHOLE TYPE EXISTS FOR. A vLLM model is "running" for
// eight to ten minutes before it answers anything, and reporting that as
// deployed-and-fine is what sent clients at a port returning connection
// refused.
func TestRunningButNotAnsweringIsStarting(t *testing.T) {
	m := &Manager{Probe: &Prober{Host: "127.0.0.1", Timeout: 200 * time.Millisecond}}
	rec := Record{RecipeID: "x", HostPort: 1} // nothing listens on 1
	if got := m.Phase(context.Background(), rec, running("sous-x"), true); got != PhaseStarting {
		t.Fatalf("phase = %q, want %q", got, PhaseStarting)
	}
}

func TestAnsweringPortIsReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	m := &Manager{Probe: &Prober{Host: u.Hostname(), Timeout: time.Second}}
	rec := Record{RecipeID: "x", HostPort: port}
	if got := m.Phase(context.Background(), rec, running("sous-x"), true); got != PhaseReady {
		t.Fatalf("phase = %q, want %q", got, PhaseReady)
	}
}

// READY MEANS 200. An earlier version accepted any answer as proof the server
// was up, which gets the interesting case backwards: a server that binds its
// port before its engine has finished loading answers 404 or 503 here, and
// calling that ready sends traffic into a model that cannot serve it - the
// exact failure the phase exists to prevent.
func TestNon200IsNotReady(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusServiceUnavailable, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "not yet", code)
		}))
		u, _ := url.Parse(srv.URL)
		port, _ := strconv.Atoi(u.Port())

		m := &Manager{Probe: &Prober{Host: u.Hostname(), Timeout: time.Second}}
		got := m.Phase(context.Background(), Record{RecipeID: "x", HostPort: port}, running("sous-x"), true)
		srv.Close()
		if got == PhaseReady {
			t.Errorf("health returned %d and the model was called ready", code)
		}
		if got != PhaseStarting {
			t.Errorf("health %d gave phase %q, want %q", code, got, PhaseStarting)
		}
	}
}

// The probe must ask the HEALTH path, not whatever answers first. A server
// that 200s on / and 503s on /health is a server that is not ready.
func TestProbeAsksTheHealthPath(t *testing.T) {
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path
		if r.URL.Path == "/health" {
			http.Error(w, "loading", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	m := &Manager{Probe: &Prober{Host: u.Hostname(), Timeout: time.Second}}
	got := m.Phase(context.Background(), Record{RecipeID: "x", HostPort: port}, running("sous-x"), true)
	if asked != "/health" {
		t.Errorf("probed %q, want /health", asked)
	}
	if got == PhaseReady {
		t.Error("a 503 on /health was reported as ready because another path answered 200")
	}
}

func TestNoContainerIsGone(t *testing.T) {
	m := &Manager{}
	if got := m.Phase(context.Background(), Record{RecipeID: "x"}, engine.ContainerState{}, true); got != PhaseGone {
		t.Fatalf("phase = %q, want %q", got, PhaseGone)
	}
}

func TestCrashedStatesAreFailed(t *testing.T) {
	m := &Manager{}
	for _, st := range []engine.ContainerState{
		{Name: "sous-x", Status: "exited", ExitCode: 1},
		{Name: "sous-x", Status: "restarting"},
		{Name: "sous-x", Status: "dead"},
		{Name: "sous-x", Status: "exited", OOMKilled: true},
	} {
		if got := m.Phase(context.Background(), Record{RecipeID: "x"}, st, true); got != PhaseFailed {
			t.Errorf("%+v phase = %q, want %q", st, got, PhaseFailed)
		}
	}
}

// A clean exit is not a failure. Conflating them would paint every deliberate
// stop red.
func TestCleanExitIsGoneNotFailed(t *testing.T) {
	m := &Manager{}
	st := engine.ContainerState{Name: "sous-x", Status: "exited", ExitCode: 0}
	if got := m.Phase(context.Background(), Record{RecipeID: "x"}, st, true); got != PhaseGone {
		t.Fatalf("phase = %q, want %q", got, PhaseGone)
	}
}

// ORDERING TEST. A crash-looping container passes through "running"
// repeatedly, so a probe that catches it mid-loop would call it ready. Failure
// has to be checked first, and this is what stops that order being swapped.
func TestCrashLoopBeatsAHealthyProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	m := &Manager{Probe: &Prober{Host: u.Hostname(), Timeout: time.Second}}
	st := engine.ContainerState{Name: "sous-x", Status: "restarting", Restarts: 12}
	if got := m.Phase(context.Background(), Record{RecipeID: "x", HostPort: port}, st, true); got != PhaseFailed {
		t.Fatalf("phase = %q, want %q - a restarting container answered a probe and was believed", got, PhaseFailed)
	}
}

// ORDERING TEST. During a stop Docker still reports the container as running,
// so anything that trusts the runtime first will show a model being torn down
// as healthy - which is exactly the confusing state the async undeploy exists
// to remove.
func TestStoppingBeatsAStillRunningContainer(t *testing.T) {
	m := &Manager{}
	m.markStopping("x")
	if got := m.Phase(context.Background(), Record{RecipeID: "x"}, running("sous-x"), true); got != PhaseStopping {
		t.Fatalf("phase = %q, want %q", got, PhaseStopping)
	}
}

// If Sous is killed mid-undeploy the in-memory flag would otherwise be
// permanent, leaving a model stuck on "stopping" forever.
func TestStoppingExpires(t *testing.T) {
	m := &Manager{}
	m.stopMu.Lock()
	m.stopSet = map[string]time.Time{"x": time.Now().Add(-2 * stopMaxAge)}
	m.stopMu.Unlock()
	if m.stopping("x") {
		t.Fatal("a stale stop flag survived; the model would show stopping forever")
	}
}

// An unreachable runtime must not be turned into a confident claim. Reporting
// Gone here would tell an operator their models had vanished when what
// actually broke was the Docker socket.
func TestUnknownRuntimeDoesNotClaimGone(t *testing.T) {
	m := &Manager{}
	got := m.Phase(context.Background(), Record{RecipeID: "x"}, engine.ContainerState{}, false)
	if got == PhaseGone {
		t.Fatal("an unreachable runtime was reported as Gone")
	}
}

func TestProbeCacheAvoidsHammering(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	p := &Prober{Host: u.Hostname(), Timeout: time.Second}
	for i := 0; i < 5; i++ {
		p.Ready(context.Background(), port, DefaultHealthPath)
	}
	if hits != 1 {
		t.Fatalf("probed %d times for 5 reads; the cache is not holding", hits)
	}
}

func TestUndeployAsyncReturnsBeforeTheStopFinishes(t *testing.T) {
	f := newFake()
	f.stopDelay = 300 * time.Millisecond
	m := newManager(t, f)
	if _, err := m.Deploy(context.Background(), "kokoro", 0, false); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if err := m.UndeployAsync("kokoro"); err != nil {
		t.Fatalf("UndeployAsync: %v", err)
	}
	if el := time.Since(start); el > 100*time.Millisecond {
		t.Fatalf("returned after %v - it waited for the stop", el)
	}
	// And the phase says so while it runs, which is what the UI renders.
	if got := m.Phase(context.Background(), Record{RecipeID: "kokoro"}, running("sous-kokoro"), true); got != PhaseStopping {
		t.Fatalf("phase during stop = %q, want %q", got, PhaseStopping)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !m.stopping("kokoro") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("stop flag never cleared")
}

func TestUndeployAsyncRejectsBadID(t *testing.T) {
	m := &Manager{}
	if err := m.UndeployAsync("../escape"); err == nil {
		t.Fatal("accepted a traversal id")
	}
}

// Resident decides what counts against the pool. Getting this wrong makes the
// node look full when it is empty, or empty when it is full.
func TestResidentCountsOnlyPhasesHoldingMemory(t *testing.T) {
	hold := []Phase{PhaseStarting, PhaseReady, PhaseStopping}
	free := []Phase{PhaseFailed, PhaseGone}
	for _, p := range hold {
		if !Resident(p) {
			t.Errorf("%q holds memory but is not resident", p)
		}
	}
	for _, p := range free {
		if Resident(p) {
			t.Errorf("%q has no container yet counts against the pool", p)
		}
	}
}

// Clearing a record is not the same as stopping a container, and the button
// that says "clear the record" must not stop anything.
func TestForgetRecordLeavesContainersAlone(t *testing.T) {
	f := newFake()
	m := newManager(t, f)
	if _, err := m.Deploy(context.Background(), "kokoro", 0, false); err != nil {
		t.Fatal(err)
	}
	before := len(f.seen())
	if err := m.ForgetRecord("kokoro"); err != nil {
		t.Fatal(err)
	}
	if len(f.seen()) != before {
		t.Errorf("ForgetRecord touched the runtime: %v", f.seen()[before:])
	}
	if err := m.Store.ReadYAML(store.KindDeployment, "kokoro", &Record{}); err == nil {
		t.Error("the record survived")
	}
}

func TestForgetRecordIsIdempotentAndRejectsBadID(t *testing.T) {
	m := newManager(t, newFake())
	if err := m.ForgetRecord("kokoro"); err != nil {
		t.Errorf("forgetting an absent record errored: %v", err)
	}
	if err := m.ForgetRecord("../escape"); err == nil {
		t.Error("accepted a traversal id")
	}
}
