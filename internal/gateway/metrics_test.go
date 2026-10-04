package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codemug/sous/internal/grpcserver"
	"github.com/codemug/sous/internal/nodecatalog"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
	"github.com/codemug/sous/internal/recipe"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// metricsReply is how the fake souslet below answers one model's scrape.
type metricsReply struct {
	status int32  // 0 means 200
	body   string // sent in proxyChunkSize pieces, then an empty Eof chunk, as souslet does
	err    string // answer with an Error envelope instead - a container that refused the connection
	hang   bool   // never answer at all
}

// proxied is one request exactly as the fake souslet received it.
type proxied struct {
	head *pb.HTTPRequestHead
	body string
}

type fakeMetricsNode struct {
	mu  sync.Mutex
	got []proxied
}

func (f *fakeMetricsNode) requests() []proxied {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]proxied(nil), f.got...)
}

// dialFakeMetricsSouslet connects a fake souslet reporting deployed as
// running, which answers each proxied request by the model named in its JSON
// body - the only way a v0.22.5 souslet learns which container to forward to.
// Unlike dialFakeEchoingSouslet it reassembles the body from its chunks first,
// as the real one does, so a test can see exactly what was put on the wire.
func dialFakeMetricsSouslet(t *testing.T, srv *grpcserver.Server, nodeID string, deployed []string, replies map[string]metricsReply) *fakeMetricsNode {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	s := grpc.NewServer()
	pb.RegisterSousletServer(s, srv)
	go func() { _ = s.Serve(lis) }()

	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	stream, err := pb.NewSousletClient(conn).Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	var deps []*pb.DeploymentState
	for _, id := range deployed {
		deps = append(deps, &pb.DeploymentState{RecipeId: id, Phase: "running"})
	}
	if err := stream.Send(&pb.Envelope{Payload: &pb.Envelope_Snapshot{Snapshot: &pb.NodeSnapshot{
		NodeId: nodeID, Deployments: deps,
	}}}); err != nil {
		t.Fatalf("send snapshot: %v", err)
	}

	f := &fakeMetricsNode{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		heads := map[string]*pb.HTTPRequestHead{}
		bodies := map[string][]byte{}
		for {
			env, err := stream.Recv()
			if err != nil {
				return
			}
			sid := env.StreamId
			if h := env.GetHttpReqHead(); h != nil {
				heads[sid] = h
				continue
			}
			c := env.GetHttpReqChunk()
			if c == nil {
				continue
			}
			bodies[sid] = append(bodies[sid], c.Data...)
			if !c.Eof {
				continue
			}
			body := bodies[sid]
			f.mu.Lock()
			f.got = append(f.got, proxied{head: heads[sid], body: string(body)})
			f.mu.Unlock()

			var probe struct {
				Model string `json:"model"`
			}
			_ = json.Unmarshal(body, &probe)
			rep, ok := replies[probe.Model]
			if !ok {
				rep = metricsReply{err: "no local deployment for model " + probe.Model}
			}
			switch {
			case rep.hang:
				continue
			case rep.err != "":
				_ = stream.Send(&pb.Envelope{StreamId: sid, Payload: &pb.Envelope_Error{Error: &pb.Error{Message: rep.err}}})
				continue
			}
			status := rep.status
			if status == 0 {
				status = 200
			}
			_ = stream.Send(&pb.Envelope{StreamId: sid, Payload: &pb.Envelope_HttpRespHead{
				HttpRespHead: &pb.HTTPResponseHead{Status: status},
			}})
			for b := []byte(rep.body); len(b) > 0; {
				n := min(len(b), proxyChunkSize)
				_ = stream.Send(&pb.Envelope{StreamId: sid, Payload: &pb.Envelope_HttpRespChunk{
					HttpRespChunk: &pb.HTTPResponseChunk{Data: b[:n]},
				}})
				b = b[n:]
			}
			_ = stream.Send(&pb.Envelope{StreamId: sid, Payload: &pb.Envelope_HttpRespChunk{
				HttpRespChunk: &pb.HTTPResponseChunk{Eof: true},
			}})
		}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		v, ok := srv.Catalog().Node(nodeID)
		if ok && v.Connected && srv.Connected(nodeID) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("node %q never showed as connected", nodeID)
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Cleanup(func() {
		_ = stream.CloseSend()
		_ = conn.Close()
		s.Stop()
		<-done
	})
	return f
}

func vllm(ids ...string) fakeCat {
	c := fakeCat{}
	for _, id := range ids {
		c[id] = recipe.Recipe{ID: id, Kind: recipe.KindVLLM}
	}
	return c
}

func scrape(t *testing.T, m *Metrics, req *http.Request) (*httptest.ResponseRecorder, []string) {
	t.Helper()
	if req == nil {
		req = httptest.NewRequest("GET", "/metrics", nil)
	}
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	return rec, strings.Split(strings.TrimRight(rec.Body.String(), "\n"), "\n")
}

func has(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

func withPrefix(lines []string, prefix string) []string {
	var out []string
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			out = append(out, l)
		}
	}
	return out
}

const qwenMetrics = `# HELP vllm:num_requests_running Number of requests in model execution batches.
# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{model_name="qwen"} 2.0
# HELP vllm:e2e_request_latency_seconds Histogram of end to end request latency in seconds.
# TYPE vllm:e2e_request_latency_seconds histogram
vllm:e2e_request_latency_seconds_bucket{le="0.3",model_name="qwen"} 1.0
vllm:e2e_request_latency_seconds_bucket{le="+Inf",model_name="qwen"} 1.0
vllm:e2e_request_latency_seconds_count{model_name="qwen"} 1.0
vllm:e2e_request_latency_seconds_sum{model_name="qwen"} 0.25
`

// THE FEATURE. One scrape of sous-api returns a node's model's own metrics,
// each sample saying which node and recipe it came from, plus whether that
// model could be scraped at all.
func TestMetricsFederatesAModelsOwnMetricsWithNodeAndRecipeLabels(t *testing.T) {
	nodes := nodecatalog.New()
	gsrv := grpcserver.New(nodes, nil)
	dialFakeMetricsSouslet(t, gsrv, "gx10", []string{"qwen"}, map[string]metricsReply{"qwen": {body: qwenMetrics}})
	m := &Metrics{Nodes: nodes, GRPC: gsrv, Cat: vllm("qwen")}

	rec, lines := scrape(t, m, nil)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	for _, want := range []string{
		"# HELP vllm:num_requests_running Number of requests in model execution batches.",
		"# TYPE vllm:num_requests_running gauge",
		`vllm:num_requests_running{model_name="qwen",node="gx10",recipe="qwen"} 2.0`,
		"# TYPE vllm:e2e_request_latency_seconds histogram",
		`vllm:e2e_request_latency_seconds_bucket{le="0.3",model_name="qwen",node="gx10",recipe="qwen"} 1.0`,
		`vllm:e2e_request_latency_seconds_bucket{le="+Inf",model_name="qwen",node="gx10",recipe="qwen"} 1.0`,
		`vllm:e2e_request_latency_seconds_count{model_name="qwen",node="gx10",recipe="qwen"} 1.0`,
		`vllm:e2e_request_latency_seconds_sum{model_name="qwen",node="gx10",recipe="qwen"} 0.25`,
		"# TYPE sous_model_scrape_up gauge",
		`sous_model_scrape_up{node="gx10",recipe="qwen"} 1`,
		"# TYPE sous_model_scrape_duration_seconds gauge",
		"# TYPE sous_node_connected gauge",
		`sous_node_connected{node="gx10"} 1`,
	} {
		if !has(lines, want) {
			t.Errorf("missing line %q in:\n%s", want, rec.Body.String())
		}
	}
	if got := withPrefix(lines, `sous_model_scrape_duration_seconds{node="gx10",recipe="qwen"} `); len(got) != 1 {
		t.Errorf("duration lines = %q, want exactly one", got)
	}
}

// Two models of the same engine expose the same families. A HELP or TYPE
// line repeated for one family is a parse error to a strict scraper, and a
// family whose samples are split by another family's is one to the stricter
// ones - so each family is declared once, first seen wins, and every model's
// samples for it follow that one declaration together.
func TestMetricsHelpAndTypeAreEmittedOncePerFamilyAcrossModels(t *testing.T) {
	nodes := nodecatalog.New()
	gsrv := grpcserver.New(nodes, nil)
	dialFakeMetricsSouslet(t, gsrv, "node-a", []string{"alpha"}, map[string]metricsReply{"alpha": {body: `# HELP vllm:num_requests_running Alpha's wording.
# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{model_name="alpha"} 1
# a comment that is neither HELP nor TYPE
# HELP vllm:only_alpha Only alpha has this.
# TYPE vllm:only_alpha counter
vllm:only_alpha 7
`}})
	dialFakeMetricsSouslet(t, gsrv, "node-b", []string{"beta"}, map[string]metricsReply{"beta": {body: `# HELP vllm:num_requests_running Beta's wording.
# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{model_name="beta"} 3
`}})
	m := &Metrics{Nodes: nodes, GRPC: gsrv, Cat: vllm("alpha", "beta")}

	rec, lines := scrape(t, m, nil)
	body := rec.Body.String()
	if got := withPrefix(lines, "# HELP vllm:num_requests_running "); len(got) != 1 || got[0] != "# HELP vllm:num_requests_running Alpha's wording." {
		t.Errorf("HELP lines = %q, want only alpha's (first seen)", got)
	}
	if got := withPrefix(lines, "# TYPE vllm:num_requests_running "); len(got) != 1 {
		t.Errorf("TYPE lines = %q, want exactly one", got)
	}
	if strings.Contains(body, "neither HELP nor TYPE") {
		t.Errorf("an ordinary comment was passed through:\n%s", body)
	}
	// The family's two samples sit directly under its one declaration.
	want := "# HELP vllm:num_requests_running Alpha's wording.\n" +
		"# TYPE vllm:num_requests_running gauge\n" +
		`vllm:num_requests_running{model_name="alpha",node="node-a",recipe="alpha"} 1` + "\n" +
		`vllm:num_requests_running{model_name="beta",node="node-b",recipe="beta"} 3` + "\n"
	if !strings.Contains(body, want) {
		t.Errorf("family not grouped under one declaration; want\n%s\nin\n%s", want, body)
	}
	if !has(lines, `vllm:only_alpha{node="node-a",recipe="alpha"} 7`) {
		t.Errorf("alpha's own family is missing:\n%s", body)
	}
}

// Only vLLM serves Prometheus metrics at /metrics. A container or
// transformers recipe may answer that path with anything or nothing, and a
// recipe the catalog does not know cannot be said to be vLLM at all.
func TestMetricsFetchesOnlyVLLMRecipes(t *testing.T) {
	nodes := nodecatalog.New()
	gsrv := grpcserver.New(nodes, nil)
	all := map[string]metricsReply{}
	for _, id := range []string{"qwen", "whisper", "kokoro", "orphan"} {
		all[id] = metricsReply{body: "up 1\n"}
	}
	f := dialFakeMetricsSouslet(t, gsrv, "gx10", []string{"kokoro", "orphan", "qwen", "whisper"}, all)
	cat := fakeCat{
		"qwen":    {ID: "qwen", Kind: recipe.KindVLLM},
		"whisper": {ID: "whisper", Kind: recipe.KindContainer},
		"kokoro":  {ID: "kokoro", Kind: recipe.KindTransformers},
	}
	m := &Metrics{Nodes: nodes, GRPC: gsrv, Cat: cat}

	rec, lines := scrape(t, m, nil)
	got := f.requests()
	if len(got) != 1 || !strings.Contains(got[0].body, `"qwen"`) {
		t.Fatalf("node received %d requests %+v, want exactly one, for qwen", len(got), got)
	}
	if ups := withPrefix(lines, "sous_model_scrape_up{"); len(ups) != 1 || ups[0] != `sous_model_scrape_up{node="gx10",recipe="qwen"} 1` {
		t.Errorf("scrape_up lines = %q, want only qwen's:\n%s", ups, rec.Body.String())
	}
}

// HARD COMPATIBILITY CONSTRAINT. The deployed souslet (v0.22.5) forwards the
// method and path verbatim and finds the container by the JSON body's
// "model" - so that is the whole request. And nothing the scraper sent
// travels on: there is no credential on this listener to leak today, but a
// scraper configured with one by mistake must not hand it to a container.
func TestMetricsRequestTheNodeReceivesIsAPlainGETNamingTheModel(t *testing.T) {
	nodes := nodecatalog.New()
	gsrv := grpcserver.New(nodes, nil)
	f := dialFakeMetricsSouslet(t, gsrv, "gx10", []string{"qwen"}, map[string]metricsReply{"qwen": {body: qwenMetrics}})
	m := &Metrics{Nodes: nodes, GRPC: gsrv, Cat: vllm("qwen")}

	req := httptest.NewRequest("GET", "/metrics?x=1", nil)
	req.Header.Set("Authorization", "Bearer sk-secret")
	req.Header.Set("X-Api-Token", "sk-secret")
	req.Header.Set("Cookie", "sous_session=secret")
	req.Header.Set("Accept", "application/openmetrics-text")
	req.Header.Set("X-Prometheus-Scrape-Timeout-Seconds", "10")
	scrape(t, m, req)

	got := f.requests()
	if len(got) != 1 {
		t.Fatalf("node received %d requests, want 1", len(got))
	}
	h := got[0].head
	if h.GetMethod() != "GET" || h.GetPath() != "/metrics" {
		t.Errorf("request = %s %s, want GET /metrics", h.GetMethod(), h.GetPath())
	}
	if len(h.GetHeaders()) != 1 || h.GetHeaders()["Content-Type"] != "application/json" {
		t.Errorf("headers = %v, want only Content-Type: application/json", h.GetHeaders())
	}
	var b map[string]any
	if err := json.Unmarshal([]byte(got[0].body), &b); err != nil {
		t.Fatalf("body %q is not JSON: %v", got[0].body, err)
	}
	if len(b) != 1 || b["model"] != "qwen" {
		t.Errorf("body = %v, want exactly {\"model\":\"qwen\"}", b)
	}
}

// One broken model must cost the scrape nothing but its own samples: every
// way a fetch can fail - an error from the node, a non-200, a body that is
// not the text format, one past the size cap - is a 0 for that model, and
// the healthy model beside it is served in full.
func TestMetricsAFailingModelIsReportedDownAndTheRestAreServed(t *testing.T) {
	nodes := nodecatalog.New()
	gsrv := grpcserver.New(nodes, nil)
	dialFakeMetricsSouslet(t, gsrv, "gx10",
		[]string{"good", "refused", "unavailable", "garbage", "huge"},
		map[string]metricsReply{
			"good":        {body: "vllm:num_requests_running 1\n"},
			"refused":     {err: "dial tcp 127.0.0.1:18003: connect: connection refused"},
			"unavailable": {status: 503, body: "vllm:num_requests_running 9\n"},
			"garbage":     {body: "vllm:fine 1\n<html>not metrics</html>\n"},
			"huge":        {body: "vllm:big 1\n" + strings.Repeat("# padding\n", 300)},
		})
	m := &Metrics{Nodes: nodes, GRPC: gsrv, Cat: vllm("good", "refused", "unavailable", "garbage", "huge"), MaxBytes: 1024}

	rec, lines := scrape(t, m, nil)
	body := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 regardless", rec.Code)
	}
	if !has(lines, `vllm:num_requests_running{node="gx10",recipe="good"} 1`) {
		t.Errorf("the healthy model's sample is missing:\n%s", body)
	}
	if !has(lines, `sous_model_scrape_up{node="gx10",recipe="good"} 1`) {
		t.Errorf("good is not up:\n%s", body)
	}
	for _, id := range []string{"refused", "unavailable", "garbage", "huge"} {
		if !has(lines, `sous_model_scrape_up{node="gx10",recipe="`+id+`"} 0`) {
			t.Errorf("%s is not reported down:\n%s", id, body)
		}
		if len(withPrefix(lines, `sous_model_scrape_duration_seconds{node="gx10",recipe="`+id+`"} `)) != 1 {
			t.Errorf("%s has no duration:\n%s", id, body)
		}
	}
	// All or nothing per model, as for any scrape: half of a model whose body
	// did not parse is not served as though it were the whole.
	for _, s := range []string{"vllm:fine", "vllm:big", `recipe="unavailable"} 9`} {
		if strings.Contains(body, s) {
			t.Errorf("a failed model's sample %q was served:\n%s", s, body)
		}
	}
}

// A model that never answers costs the scrape its timeout and no more, and
// the scrape still carries everything else.
func TestMetricsAHungModelCostsOnlyItsTimeout(t *testing.T) {
	nodes := nodecatalog.New()
	gsrv := grpcserver.New(nodes, nil)
	dialFakeMetricsSouslet(t, gsrv, "gx10", []string{"good", "stuck"}, map[string]metricsReply{
		"good":  {body: "vllm:num_requests_running 1\n"},
		"stuck": {hang: true},
	})
	m := &Metrics{Nodes: nodes, GRPC: gsrv, Cat: vllm("good", "stuck"), Timeout: 200 * time.Millisecond}

	start := time.Now()
	rec, lines := scrape(t, m, nil)
	// ServeHTTP waits for every fetch, so returning at all proves the stuck
	// one's goroutine was released, not just abandoned.
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("scrape took %v against a 200ms per-model timeout", el)
	}
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !has(lines, `sous_model_scrape_up{node="gx10",recipe="stuck"} 0`) {
		t.Errorf("stuck is not reported down:\n%s", rec.Body.String())
	}
	if !has(lines, `vllm:num_requests_running{node="gx10",recipe="good"} 1`) || !has(lines, `sous_model_scrape_up{node="gx10",recipe="good"} 1`) {
		t.Errorf("the healthy model was not served beside the hung one:\n%s", rec.Body.String())
	}
}

// A node the catalog knows but that is not connected is reported as such,
// and nothing is sent to it - its last snapshot is not a promise that its
// models are there to ask.
func TestMetricsADisconnectedNodeIsReportedAndNotFetched(t *testing.T) {
	nodes := nodecatalog.New()
	gsrv := grpcserver.New(nodes, nil)
	dialFakeMetricsSouslet(t, gsrv, "live", []string{"qwen"}, map[string]metricsReply{"qwen": {body: "vllm:x 1\n"}})
	gone := dialFakeMetricsSouslet(t, gsrv, "gone", []string{"llama"}, map[string]metricsReply{"llama": {body: "vllm:x 2\n"}})
	nodes.MarkDisconnected("gone")
	m := &Metrics{Nodes: nodes, GRPC: gsrv, Cat: vllm("qwen", "llama")}

	rec, lines := scrape(t, m, nil)
	body := rec.Body.String()
	if !has(lines, `sous_node_connected{node="gone"} 0`) || !has(lines, `sous_node_connected{node="live"} 1`) {
		t.Errorf("node connectivity wrong:\n%s", body)
	}
	if n := len(gone.requests()); n != 0 {
		t.Errorf("the disconnected node was sent %d requests", n)
	}
	if strings.Contains(body, `recipe="llama"`) {
		t.Errorf("the disconnected node's model appears in the scrape:\n%s", body)
	}
	if !has(lines, `vllm:x{node="live",recipe="qwen"} 1`) {
		t.Errorf("the connected node's model is missing:\n%s", body)
	}
}

// This listener carries no authentication, so it must answer exactly one
// thing and nothing else - in particular none of the main mux's routes.
func TestMetricsListenerServesOnlyGETMetrics(t *testing.T) {
	nodes := nodecatalog.New()
	m := &Metrics{Nodes: nodes, GRPC: grpcserver.New(nodes, nil), Cat: fakeCat{}}
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/metrics", 200},
		{"POST", "/metrics", 405},
		{"PUT", "/metrics", 405},
		{"HEAD", "/metrics", 405},
		{"GET", "/", 404},
		{"GET", "/metrics/", 404},
		{"GET", "/v1/models", 404},
		{"POST", "/v1/chat/completions", 404},
		{"GET", "/api/recipes", 404},
	} {
		rec, _ := scrape(t, m, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, rec.Code, c.want)
		}
	}
}

func TestRelabelAddsNodeAndRecipeToEverySampleShape(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`foo 1`, `foo{node="n",recipe="r"} 1`},
		{`foo{a="b"} 1`, `foo{a="b",node="n",recipe="r"} 1`},
		{`foo 1 1700000000000`, `foo{node="n",recipe="r"} 1 1700000000000`},
		{`foo{a="b"} 1.5e+06 1700000000000`, `foo{a="b",node="n",recipe="r"} 1.5e+06 1700000000000`},
		{`foo{} 1`, `foo{node="n",recipe="r"} 1`},
		{`foo{a="b",} 1`, `foo{a="b",node="n",recipe="r"} 1`},
		{`foo {a="b"} 1`, `foo{a="b",node="n",recipe="r"} 1`},
		{"foo\t1", `foo{node="n",recipe="r"} 1`},
		{`  foo 1`, `foo{node="n",recipe="r"} 1`},
		{`foo{ a = "b" , c="d" } 1`, `foo{a="b",c="d",node="n",recipe="r"} 1`},
		// The name ends at the first brace; braces, commas and spaces inside a
		// quoted value are the value's.
		{`foo{path="/v1/{id}, x",le="+Inf"} 3`, `foo{path="/v1/{id}, x",le="+Inf",node="n",recipe="r"} 3`},
		// Escapes in an incoming value are carried through untouched.
		{`foo{a="say \"hi\" {",b="c\\d\ne"} 1`, `foo{a="say \"hi\" {",b="c\\d\ne",node="n",recipe="r"} 1`},
		{`vllm:prompt_tokens_total{model_name="m"} +Inf`, `vllm:prompt_tokens_total{model_name="m",node="n",recipe="r"} +Inf`},
		{`foo NaN`, `foo{node="n",recipe="r"} NaN`},
	} {
		_, got, err := relabel(c.in, "n", "r")
		if err != nil {
			t.Errorf("relabel(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("relabel(%q)\n got %s\nwant %s", c.in, got, c.want)
		}
	}
}

func TestRelabelEscapesTheLabelValuesItAdds(t *testing.T) {
	_, got, err := relabel(`foo 1`, `gx"10`, "a\\b\nc")
	if err != nil {
		t.Fatal(err)
	}
	if want := `foo{node="gx\"10",recipe="a\\b\nc"} 1`; got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
}

// A model that exports its own node or recipe label must not produce a sample
// with that label twice, which is a parse error. The incoming one is renamed
// the way Prometheus itself renames a target label it would clash with -
// exported_<name>, repeated while that too is taken - so nothing is lost.
func TestRelabelRenamesAnIncomingNodeOrRecipeLabel(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`foo{node="up",recipe="x"} 1`, `foo{exported_node="up",exported_recipe="x",node="n",recipe="r"} 1`},
		{`foo{node="a",exported_node="b"} 1`, `foo{exported_exported_node="a",exported_node="b",node="n",recipe="r"} 1`},
	} {
		_, got, err := relabel(c.in, "n", "r")
		if err != nil {
			t.Errorf("relabel(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("relabel(%q)\n got %s\nwant %s", c.in, got, c.want)
		}
	}
}

func TestRelabelRejectsWhatIsNotASample(t *testing.T) {
	for _, in := range []string{
		`{a="b"} 1`,
		`foo`,
		`foo{a="b"}`,
		`foo{a="b" 1`,
		`foo{a="b} 1`,
		`foo{a=b} 1`,
		`foo{="b"} 1`,
		`foo bar`,
		`foo 1 2 3`,
		`foo 1 notatime`,
		`<html>not metrics</html>`,
		`{"error":"not found"}`,
		// Go's float parser takes these; the exposition format does not, and
		// a store that fails a whole scrape on one bad line would lose every
		// model's metrics to one container's typo.
		`foo 0x1p3`,
		`foo 1_0`,
		`foo{a="b",a="c"} 1`,
		`foo{__name__="bar"} 1`,
		`foo{a="\q"} 1`,
		"foo{a=\"\xff\"} 1",
	} {
		if _, got, err := relabel(in, "n", "r"); err == nil {
			t.Errorf("relabel(%q) = %q, want an error", in, got)
		}
	}
}

// ---- against the real thing, and against a model that will not answer -------

// testdata/vllm-0.30.0-metrics.txt is what vLLM v0.30.0 really answered on
// asus-gx10 on 2026-10-05, fetched as this code fetches it: a GET carrying a
// JSON body. Parsing is all or nothing, so one line shape this parser does
// not know would blank a model's every metric - this is the check that the
// real exposition has none.
func TestMetricsParsesWhatVLLMReallyServes(t *testing.T) {
	body, err := os.ReadFile("testdata/vllm-0.30.0-metrics.txt")
	if err != nil {
		t.Fatal(err)
	}
	e, err := parseExposition(body, "asus-gx10", "qwen38-27b", 0)
	if err != nil {
		t.Fatalf("vLLM's own /metrics did not parse: %v", err)
	}
	wantSamples, wantTypes := 0, 0
	for _, l := range strings.Split(string(body), "\n") {
		switch {
		case strings.HasPrefix(l, "# TYPE "):
			wantTypes++
		case l != "" && !strings.HasPrefix(l, "#"):
			wantSamples++
		}
	}
	var b bytes.Buffer
	e.write(&b)
	gotSamples, gotTypes := 0, 0
	for _, l := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, "# TYPE "):
			gotTypes++
		case strings.HasPrefix(l, "#"):
		default:
			gotSamples++
			if !strings.Contains(l, `node="asus-gx10",recipe="qwen38-27b"} `) {
				t.Fatalf("a sample came out without its labels: %s", l)
			}
		}
	}
	if gotSamples != wantSamples || gotTypes != wantTypes {
		t.Fatalf("%d samples and %d TYPE lines in, %d and %d out", wantSamples, wantTypes, gotSamples, gotTypes)
	}
	if !strings.Contains(b.String(), `vllm:spec_decode_num_drafts_total{engine="0",model_name="qwen38-27b",node="asus-gx10",recipe="qwen38-27b"} `) {
		t.Error("the draft counter the dashboard reads is missing")
	}
}

// A 200 with nothing in it is not a model reporting metrics.
func TestMetricsABodyWithNoSamplesIsNotUp(t *testing.T) {
	nodes := nodecatalog.New()
	gsrv := grpcserver.New(nodes, nil)
	dialFakeMetricsSouslet(t, gsrv, "gx10", []string{"empty"}, map[string]metricsReply{
		"empty": {body: "# HELP nothing here\n\n"},
	})
	m := &Metrics{Nodes: nodes, GRPC: gsrv, Cat: vllm("empty")}
	rec, lines := scrape(t, m, nil)
	if !has(lines, `sous_model_scrape_up{node="gx10",recipe="empty"} 0`) {
		t.Errorf("a body with no samples was reported up:\n%s", rec.Body.String())
	}
}

// A SOUSLET CANNOT CANCEL A REQUEST IT IS FORWARDING. Giving up here frees
// this side; the node's request to the hung container stays open until the
// container answers. Asked again on every scrape, a model that never answers
// would collect one more stuck request on its node every 15 seconds for as
// long as it stays hung. So after a timeout it is left alone for a while.
func TestMetricsAHungModelIsNotAskedAgainUntilItsBackoffPasses(t *testing.T) {
	nodes := nodecatalog.New()
	gsrv := grpcserver.New(nodes, nil)
	node := dialFakeMetricsSouslet(t, gsrv, "gx10", []string{"good", "stuck"}, map[string]metricsReply{
		"good":  {body: "vllm:num_requests_running 1\n"},
		"stuck": {hang: true},
	})
	now := time.Unix(1_700_000_000, 0)
	m := &Metrics{Nodes: nodes, GRPC: gsrv, Cat: vllm("good", "stuck"), Timeout: 150 * time.Millisecond,
		MinInterval: -1, Now: func() time.Time { return now }}

	asked := func(model string) int {
		n := 0
		for _, r := range node.requests() {
			if strings.Contains(r.body, `"`+model+`"`) {
				n++
			}
		}
		return n
	}

	scrape(t, m, nil)
	if asked("stuck") != 1 {
		t.Fatalf("first scrape asked the hung model %d times, want 1", asked("stuck"))
	}

	// Straight away again: the healthy model is asked, the hung one is not.
	start := time.Now()
	rec, lines := scrape(t, m, nil)
	if time.Since(start) > 120*time.Millisecond {
		t.Errorf("a scrape that skips the hung model still waited for it (%v)", time.Since(start))
	}
	if asked("stuck") != 1 || asked("good") != 2 {
		t.Fatalf("second scrape: hung model asked %d times (want 1), healthy %d (want 2)", asked("stuck"), asked("good"))
	}
	if !has(lines, `sous_model_scrape_up{node="gx10",recipe="stuck"} 0`) || !has(lines, `sous_model_scrape_up{node="gx10",recipe="good"} 1`) {
		t.Errorf("a skipped model must still be reported down, and the other up:\n%s", rec.Body.String())
	}

	// Once the backoff has passed it gets another chance.
	now = now.Add(10 * time.Minute)
	scrape(t, m, nil)
	if asked("stuck") != 2 {
		t.Fatalf("after the backoff the hung model was asked %d times in all, want 2", asked("stuck"))
	}
}

// A failure that is not a timeout leaves nothing open on the node, so there
// is nothing to back off from: it is asked again on the next scrape, and is
// seen the moment it recovers.
func TestMetricsAModelThatRefusesIsAskedAgainOnTheNextScrape(t *testing.T) {
	nodes := nodecatalog.New()
	gsrv := grpcserver.New(nodes, nil)
	node := dialFakeMetricsSouslet(t, gsrv, "gx10", []string{"down"}, map[string]metricsReply{
		"down": {err: "connection refused"},
	})
	m := &Metrics{Nodes: nodes, GRPC: gsrv, Cat: vllm("down"), MinInterval: -1}
	scrape(t, m, nil)
	scrape(t, m, nil)
	if n := len(node.requests()); n != 2 {
		t.Fatalf("a refusing model was asked %d times over two scrapes, want 2", n)
	}
}

// THE LISTENER IS UNAUTHENTICATED, so anyone who can reach it can ask as
// often as they like. Scrapes run one at a time, and with the backoff above a
// burst of them costs a hung model one request, not one each.
func TestMetricsABurstOfScrapesAsksAHungModelOnce(t *testing.T) {
	nodes := nodecatalog.New()
	gsrv := grpcserver.New(nodes, nil)
	node := dialFakeMetricsSouslet(t, gsrv, "gx10", []string{"stuck"}, map[string]metricsReply{
		"stuck": {hang: true},
	})
	m := &Metrics{Nodes: nodes, GRPC: gsrv, Cat: vllm("stuck"), Timeout: 100 * time.Millisecond, MinInterval: -1}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/metrics", nil))
		}()
	}
	wg.Wait()
	if n := len(node.requests()); n != 1 {
		t.Fatalf("8 scrapes at once sent the hung model %d requests, want 1", n)
	}
}

// ---- an unauthenticated listener, and what it must survive ---------------------

// ANYONE WHO CAN REACH THE LISTENER CAN ASK AS OFTEN AS THEY LIKE. What they
// cannot do is make sous-api ask the nodes that often: a scrape inside
// MinInterval of the last one is answered from it.
func TestMetricsScrapesInsideTheIntervalAreAnsweredFromTheLastOne(t *testing.T) {
	nodes := nodecatalog.New()
	gsrv := grpcserver.New(nodes, nil)
	node := dialFakeMetricsSouslet(t, gsrv, "gx10", []string{"qwen"}, map[string]metricsReply{
		"qwen": {body: "vllm:num_requests_running 1\n"},
	})
	now := time.Unix(1_700_000_000, 0)
	m := &Metrics{Nodes: nodes, GRPC: gsrv, Cat: vllm("qwen"), Now: func() time.Time { return now }}

	for i := 0; i < 20; i++ {
		rec, lines := scrape(t, m, nil)
		if rec.Code != 200 || !has(lines, `vllm:num_requests_running{node="gx10",recipe="qwen"} 1`) {
			t.Fatalf("scrape %d was not served:\n%s", i, rec.Body.String())
		}
	}
	if n := len(node.requests()); n != 1 {
		t.Fatalf("20 scrapes in the same instant asked the node %d times, want 1", n)
	}
	now = now.Add(6 * time.Second)
	scrape(t, m, nil)
	if n := len(node.requests()); n != 2 {
		t.Fatalf("a scrape after the interval asked the node %d times in all, want 2", n)
	}
}

// A SCRAPER THAT HANGS UP MUST NOT UNDO THE BACKOFF. The fetch used to run
// under the request's context, so a caller who gave up before the model's
// timeout cancelled it - which was read as "the model answered", the backoff
// was cleared, and the node collected one more stuck request per such scrape.
// A fetch now belongs to the scrape, not to whoever asked for it.
func TestMetricsAScraperThatHangsUpDoesNotClearAHungModelsBackoff(t *testing.T) {
	nodes := nodecatalog.New()
	gsrv := grpcserver.New(nodes, nil)
	node := dialFakeMetricsSouslet(t, gsrv, "gx10", []string{"stuck"}, map[string]metricsReply{
		"stuck": {hang: true},
	})
	m := &Metrics{Nodes: nodes, GRPC: gsrv, Cat: vllm("stuck"), Timeout: 150 * time.Millisecond, MinInterval: -1}

	for i := 0; i < 4; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		m.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/metrics", nil).WithContext(ctx))
		cancel()
		time.Sleep(200 * time.Millisecond) // past the model's timeout, so the scrape it started has finished
	}
	if n := len(node.requests()); n != 1 {
		t.Fatalf("4 scrapes that hung up early sent the hung model %d requests, want 1", n)
	}
}

// blockedWriter is a client that connected, asked, and never read the answer.
type blockedWriter struct {
	h       http.Header
	release chan struct{}
}

func (b *blockedWriter) Header() http.Header         { return b.h }
func (b *blockedWriter) WriteHeader(int)             {}
func (b *blockedWriter) Write(p []byte) (int, error) { <-b.release; return len(p), nil }

// A client that never reads its answer must hold up nobody else. The scrape
// lock used to be held across the write, so one such client stopped every
// later scrape - on an unauthenticated port, for as long as it liked.
func TestMetricsAClientThatNeverReadsHoldsUpNobodyElse(t *testing.T) {
	nodes := nodecatalog.New()
	gsrv := grpcserver.New(nodes, nil)
	dialFakeMetricsSouslet(t, gsrv, "gx10", []string{"qwen"}, map[string]metricsReply{
		"qwen": {body: "vllm:num_requests_running 1\n"},
	})
	m := &Metrics{Nodes: nodes, GRPC: gsrv, Cat: vllm("qwen"), MinInterval: -1}

	stuck := &blockedWriter{h: http.Header{}, release: make(chan struct{})}
	defer close(stuck.release)
	go m.ServeHTTP(stuck, httptest.NewRequest("GET", "/metrics", nil))
	time.Sleep(100 * time.Millisecond) // it has its answer and is sitting in Write

	done := make(chan int, 1)
	go func() {
		rec, _ := scrape(t, m, nil)
		done <- rec.Code
	}()
	select {
	case code := <-done:
		if code != 200 {
			t.Fatalf("status = %d, want 200", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a scrape waited behind a client that never reads its answer")
	}
}

// A scrape waiting for its turn leaves when its client does, rather than
// queueing up work for nobody.
func TestMetricsAWaitingScrapeLeavesWhenItsClientDoes(t *testing.T) {
	nodes := nodecatalog.New()
	gsrv := grpcserver.New(nodes, nil)
	dialFakeMetricsSouslet(t, gsrv, "gx10", []string{"stuck"}, map[string]metricsReply{
		"stuck": {hang: true},
	})
	m := &Metrics{Nodes: nodes, GRPC: gsrv, Cat: vllm("stuck"), Timeout: 600 * time.Millisecond, MinInterval: -1}

	go m.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/metrics", nil)) // holds the turn for 600ms
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	m.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/metrics", nil).WithContext(ctx))
	if el := time.Since(start); el > 300*time.Millisecond {
		t.Fatalf("a scrape whose client left after 30ms still waited %v for its turn", el)
	}
}

// A MODEL IS SOMEONE ELSE'S IMAGE. 8 MiB of the shortest possible sample is
// two million lines, each of which grows by the labels added here - measured
// at 112 MiB of output from one model on one scrape. A real vLLM serves about
// 600.
func TestMetricsAModelWithTooManySamplesIsReportedDown(t *testing.T) {
	nodes := nodecatalog.New()
	gsrv := grpcserver.New(nodes, nil)
	dialFakeMetricsSouslet(t, gsrv, "gx10", []string{"flood", "good"}, map[string]metricsReply{
		"flood": {body: strings.Repeat("a 1\n", 501)},
		"good":  {body: "vllm:num_requests_running 1\n"},
	})
	m := &Metrics{Nodes: nodes, GRPC: gsrv, Cat: vllm("flood", "good"), MaxSamples: 500}
	rec, lines := scrape(t, m, nil)
	if !has(lines, `sous_model_scrape_up{node="gx10",recipe="flood"} 0`) || len(withPrefix(lines, "a{")) != 0 {
		t.Errorf("a model over the sample cap was served:\n%.400s", rec.Body.String())
	}
	if !has(lines, `sous_model_scrape_up{node="gx10",recipe="good"} 1`) {
		t.Errorf("the other model was not served:\n%.400s", rec.Body.String())
	}
}

// What one model sends must not be able to fail the scrape for all of them.
// A store that rejects a whole scrape on one malformed line would otherwise
// be blinded to every model by one container.
func TestParseExpositionRejectsWhatAStrictParserWould(t *testing.T) {
	for name, body := range map[string]string{
		"an unknown TYPE":          "# TYPE foo junk\nfoo 1\n",
		"words after the TYPE":     "# TYPE foo gauge junk\nfoo 1\n",
		"invalid UTF-8 in a HELP":  "# HELP foo \xff\nfoo 1\n",
		"invalid UTF-8 in a label": "foo{a=\"\xff\"} 1\n",
	} {
		if _, err := parseExposition([]byte(body), "n", "r", 0); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	for name, body := range map[string]string{
		"every TYPE the format has": "# TYPE a counter\na 1\n# TYPE b gauge\nb 1\n# TYPE c histogram\nc_count 1\n# TYPE d summary\nd_count 1\n# TYPE e untyped\ne 1\n",
		"the values it has":         "a NaN\nb +Inf\nc -Inf\nd 1.5e+09\ne -0.25\nf 3\n",
		"the escapes it has":        "a{b=\"x\\\\y\\\"z\\nw\"} 1\n",
	} {
		if _, err := parseExposition([]byte(body), "n", "r", 0); err != nil {
			t.Errorf("%s was rejected: %v", name, err)
		}
	}
}

// The backoff doubles from 30s and stops at 5 minutes, and a model that is no
// longer a target is forgotten.
func TestMetricsBackoffDoublesToItsCapAndForgetsWhatIsGone(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	m := &Metrics{Now: func() time.Time { return now }}
	tg := scrapeTarget{node: "gx10", recipe: "stuck"}

	for i, want := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
		m.timedOut(tg)
		now = now.Add(want - time.Second)
		if !m.skip(tg) {
			t.Fatalf("timeout %d: asked again before %v had passed", i+1, want)
		}
		now = now.Add(time.Second)
		if m.skip(tg) {
			t.Fatalf("timeout %d: still skipped after %v", i+1, want)
		}
	}

	m.timedOut(tg)
	m.forget([]scrapeTarget{{node: "gx10", recipe: "other"}})
	if m.skip(tg) {
		t.Fatal("a model that is no longer deployed kept its backoff")
	}
}

// Only a model that was asked and did not answer in time is backed off.
func TestMetricsOnlyAnUnansweredRequestIsATimeout(t *testing.T) {
	boom := errors.New("boom")
	for _, c := range []struct {
		name     string
		err      error
		sent     bool
		deadline bool
		want     outcome
	}{
		{"answered", nil, true, false, modelAnswered},
		{"refused straight away", boom, true, false, modelAnswered},
		{"asked, and the deadline passed", boom, true, true, modelTimedOut},
		{"never reached the node", boom, false, true, modelUnknown},
		{"node gone before it was asked", boom, false, false, modelUnknown},
	} {
		if got := classify(c.err, c.sent, c.deadline); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
