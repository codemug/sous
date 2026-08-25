package httpapi

import (
	"context"
	"encoding/json"
	"github.com/codemug/sous/internal/apikey"
	"github.com/codemug/sous/internal/fetch"
	"github.com/codemug/sous/internal/hf"
	"github.com/codemug/sous/internal/reqlog"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/codemug/sous/internal/auth"
	"github.com/codemug/sous/internal/capacity"
	"github.com/codemug/sous/internal/catalog"
	"github.com/codemug/sous/internal/deploy"
	"github.com/codemug/sous/internal/engine"
	"github.com/codemug/sous/internal/ports"
	"github.com/codemug/sous/internal/store"
)

type fakeRuntime struct {
	mu      sync.Mutex
	running map[string]bool
	states  map[string]engine.ContainerState
	jobs    map[string]engine.ContainerState
}

func (f *fakeRuntime) Start(_ context.Context, s engine.Spec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running[s.Name] = true
	return "cid-" + s.Name, nil
}
func (f *fakeRuntime) Stop(_ context.Context, n string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.running, n)
	return nil
}
func (f *fakeRuntime) Logs(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(
		"INFO Model loading took 24.87 GiB memory and 162.6 seconds\n" +
			"INFO Available KV cache memory: 45.67 GiB\n" +
			"INFO GPU KV cache size: 352,000 tokens\n" +
			"INFO Using FLASHINFER attention backend\n" +
			"INFO Profiling CUDA graph memory: PIECEWISE=7 (largest=32)\n")), nil
}

// Running reports what Start/Stop actually recorded.
//
// It used to return nil unconditionally, which was harmless while nothing read
// it - and became a lie the moment /api/status did. A drift test against that
// fake passed vacuously: every model looked dead, so "detects a dead
// container" could not fail.
func (f *fakeRuntime) Running(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.running))
	for n := range f.running {
		out = append(out, n)
	}
	return out, nil
}

func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	return newServerWithHub(t, t.TempDir())
}

// newServerWithHub seeds a fake HuggingFace cache: one repo a recipe uses and
// one nothing references, which is the shape the larder has to tell apart.
func newTestServerWithHub(t *testing.T) http.Handler {
	t.Helper()
	hub := t.TempDir()
	for _, name := range []string{
		"models--Inferact--Qwen3.8-27B-NVFP4",
		"models--Kwaipilot--KAT-Coder-V2.5-Dev",
	} {
		d := filepath.Join(hub, name, "snapshots", "abc")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "w.bin"), make([]byte, 4096), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return newServerWithHub(t, hub)
}

// newTestServerWithRuntime hands back the fake runtime as well, so a test can
// make a container vanish behind Sous's back - which is what the drift
// detection in /api/status exists to catch.
func newTestServerWithRuntime(t *testing.T) (http.Handler, *fakeRuntime) {
	t.Helper()
	rt := &fakeRuntime{running: map[string]bool{}}
	return buildServer(t, t.TempDir(), rt), rt
}

func newServerWithHub(t *testing.T, hub string) http.Handler {
	return buildServer(t, hub, &fakeRuntime{running: map[string]bool{}})
}

// buildServerAuth builds a server with auth ENABLED, for the login-flow tests.
// Everything else runs with auth off so the handler tests stay about handlers.
func buildServerAuth(t *testing.T, guard auth.Config) http.Handler {
	t.Helper()
	h, _ := buildServerWith(t, t.TempDir(), &fakeRuntime{running: map[string]bool{}}, guard)
	return h
}

func buildServer(t *testing.T, hub string, rt *fakeRuntime) http.Handler {
	h, _ := buildServerWith(t, hub, rt, auth.Config{Disabled: true})
	return h
}

// newTestServerWithReqLogDir hands back the request-log directory as well, so
// a test can read the audit trail directly rather than only through the
// summary the admin page shows.
func newTestServerWithReqLogDir(t *testing.T) (http.Handler, string) {
	t.Helper()
	return buildServerWith(t, t.TempDir(), &fakeRuntime{running: map[string]bool{}}, auth.Config{Disabled: true})
}

func buildServerWith(t *testing.T, hub string, rt *fakeRuntime, guard auth.Config) (http.Handler, string) {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := catalog.New(s)
	if _, err := c.SeedIfEmpty(); err != nil {
		t.Fatal(err)
	}
	keys := &apikey.Manager{Store: s}
	fx := &fetch.Manager{Runtime: rt, ModelDir: "/models", Image: "test-image"}
	guard.Keys = apikey.Guard{M: keys}
	m := &deploy.Manager{
		Store: s, Catalog: c, Runtime: rt,
		Planner:    capacity.Planner{PoolGiB: 121.6, ReserveGiB: 24, WarnFreeGiB: 12},
		Ports:      ports.Allocator{Low: 41300, High: 41400},
		BindHost:   "127.0.0.1",
		ModelDir:   "/models",
		DropCaches: func() error { return nil },
	}
	// A real token store on a temp dir: the tests that matter here are about
	// the token NOT leaking into recipes and exports, which a nil store could
	// not demonstrate.
	hfs, err := hf.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.Secrets = hfs
	fx.Secrets = hfs

	// A real writer and retention store on a temp dir, matching hfs above -
	// the tests that matter here include proving a request actually lands on
	// disk and that cleanup respects the configured window.
	reqLogDir := t.TempDir()
	reqLogW := &reqlog.Writer{Dir: reqLogDir}
	reqLogR, err := reqlog.NewRetentionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(m, c, keys, fx, hfs, reqLogW, reqLogR, 121.6, hub, t.TempDir(), guard)
	if err != nil {
		t.Fatal(err)
	}
	return h, reqLogDir
}

func TestListRecipesReturnsSeeds(t *testing.T) {
	h := newTestServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/recipes", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	var got []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if len(got) < 8 {
		t.Fatalf("want at least 8 seeded recipes, got %d", len(got))
	}
}

func TestPlanReportsMarginNotJustBoolean(t *testing.T) {
	h := newTestServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/plan/qwen38", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["margin_gib"]; !ok {
		t.Fatalf("plan must report a margin, got %v", got)
	}
}

func TestDeployRejectsUnknownRecipe(t *testing.T) {
	h := newTestServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/deploy/nosuchmodel", nil))
	if rr.Code < 400 {
		t.Fatalf("want 4xx for an unknown recipe, got %d", rr.Code)
	}
}

func TestDeployRejectsTraversalID(t *testing.T) {
	h := newTestServer(t)
	for _, bad := range []string{"..%2Fescape", "a%2Fb", "Qwen38"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/deploy/"+bad, nil))
		if rr.Code < 400 {
			t.Fatalf("accepted dangerous id %q with status %d", bad, rr.Code)
		}
	}
}

// A capacity refusal is a 409 carrying the margin, not a flat 500.
func TestCapacityRefusalIsConflictWithMargin(t *testing.T) {
	h := newTestServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/deploy/qwen38", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("first deploy: %d %s", rr.Code, rr.Body)
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/deploy/qwen36", nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409 for a capacity refusal, got %d: %s", rr.Code, rr.Body)
	}
	var got map[string]any
	json.Unmarshal(rr.Body.Bytes(), &got)
	if got["must_free"] == nil {
		t.Fatalf("refusal must name what to free: %v", got)
	}
}

func TestDeployThenListShowsIt(t *testing.T) {
	h := newTestServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/deploy/kokoro", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("deploy: %d %s", rr.Code, rr.Body)
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/deployments", nil))
	var got []map[string]any
	json.Unmarshal(rr.Body.Bytes(), &got)
	if len(got) != 1 {
		t.Fatalf("want 1 deployment, got %d", len(got))
	}
	if got[0]["host_port"] == nil {
		t.Fatal("deployment must report its allocated port")
	}
}

func TestModelsPageRenders(t *testing.T) {
	h := newTestServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/models", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	body := rr.Body.String()
	// "CPU only" was rendered by a zero-footprint recipe; none remain now
	// that whisper is deleted and kokoro runs on the GPU.
	for _, want := range []string{"qwen38", "kokoro", "Sous", "nemotron35"} {
		if !strings.Contains(body, want) {
			t.Errorf("catalog page missing %q", want)
		}
	}
}

// /deployments is RETIRED. It listed the running set, which the Node page
// already shows with more context - and two lists of the same objects is how
// the pool bar and the cards came to disagree. The old name 301s rather than
// quietly continuing to work.
func TestDeploymentsRedirectsToNode(t *testing.T) {
	h := newTestServer(t)
	rr := send(t, h, http.MethodGet, "/deployments", "", "")
	if rr.Code != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want 301", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want /", loc)
	}
}

// Catalog was renamed to Models for the same reason.
func TestCatalogRedirectsToModels(t *testing.T) {
	h := newTestServer(t)
	rr := send(t, h, http.MethodGet, "/catalog", "", "")
	if rr.Code != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want 301", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/models" {
		t.Errorf("Location = %q, want /models", loc)
	}
}

// The measured backend and graph mode must reach the page: they are the
// evidence for a throughput change that produces no error.
// Boot telemetry lives on the model page now that /deployments has retired -
// beside the config and logs it explains, rather than on a list that only
// repeated the Node page. Covered by TestModelPageRendersConfigTelemetryAndLogs
// in status_test.go, which already asserts the parsed backend.

func TestUnknownPathIs404(t *testing.T) {
	h := newTestServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
}

func TestHealthz(t *testing.T) {
	h := newTestServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
}

// ---------- larder ----------

func TestLarderAPIListsEntriesAndReclaimable(t *testing.T) {
	h := newTestServerWithHub(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/larder", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["entries"] == nil || got["reclaimable_bytes"] == nil || got["total_bytes"] == nil {
		t.Fatalf("larder response missing fields: %v", got)
	}
	// Only the unreferenced repo is reclaimable, so it must be less than total.
	if got["reclaimable_bytes"].(float64) >= got["total_bytes"].(float64) {
		t.Fatalf("referenced weights counted as reclaimable: %v", got)
	}
}

// The weights qwen38 uses must not be deletable while a recipe names them.
func TestLarderDeleteRefusesReferencedOverAPI(t *testing.T) {
	h := newTestServerWithHub(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost,
		"/api/larder/delete?repo=Inferact%2FQwen3.8-27B-NVFP4", nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409 for a referenced repo, got %d: %s", rr.Code, rr.Body)
	}
	var got map[string]string
	json.Unmarshal(rr.Body.Bytes(), &got)
	if got["reason"] == "" {
		t.Fatal("a guard must say why over the API too")
	}
}

func TestLarderDeleteStaleSucceeds(t *testing.T) {
	h := newTestServerWithHub(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost,
		"/api/larder/delete?repo=Kwaipilot%2FKAT-Coder-V2.5-Dev", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("stale weights should delete: %d %s", rr.Code, rr.Body)
	}
	var got map[string]any
	json.Unmarshal(rr.Body.Bytes(), &got)
	if got["freed_bytes"] == nil {
		t.Fatal("must report bytes freed")
	}
}

func TestLarderDeleteRejectsTraversal(t *testing.T) {
	h := newTestServerWithHub(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost,
		"/api/larder/delete?repo=..%2F..%2Fetc&force=true", nil))
	if rr.Code < 400 {
		t.Fatalf("accepted a traversal repo id: %d", rr.Code)
	}
}

func TestLarderPageRenders(t *testing.T) {
	h := newTestServerWithHub(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/larder", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	body := rr.Body.String()
	for _, want := range []string{"reclaimable", "KAT-Coder", "stale", "in use"} {
		if !strings.Contains(body, want) {
			t.Errorf("larder page missing %q", want)
		}
	}
}

// ---------- sources ----------

func TestSourcesPageRendersEmptyState(t *testing.T) {
	h := newTestServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/sources", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	if !strings.Contains(rr.Body.String(), "No sources configured") {
		t.Fatal("empty state not rendered")
	}
	// The page must state the guarantee, since it is the whole safety story.
	if !strings.Contains(rr.Body.String(), "never deploys") {
		t.Fatal("the page must say a fetch does not deploy")
	}
}

func TestAddSourceThenList(t *testing.T) {
	h := newTestServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost,
		"/api/sources?name=community&url=https://example.invalid/r.git", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("add: %d %s", rr.Code, rr.Body)
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/sources", nil))
	var got []map[string]any
	json.Unmarshal(rr.Body.Bytes(), &got)
	if len(got) != 1 || got[0]["name"] != "community" {
		t.Fatalf("source not stored: %v", got)
	}
	// An unspecified ref must default rather than producing an unusable source.
	if got[0]["ref"] != "main" {
		t.Fatalf("ref should default to main, got %v", got[0]["ref"])
	}
}

func TestAddSourceRequiresURL(t *testing.T) {
	h := newTestServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sources?name=x", nil))
	if rr.Code < 400 {
		t.Fatalf("accepted a source with no URL: %d", rr.Code)
	}
}

// A source that cannot be reached must report per-source rather than failing
// the whole fetch, and must never take down the catalog.
func TestFetchReportsPerSourceErrors(t *testing.T) {
	h := newTestServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost,
		"/api/sources?name=broken&url=/nonexistent/repo", nil))
	if rr.Code != http.StatusOK {
		t.Fatal("add failed")
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sources/fetch", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("fetch should report, not fail: %d %s", rr.Code, rr.Body)
	}
	var got []map[string]any
	json.Unmarshal(rr.Body.Bytes(), &got)
	if len(got) != 1 || got[0]["error"] == nil {
		t.Fatalf("a broken source must report its error: %v", got)
	}
	// And the catalog must still serve.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatal("a broken source took down the catalog page")
	}
}

// ImageExposedPort mirrors the real engine's lookup. 8880 is not arbitrary: it
// is what kokoro actually listens on, and hardcoding 8000 here would let the
// port-mapping bug this method exists to fix pass the tests.
func (f *fakeRuntime) ImageExposedPort(context.Context, string) (int, error) {
	return 8880, nil
}

// States mirrors Running for the fake: everything the fake knows about is
// running. Tests that need a crashed or stopped container set fake.states
// directly.
func (f *fakeRuntime) States(context.Context) (map[string]engine.ContainerState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.states != nil {
		return f.states, nil
	}
	out := map[string]engine.ContainerState{}
	for n := range f.running {
		out[n] = engine.ContainerState{Name: n, Status: "running"}
	}
	return out, nil
}

// Job methods so the fake also satisfies fetch.Runtime. Downloads are not what
// the handler tests are about, so these record rather than simulate.
func (f *fakeRuntime) StartJob(_ context.Context, s engine.JobSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.jobs == nil {
		f.jobs = map[string]engine.ContainerState{}
	}
	f.jobs[s.Name] = engine.ContainerState{Name: s.Name, Status: "running", Labels: s.Labels}
	return "job-" + s.Name, nil
}

func (f *fakeRuntime) JobStates(context.Context) (map[string]engine.ContainerState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]engine.ContainerState{}
	for k, v := range f.jobs {
		out[k] = v
	}
	return out, nil
}

func (f *fakeRuntime) RemoveJob(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.jobs, name)
	return nil
}
