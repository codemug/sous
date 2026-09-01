package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codemug/sous/internal/auth"
	"github.com/codemug/sous/internal/deploy"
	"github.com/codemug/sous/internal/engine"
	"github.com/codemug/sous/internal/grpcserver"
	"github.com/codemug/sous/internal/nodecatalog"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
	"github.com/codemug/sous/internal/recipe"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type fakeRes struct {
	recs   []deploy.Record
	phases map[string]deploy.Phase
}

func (f *fakeRes) List() ([]deploy.Record, error) { return f.recs, nil }
func (f *fakeRes) States(context.Context) (map[string]engine.ContainerState, error) {
	return map[string]engine.ContainerState{}, nil
}
func (f *fakeRes) Phase(_ context.Context, rec deploy.Record, _ engine.ContainerState, _ bool) deploy.Phase {
	if p, ok := f.phases[rec.RecipeID]; ok {
		return p
	}
	return deploy.PhaseReady
}

type fakeCat map[string]recipe.Recipe

func (c fakeCat) Get(id string) (recipe.Recipe, error) {
	r, ok := c[id]
	if !ok {
		return recipe.Recipe{}, fmt.Errorf("no recipe %q", id)
	}
	return r, nil
}

// upstream stands in for a served model. It records what it was asked for.
func upstream(t *testing.T, body *string) (*httptest.Server, int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if body != nil {
			*body = string(b)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	return srv, port
}

func newGW(res *fakeRes, cat fakeCat, host string) *Gateway {
	return &Gateway{Res: res, Cat: cat, Host: host}
}

func TestListModelsUsesAliases(t *testing.T) {
	res := &fakeRes{recs: []deploy.Record{{RecipeID: "ornith15", HostPort: 8000}}}
	cat := fakeCat{"ornith15": {ID: "ornith15", ServedAs: []string{"ornith"}, Modality: recipe.ModalityText}}
	rr := httptest.NewRecorder()
	newGW(res, cat, "127.0.0.1").ListModels(rr, httptest.NewRequest("GET", "/v1/models", nil))

	var out struct {
		Object string     `json:"object"`
		Data   []modelObj `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "list" || len(out.Data) != 1 {
		t.Fatalf("body = %s", rr.Body.String())
	}
	if out.Data[0].ID != "ornith" {
		t.Errorf("id = %q, want the alias %q", out.Data[0].ID, "ornith")
	}
	if out.Data[0].RecipeID != "ornith15" {
		t.Errorf("recipe_id = %q", out.Data[0].RecipeID)
	}
}

// A model that is still loading must still be LISTED. Hiding it makes a client
// polling /v1/models conclude it does not exist, which is a worse answer than
// "it exists and is not ready".
func TestListModelsIncludesNotReadyOnesWithPhase(t *testing.T) {
	res := &fakeRes{
		recs:   []deploy.Record{{RecipeID: "a", HostPort: 1}, {RecipeID: "b", HostPort: 2}},
		phases: map[string]deploy.Phase{"a": deploy.PhaseStarting, "b": deploy.PhaseReady},
	}
	cat := fakeCat{"a": {ID: "a", ServedAs: []string{"aa"}}, "b": {ID: "b", ServedAs: []string{"bb"}}}
	rr := httptest.NewRecorder()
	newGW(res, cat, "127.0.0.1").ListModels(rr, httptest.NewRequest("GET", "/v1/models", nil))

	var out struct{ Data []modelObj }
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if len(out.Data) != 2 {
		t.Fatalf("want both models listed, got %d", len(out.Data))
	}
	got := map[string]string{}
	for _, m := range out.Data {
		got[m.ID] = m.Phase
	}
	if got["aa"] != string(deploy.PhaseStarting) || got["bb"] != string(deploy.PhaseReady) {
		t.Errorf("phases = %v", got)
	}
}

func TestProxyReachesTheRightUpstream(t *testing.T) {
	var seen string
	srv, port := upstream(t, &seen)
	defer srv.Close()

	res := &fakeRes{recs: []deploy.Record{{RecipeID: "ornith15", HostPort: port}}}
	cat := fakeCat{"ornith15": {ID: "ornith15", ServedAs: []string{"ornith"}}}

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"ornith","messages":[{"role":"user","content":"hi"}]}`))
	rr := httptest.NewRecorder()
	newGW(res, cat, "127.0.0.1").Proxy(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(seen, `"messages"`) {
		t.Errorf("upstream did not receive the body: %s", seen)
	}
}

// THE REWRITE THAT MAKES RECIPE IDS USABLE. vLLM answers to its
// --served-model-name. A caller who addressed the model by its recipe id would
// otherwise get a 404 from the upstream for a model the gateway just listed.
func TestRecipeIDIsRewrittenToTheServedName(t *testing.T) {
	var seen string
	srv, port := upstream(t, &seen)
	defer srv.Close()

	res := &fakeRes{recs: []deploy.Record{{RecipeID: "ornith15", HostPort: port}}}
	cat := fakeCat{"ornith15": {ID: "ornith15", ServedAs: []string{"ornith"}}}

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"ornith15","messages":[]}`))
	rr := httptest.NewRecorder()
	newGW(res, cat, "127.0.0.1").Proxy(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(seen), &got); err != nil {
		t.Fatalf("upstream body not JSON: %s", seen)
	}
	if got["model"] != "ornith" {
		t.Errorf("upstream asked for model %v, want %q", got["model"], "ornith")
	}
}

// Rewriting must not drop fields the gateway does not know about, which is
// most of them. Re-encoding from a typed struct would silently delete these.
func TestRewritePreservesUnknownFields(t *testing.T) {
	out, err := rewriteModel([]byte(`{"model":"a","temperature":0.7,"tools":[{"x":1}],"weird":"keep"}`), "b")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "b" {
		t.Errorf("model = %v", m["model"])
	}
	for _, k := range []string{"temperature", "tools", "weird"} {
		if _, ok := m[k]; !ok {
			t.Errorf("field %q was dropped by the rewrite", k)
		}
	}
}

// THE PAYOFF FOR THE PHASE WORK. A loading model must return a described 503,
// not a connection refused from a port that is up but not serving.
func TestStartingModelReturns503WithRetryAfter(t *testing.T) {
	res := &fakeRes{
		recs:   []deploy.Record{{RecipeID: "ornith15", HostPort: 1}},
		phases: map[string]deploy.Phase{"ornith15": deploy.PhaseStarting},
	}
	cat := fakeCat{"ornith15": {ID: "ornith15", ServedAs: []string{"ornith"}}}

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"ornith"}`))
	rr := httptest.NewRecorder()
	newGW(res, cat, "127.0.0.1").Proxy(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After; a client will hammer a model that takes minutes to load")
	}
	if !strings.Contains(rr.Body.String(), "loading") {
		t.Errorf("body does not say why: %s", rr.Body.String())
	}
}

func TestFailedAndStoppingAreDistinguished(t *testing.T) {
	for phase, want := range map[deploy.Phase]string{
		deploy.PhaseFailed:   "model_failed",
		deploy.PhaseStopping: "model_stopping",
		deploy.PhaseGone:     "model_unavailable",
	} {
		res := &fakeRes{
			recs:   []deploy.Record{{RecipeID: "x", HostPort: 1}},
			phases: map[string]deploy.Phase{"x": phase},
		}
		cat := fakeCat{"x": {ID: "x", ServedAs: []string{"xx"}}}
		rr := httptest.NewRecorder()
		newGW(res, cat, "127.0.0.1").Proxy(rr,
			httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"xx"}`)))
		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status %d", phase, rr.Code)
		}
		if !strings.Contains(rr.Body.String(), want) {
			t.Errorf("%s: body %s, want type %q", phase, rr.Body.String(), want)
		}
	}
}

// A bare 404 sends the caller to the dashboard to find out what they should
// have asked for. Naming what is deployed answers it in one round trip.
func TestUnknownModelNamesWhatIsAvailable(t *testing.T) {
	res := &fakeRes{recs: []deploy.Record{{RecipeID: "ornith15", HostPort: 1}}}
	cat := fakeCat{"ornith15": {ID: "ornith15", ServedAs: []string{"ornith"}}}
	rr := httptest.NewRecorder()
	newGW(res, cat, "127.0.0.1").Proxy(rr,
		httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4"}`)))

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "ornith") {
		t.Errorf("404 did not name what is available: %s", rr.Body.String())
	}
}

// OpenAI's error envelope, because every SDK parses it. A bare string makes
// them all report "unknown error".
func TestErrorsUseTheOpenAIEnvelope(t *testing.T) {
	res := &fakeRes{}
	rr := httptest.NewRecorder()
	newGW(res, fakeCat{}, "127.0.0.1").Proxy(rr,
		httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"nope"}`)))

	var out struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("not JSON: %s", rr.Body.String())
	}
	if out.Error.Message == "" || out.Error.Type == "" {
		t.Errorf("envelope incomplete: %s", rr.Body.String())
	}
}

// Streaming must arrive as it is produced. A buffered proxy turns token
// streaming into a long pause and then a wall of text.
func TestStreamingIsNotBuffered(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			return
		}
		_, _ = w.Write([]byte("data: first\n\n"))
		fl.Flush()
		<-release // hold the stream open; a buffering proxy would block here
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		fl.Flush()
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	res := &fakeRes{recs: []deploy.Record{{RecipeID: "x", HostPort: port}}}
	cat := fakeCat{"x": {ID: "x", ServedAs: []string{"xx"}}}

	front := httptest.NewServer(http.HandlerFunc(newGW(res, cat, u.Hostname()).Proxy))
	defer front.Close()

	resp, err := http.Post(front.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"xx","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 64)
	n, err := resp.Body.Read(buf)
	close(release)
	if err != nil {
		t.Fatalf("first chunk never arrived: %v", err)
	}
	if !strings.Contains(string(buf[:n]), "first") {
		t.Errorf("first chunk = %q, want it before the stream closed", string(buf[:n]))
	}
}

// MULTIPART. Audio transcription names its model in a form field, not JSON.
// The suite tested only JSON bodies, so this path shipped broken: the body was
// drained into a buffer before FormValue was called, which then silently found
// nothing and told the caller it had named no model when it had named one
// correctly. Found by actually calling the ASR endpoint.
func TestMultipartCarriesTheModelInAFormField(t *testing.T) {
	var seen string
	srv, port := upstream(t, &seen)
	defer srv.Close()

	res := &fakeRes{recs: []deploy.Record{{RecipeID: "asr", HostPort: port}}}
	cat := fakeCat{"asr": {ID: "asr", ServedAs: []string{"asr"}}}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "asr")
	fw, err := mw.CreateFormFile("file", "clip.mp3")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("ID3-not-really-audio")); err != nil {
		t.Fatal(err)
	}
	_ = mw.Close()

	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	newGW(res, cat, "127.0.0.1").Proxy(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	// The upload must arrive byte for byte: a file re-encoded by a round trip
	// through the form parser is not the file the caller sent.
	if !strings.Contains(seen, "ID3-not-really-audio") {
		t.Errorf("the uploaded file did not reach the upstream intact")
	}
	if !strings.Contains(seen, `name="model"`) {
		t.Errorf("the form was rewritten rather than forwarded: %s", seen[:min(200, len(seen))])
	}
}

func TestMultipartWithoutAModelIsStillRefusedClearly(t *testing.T) {
	res := &fakeRes{recs: []deploy.Record{{RecipeID: "asr", HostPort: 1}}}
	cat := fakeCat{"asr": {ID: "asr", ServedAs: []string{"asr"}}}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "clip.mp3")
	_, _ = fw.Write([]byte("audio"))
	_ = mw.Close()

	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	newGW(res, cat, "127.0.0.1").Proxy(rr, req)

	if rr.Code == http.StatusOK {
		t.Fatal("a request naming no model was proxied anyway")
	}
	if !strings.Contains(rr.Body.String(), "model") {
		t.Errorf("the refusal does not mention the model: %s", rr.Body.String())
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// dialFakeEchoingSouslet connects a fake souslet to srv, exactly the way
// grpcserver's own dialFakeSouslet test helper does (bufconn, no real
// network), except its read loop ALSO answers any HTTPRequestHead/Chunk pair
// - the shape Gateway.Proxy's gRPC path sends - with a fixed
// HTTPResponseHead{Status: 200} followed by an
// HTTPResponseChunk{Data: []byte("ok"), Eof: true}. Enough to prove the
// gateway relays a request through gRPC end to end without needing a real
// vLLM container. Written once here, specific to this test's scenario
// (grpcserver's own helper doesn't need this behavior and shouldn't be
// bloated with it - see Task 9's brief).
//
// Blocks until srv genuinely has nodeID registered before returning, so the
// caller can immediately proxy through it without its own retry loop - the
// readiness probe is a real OpenProxyStream/Close round trip against srv,
// not a sleep.
func dialFakeEchoingSouslet(t *testing.T, srv *grpcserver.Server, nodeID string) func() {
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
	client := pb.NewSousletClient(conn)
	stream, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := stream.Send(&pb.Envelope{Payload: &pb.Envelope_Snapshot{Snapshot: &pb.NodeSnapshot{
		NodeId:      nodeID,
		Deployments: []*pb.DeploymentState{{RecipeId: "dflash2", Phase: "ready"}},
	}}}); err != nil {
		t.Fatalf("send snapshot: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			env, err := stream.Recv()
			if err != nil {
				return
			}
			if env.GetHttpReqHead() == nil {
				continue // the request-body chunk that follows the head; nothing to answer
			}
			streamID := env.StreamId
			_ = stream.Send(&pb.Envelope{StreamId: streamID, Payload: &pb.Envelope_HttpRespHead{
				HttpRespHead: &pb.HTTPResponseHead{Status: 200},
			}})
			_ = stream.Send(&pb.Envelope{StreamId: streamID, Payload: &pb.Envelope_HttpRespChunk{
				HttpRespChunk: &pb.HTTPResponseChunk{Data: []byte("ok"), Eof: true},
			}})
		}
	}()

	// Wait for srv's Connect handler to actually register nodeID - stream.Send
	// above only hands the snapshot to the local transport, it does not wait
	// for the server side to finish registering the connection. A real
	// OpenProxyStream probe (immediately closed) is a direct readiness check
	// against the thing the test is about to depend on, not a sleep guess.
	deadline := time.Now().Add(2 * time.Second)
	for {
		ps, err := srv.OpenProxyStream(nodeID)
		if err == nil {
			ps.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("node %q never showed as connected to srv: %v", nodeID, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	return func() {
		_ = stream.CloseSend()
		_ = conn.Close()
		s.Stop()
		<-done
	}
}

// THE CORE OF TASK 9. A request for a model deployed on a connected node
// must be relayed over that node's gRPC connection rather than dialed on a
// local port - this is what makes the gateway work when the model is
// running on a different machine than sous-api.
func TestProxyForwardsToTheNodeCurrentlyRunningTheModel(t *testing.T) {
	nodes := nodecatalog.New()
	nodes.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{
		NodeId:      "asus-gx10",
		Deployments: []*pb.DeploymentState{{RecipeId: "dflash2", Phase: "ready"}},
	})
	gsrv := grpcserver.New(nodes)
	// A fake souslet that answers any proxied request with a fixed 200 and
	// body "ok" - enough to prove the gateway relays through gRPC end to
	// end without needing a real vLLM container.
	stopFakeSouslet := dialFakeEchoingSouslet(t, gsrv, "asus-gx10")
	defer stopFakeSouslet()

	g := &Gateway{Nodes: nodes, GRPC: gsrv}
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"dflash2"}`))
	rec := httptest.NewRecorder()
	g.Proxy(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("body = %q, want ok", rec.Body.String())
	}
}

// Streaming over the gRPC path must arrive AS PRODUCED, not buffered until
// the whole response completes - the same property TestStreamingIsNotBuffered
// already proves for the local-forward path, proven here for the node-routed
// one end to end: gateway -> grpcserver -> fake souslet -> back, with the
// fake souslet deliberately holding its stream open between two chunks so a
// buffering relay would visibly block here.
func TestProxyOverGRPCStreamsWithoutBuffering(t *testing.T) {
	nodes := nodecatalog.New()
	nodes.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{
		NodeId:      "asus-gx10",
		Deployments: []*pb.DeploymentState{{RecipeId: "dflash2", Phase: "ready"}},
	})
	gsrv := grpcserver.New(nodes)

	release := make(chan struct{})
	stop := dialFakeSousletThatHoldsMidStream(t, gsrv, "asus-gx10", release)
	defer stop()

	g := &Gateway{Nodes: nodes, GRPC: gsrv}
	front := httptest.NewServer(http.HandlerFunc(g.Proxy))
	defer front.Close()

	resp, err := http.Post(front.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"dflash2","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 64)
	n, err := resp.Body.Read(buf)
	close(release) // only after the first chunk actually arrived
	if err != nil {
		t.Fatalf("first chunk never arrived: %v", err)
	}
	if !strings.Contains(string(buf[:n]), "first-chunk") {
		t.Errorf("first chunk = %q, want it before the stream closed", string(buf[:n]))
	}

	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the rest of the body: %v", err)
	}
	if !strings.Contains(string(rest), "final") {
		t.Errorf("final chunk missing from the rest of the body: %q", rest)
	}
}

// dialFakeSousletThatHoldsMidStream is TestProxyOverGRPCStreamsWithoutBuffering's
// own helper: like dialFakeEchoingSouslet, but its response has two chunks
// with a deliberate hold (on release) between them, so a test can prove the
// first chunk reaches the original HTTP client before the second is even
// sent - proof the gateway is not buffering the whole response before
// writing anything.
func dialFakeSousletThatHoldsMidStream(t *testing.T, srv *grpcserver.Server, nodeID string, release chan struct{}) func() {
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
	client := pb.NewSousletClient(conn)
	stream, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := stream.Send(&pb.Envelope{Payload: &pb.Envelope_Snapshot{Snapshot: &pb.NodeSnapshot{
		NodeId:      nodeID,
		Deployments: []*pb.DeploymentState{{RecipeId: "dflash2", Phase: "ready"}},
	}}}); err != nil {
		t.Fatalf("send snapshot: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			env, err := stream.Recv()
			if err != nil {
				return
			}
			if env.GetHttpReqHead() == nil {
				continue
			}
			sid := env.StreamId
			_ = stream.Send(&pb.Envelope{StreamId: sid, Payload: &pb.Envelope_HttpRespHead{
				HttpRespHead: &pb.HTTPResponseHead{Status: 200},
			}})
			_ = stream.Send(&pb.Envelope{StreamId: sid, Payload: &pb.Envelope_HttpRespChunk{
				HttpRespChunk: &pb.HTTPResponseChunk{Data: []byte("first-chunk")},
			}})
			<-release // hold the stream open; a buffering relay would block here
			_ = stream.Send(&pb.Envelope{StreamId: sid, Payload: &pb.Envelope_HttpRespChunk{
				HttpRespChunk: &pb.HTTPResponseChunk{Data: []byte("final"), Eof: true},
			}})
		}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		ps, err := srv.OpenProxyStream(nodeID)
		if err == nil {
			ps.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("node %q never showed as connected to srv: %v", nodeID, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	return func() {
		_ = stream.CloseSend()
		_ = conn.Close()
		s.Stop()
		<-done
	}
}

// A model name that no connected node reports must fail cleanly - the
// gateway's whole design ethos ("a 503 naming the phase is more useful than
// a hang") applies just as much to the gRPC path as the local one.
func TestProxyOverGRPCReturns404ForAModelNoNodeIsRunning(t *testing.T) {
	nodes := nodecatalog.New()
	gsrv := grpcserver.New(nodes)
	g := &Gateway{Nodes: nodes, GRPC: gsrv}

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"nope"}`))
	rec := httptest.NewRecorder()
	g.Proxy(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

// BOUNDED FAILURE. Opening a proxy stream to a node with no live connection
// must fail immediately rather than hang the caller - the same "fail fast,
// don't buffer" guarantee Server.Send already gives command dispatch.
func TestProxyOverGRPCFailsFastWhenTheNodeIsNotConnected(t *testing.T) {
	nodes := nodecatalog.New()
	nodes.ReplaceSnapshot("ghost-node", &pb.NodeSnapshot{
		NodeId:      "ghost-node",
		Deployments: []*pb.DeploymentState{{RecipeId: "dflash2", Phase: "ready"}},
	})
	// Deliberately never dial a fake souslet: NodeFor will say the recipe is
	// on "ghost-node" (the catalog was seeded directly), but grpcserver has
	// no live connection for it - the exact case a node that crashed after
	// its last snapshot but before the catalog noticed would produce.
	gsrv := grpcserver.New(nodes)
	g := &Gateway{Nodes: nodes, GRPC: gsrv}

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"dflash2"}`))
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		g.Proxy(rec, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Proxy did not return within 2s against a node with no live gRPC connection - this is the hang the design must avoid")
	}
	if rec.Code != http.StatusServiceUnavailable && rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 503 or 502: %s", rec.Code, rec.Body.String())
	}
}

// THE FIX FOR FINDING 1 (review round). A request body larger than one
// proxyChunkSize must actually travel as MULTIPLE HTTPRequestChunk messages,
// not one big one - grpc-go's default 4MB max receive message size means a
// single-message body in the 4-32MB range (this gateway's own
// maxRequestBytes says audio uploads are the expected large case, not an
// edge case) would fail with ResourceExhausted inside souslet's receive
// loop, which - per Run's reconnect-on-any-stream-error design - drops the
// WHOLE node's connection, not just that one request. This test proves both
// that the body round-trips byte for byte AND that it was genuinely split
// into more than one chunk on the wire (via the fake souslet's own chunk
// count, echoed back in a response header) - a body that merely "still
// works" would not by itself prove chunking is actually happening.
func TestProxyOverGRPCChunksRequestBodiesLargerThanOneChunk(t *testing.T) {
	nodes := nodecatalog.New()
	nodes.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{
		NodeId:      "asus-gx10",
		Deployments: []*pb.DeploymentState{{RecipeId: "dflash2", Phase: "ready"}},
	})
	gsrv := grpcserver.New(nodes)
	stop := dialFakeSousletThatEchoesTheRequestBody(t, gsrv, "asus-gx10")
	defer stop()

	g := &Gateway{Nodes: nodes, GRPC: gsrv}
	// proxyChunkSize is 4096; comfortably more than one chunk's worth so a
	// regression back to "one SendChunk call for the whole body" cannot
	// accidentally still pass by coincidence.
	padding := strings.Repeat("x", proxyChunkSize*3+100)
	body := `{"model":"dflash2","padding":"` + padding + `"}`

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	g.Proxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != body {
		t.Fatalf("body did not round-trip intact: got %d bytes, want %d bytes", rec.Body.Len(), len(body))
	}
	chunks, err := strconv.Atoi(rec.Header().Get("X-Chunk-Count"))
	if err != nil {
		t.Fatalf("X-Chunk-Count header missing or not a number: %q", rec.Header().Get("X-Chunk-Count"))
	}
	if chunks < 2 {
		t.Fatalf("chunk count = %d, want at least 2 - the body was sent as a single message, not actually chunked", chunks)
	}
}

// dialFakeSousletThatEchoesTheRequestBody accumulates every HTTPRequestChunk
// for a stream (across however many arrive before Eof) and echoes the
// reassembled body back verbatim as the response body, with the number of
// chunks it took to arrive reported in an X-Chunk-Count response header -
// direct, wire-level proof of how many HTTPRequestChunk messages the
// gateway actually sent, independent of whether the reassembled bytes
// happen to be correct.
func dialFakeSousletThatEchoesTheRequestBody(t *testing.T, srv *grpcserver.Server, nodeID string) func() {
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
	client := pb.NewSousletClient(conn)
	stream, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := stream.Send(&pb.Envelope{Payload: &pb.Envelope_Snapshot{Snapshot: &pb.NodeSnapshot{
		NodeId:      nodeID,
		Deployments: []*pb.DeploymentState{{RecipeId: "dflash2", Phase: "ready"}},
	}}}); err != nil {
		t.Fatalf("send snapshot: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		var streamID string
		var body []byte
		var count int
		for {
			env, err := stream.Recv()
			if err != nil {
				return
			}
			if env.GetHttpReqHead() != nil {
				streamID = env.StreamId
				body = nil
				count = 0
				continue
			}
			chunk := env.GetHttpReqChunk()
			if chunk == nil || env.StreamId != streamID {
				continue
			}
			body = append(body, chunk.Data...)
			count++
			if !chunk.Eof {
				continue
			}
			_ = stream.Send(&pb.Envelope{StreamId: streamID, Payload: &pb.Envelope_HttpRespHead{
				HttpRespHead: &pb.HTTPResponseHead{Status: 200, Headers: map[string]string{
					"X-Chunk-Count": strconv.Itoa(count),
				}},
			}})
			_ = stream.Send(&pb.Envelope{StreamId: streamID, Payload: &pb.Envelope_HttpRespChunk{
				HttpRespChunk: &pb.HTTPResponseChunk{Data: body, Eof: true},
			}})
		}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		ps, err := srv.OpenProxyStream(nodeID)
		if err == nil {
			ps.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("node %q never showed as connected to srv: %v", nodeID, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	return func() {
		_ = stream.CloseSend()
		_ = conn.Close()
		s.Stop()
		<-done
	}
}

// THE FIX FOR FINDING 2 (review round). When the original HTTP client
// disconnects mid-stream, the gateway must stop relaying promptly instead
// of silently draining the rest of the response into a discarded write
// error - bounding the GATEWAY side's resource usage even though it cannot
// (yet, see proxyOverGRPC's doc comment) stop souslet's own generation.
// Proven here by a fake souslet that streams chunks indefinitely (an
// unbounded generation, the realistic LLM-streaming shape) and a client
// that cancels its own request context after the first chunk - the gateway
// goroutine driving Proxy must return promptly rather than keep looping
// forever alongside a souslet that never stops on its own.
func TestProxyOverGRPCStopsRelayingWhenTheClientDisconnects(t *testing.T) {
	nodes := nodecatalog.New()
	nodes.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{
		NodeId:      "asus-gx10",
		Deployments: []*pb.DeploymentState{{RecipeId: "dflash2", Phase: "ready"}},
	})
	gsrv := grpcserver.New(nodes)
	firstChunkSent := make(chan struct{}, 1)
	stop := dialFakeSousletThatStreamsForever(t, gsrv, "asus-gx10", firstChunkSent)
	defer stop()

	g := &Gateway{Nodes: nodes, GRPC: gsrv}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"dflash2"}`)).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		g.Proxy(rec, req)
		close(done)
	}()

	select {
	case <-firstChunkSent:
	case <-time.After(2 * time.Second):
		t.Fatal("fake souslet never got a chance to stream a first chunk")
	}
	cancel() // simulate the client going away mid-stream

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Proxy kept relaying after the client's context was cancelled - it must stop promptly, not drain forever alongside a souslet that never stops on its own")
	}
}

// dialFakeSousletThatStreamsForever sends a response head, then chunks in
// a tight loop with no Eof, ever - standing in for a real, unbounded LLM
// token stream. Signals firstChunkSent once the first one is on the wire,
// so a test can cancel the client side only after streaming has genuinely
// started (not racing against the request not having reached the fake
// souslet yet).
func dialFakeSousletThatStreamsForever(t *testing.T, srv *grpcserver.Server, nodeID string, firstChunkSent chan struct{}) func() {
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
	client := pb.NewSousletClient(conn)
	stream, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := stream.Send(&pb.Envelope{Payload: &pb.Envelope_Snapshot{Snapshot: &pb.NodeSnapshot{
		NodeId:      nodeID,
		Deployments: []*pb.DeploymentState{{RecipeId: "dflash2", Phase: "ready"}},
	}}}); err != nil {
		t.Fatalf("send snapshot: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			env, err := stream.Recv()
			if err != nil {
				return
			}
			if env.GetHttpReqHead() == nil {
				continue
			}
			sid := env.StreamId
			_ = stream.Send(&pb.Envelope{StreamId: sid, Payload: &pb.Envelope_HttpRespHead{
				HttpRespHead: &pb.HTTPResponseHead{Status: 200},
			}})
			for i := 0; ; i++ {
				if err := stream.Send(&pb.Envelope{StreamId: sid, Payload: &pb.Envelope_HttpRespChunk{
					HttpRespChunk: &pb.HTTPResponseChunk{Data: []byte("tok")},
				}}); err != nil {
					return // the client side (this test's grpc.ClientConn) went away
				}
				if i == 0 {
					select {
					case firstChunkSent <- struct{}{}:
					default:
					}
				}
				time.Sleep(2 * time.Millisecond) // paced, so this loop doesn't just spin CPU forever in the background
			}
		}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		ps, err := srv.OpenProxyStream(nodeID)
		if err == nil {
			ps.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("node %q never showed as connected to srv: %v", nodeID, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	return func() {
		_ = stream.CloseSend()
		_ = conn.Close()
		s.Stop()
		<-done
	}
}

// scopedCtx puts a scoped key on the request, as the auth middleware does.
func scopedCtx(r *http.Request, models ...string) *http.Request {
	return r.WithContext(authWithKey(r.Context(), models))
}

// A scoped key must be refused for a model it does not carry - with 403, not
// 404: the model is real, and the caller should learn their credential is the
// problem rather than go hunting for a typo.
func TestScopedKeyIsForbiddenForAModelItLacks(t *testing.T) {
	var seen string
	srv, port := upstream(t, &seen)
	defer srv.Close()

	res := &fakeRes{recs: []deploy.Record{{RecipeID: "qwen36", HostPort: port}}}
	cat := fakeCat{"qwen36": {ID: "qwen36", ServedAs: []string{"qwen"}}}

	req := scopedCtx(httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"qwen"}`)), "asr", "kokoro")
	rr := httptest.NewRecorder()
	newGW(res, cat, "127.0.0.1").Proxy(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "model_not_permitted") {
		t.Errorf("body = %s", rr.Body.String())
	}
	if seen != "" {
		t.Error("the request reached the upstream despite being out of scope")
	}
}

func TestScopedKeyReachesItsOwnModel(t *testing.T) {
	var seen string
	srv, port := upstream(t, &seen)
	defer srv.Close()

	res := &fakeRes{recs: []deploy.Record{{RecipeID: "asr", HostPort: port}}}
	cat := fakeCat{"asr": {ID: "asr", ServedAs: []string{"asr"}}}

	req := scopedCtx(httptest.NewRequest("POST", "/v1/audio/transcriptions",
		strings.NewReader(`{"model":"asr"}`)), "asr")
	rr := httptest.NewRecorder()
	newGW(res, cat, "127.0.0.1").Proxy(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
}

// A key scoped to an alias must keep working when the same model is addressed
// by its recipe id, or the scope becomes a spelling test.
func TestScopeMatchesAliasesAndRecipeIDAlike(t *testing.T) {
	srv, port := upstream(t, nil)
	defer srv.Close()

	res := &fakeRes{recs: []deploy.Record{{RecipeID: "ornith15", HostPort: port}}}
	cat := fakeCat{"ornith15": {ID: "ornith15", ServedAs: []string{"ornith"}}}

	for _, asked := range []string{"ornith", "ornith15"} {
		req := scopedCtx(httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"`+asked+`"}`)), "ornith")
		rr := httptest.NewRecorder()
		newGW(res, cat, "127.0.0.1").Proxy(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("asking for %q with a key scoped to the alias = %d", asked, rr.Code)
		}
	}
}

// An unscoped key - and the admin token, which puts nothing on the context -
// must reach everything, or the upgrade revokes every existing credential.
func TestUnscopedCallerReachesEverything(t *testing.T) {
	srv, port := upstream(t, nil)
	defer srv.Close()
	res := &fakeRes{recs: []deploy.Record{{RecipeID: "x", HostPort: port}}}
	cat := fakeCat{"x": {ID: "x", ServedAs: []string{"xx"}}}

	// No key on the context at all: the admin token path.
	rr := httptest.NewRecorder()
	newGW(res, cat, "127.0.0.1").Proxy(rr,
		httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"xx"}`)))
	if rr.Code != http.StatusOK {
		t.Errorf("admin caller = %d, want 200", rr.Code)
	}

	// A key with an empty allowlist.
	rr2 := httptest.NewRecorder()
	newGW(res, cat, "127.0.0.1").Proxy(rr2,
		scopedCtx(httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"xx"}`))))
	if rr2.Code != http.StatusOK {
		t.Errorf("unscoped key = %d, want 200", rr2.Code)
	}
}

// Listing models a key will be refused for advertises something useless.
func TestListModelsHidesWhatTheKeyCannotUse(t *testing.T) {
	res := &fakeRes{recs: []deploy.Record{
		{RecipeID: "asr", HostPort: 1}, {RecipeID: "qwen36", HostPort: 2},
	}}
	cat := fakeCat{
		"asr":    {ID: "asr", ServedAs: []string{"asr"}},
		"qwen36": {ID: "qwen36", ServedAs: []string{"qwen"}},
	}
	req := scopedCtx(httptest.NewRequest("GET", "/v1/models", nil), "asr")
	rr := httptest.NewRecorder()
	newGW(res, cat, "127.0.0.1").ListModels(rr, req)

	body := rr.Body.String()
	if !strings.Contains(body, `"asr"`) {
		t.Errorf("the key's own model is missing: %s", body)
	}
	if strings.Contains(body, `"qwen"`) {
		t.Errorf("a model the key cannot use was advertised: %s", body)
	}
}

// fakeAliases is the operator alias store.
type fakeAliases map[string][]string

func (f fakeAliases) Of(recipeID string) []string { return f[recipeID] }

// AN ALIAS IS A MODEL AS FAR AS A CLIENT IS CONCERNED. It has to appear in
// /v1/models or nothing that discovers models will ever ask for it.
func TestOperatorAliasesAppearInListModels(t *testing.T) {
	res := &fakeRes{recs: []deploy.Record{{RecipeID: "qwen38-dflash2", HostPort: 8000}}}
	cat := fakeCat{"qwen38-dflash2": {ID: "qwen38-dflash2", ServedAs: []string{"dflash2"},
		Modality: recipe.ModalityText}}
	gw := newGW(res, cat, "127.0.0.1")
	gw.Alias = fakeAliases{"qwen38-dflash2": {"fast", "default"}}

	rr := httptest.NewRecorder()
	gw.ListModels(rr, httptest.NewRequest("GET", "/v1/models", nil))

	var got struct {
		Data []struct {
			ID       string `json:"id"`
			RecipeID string `json:"recipe_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, d := range got.Data {
		names[d.ID] = d.RecipeID
	}
	// The recipe's own served_as stays listed; the aliases join it.
	for _, want := range []string{"dflash2", "fast", "default"} {
		if names[want] != "qwen38-dflash2" {
			t.Errorf("/v1/models has no %q pointing at qwen38-dflash2; got %v", want, names)
		}
	}
}

// AND CALLING ONE HAS TO ROUTE. The alias reaches the model, and the body is
// rewritten to the name the engine actually answers to - an alias is a name for
// callers, never one the engine has to know about.
func TestCallingAnAliasRoutesAndRewritesToUpstream(t *testing.T) {
	var seen string
	srv, port := upstream(t, &seen)
	defer srv.Close()

	res := &fakeRes{recs: []deploy.Record{{RecipeID: "qwen38-dflash2", HostPort: port}}}
	cat := fakeCat{"qwen38-dflash2": {ID: "qwen38-dflash2", ServedAs: []string{"dflash2"}}}
	gw := newGW(res, cat, "127.0.0.1")
	gw.Alias = fakeAliases{"qwen38-dflash2": {"fast"}}

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"fast","messages":[]}`))
	rr := httptest.NewRecorder()
	gw.Proxy(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(seen), &got); err != nil {
		t.Fatalf("upstream body not JSON: %s", seen)
	}
	if got["model"] != "dflash2" {
		t.Errorf("upstream asked for %v, want dflash2 - the alias was forwarded verbatim", got["model"])
	}
}

// A gateway with no alias store is the normal case and must not panic.
func TestNoAliasStoreIsFine(t *testing.T) {
	res := &fakeRes{recs: []deploy.Record{{RecipeID: "ornith15", HostPort: 8000}}}
	cat := fakeCat{"ornith15": {ID: "ornith15", ServedAs: []string{"ornith"}}}
	gw := newGW(res, cat, "127.0.0.1")
	gw.Alias = nil
	rr := httptest.NewRecorder()
	gw.ListModels(rr, httptest.NewRequest("GET", "/v1/models", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
}

// fakeReqLog captures what the gateway would have logged.
type fakeReqLog struct {
	sender, remoteAddr, model string
	body                      []byte
	calls                     int
}

func (f *fakeReqLog) Log(sender, remoteAddr, model string, body []byte) {
	f.sender, f.remoteAddr, f.model = sender, remoteAddr, model
	f.body = body
	f.calls++
}

// THE AUDIT LOG SEES WHAT WAS ACTUALLY SENT, verbatim, and who sent it.
func TestChatCompletionsAreLoggedWithSenderAndBody(t *testing.T) {
	srv, port := upstream(t, nil)
	defer srv.Close()
	res := &fakeRes{recs: []deploy.Record{{RecipeID: "qwen38", HostPort: port}}}
	cat := fakeCat{"qwen38": {ID: "qwen38"}}
	gw := newGW(res, cat, "127.0.0.1")
	rl := &fakeReqLog{}
	gw.ReqLog = rl

	body := `{"model":"qwen38","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.RemoteAddr = "10.0.0.7:5555"
	req = req.WithContext(auth.WithKeyForTest(req.Context(), auth.KeyInfo{Name: "voice demo"}))
	gw.Proxy(httptest.NewRecorder(), req)

	if rl.calls != 1 {
		t.Fatalf("Log called %d times, want 1", rl.calls)
	}
	if rl.sender != "voice demo" {
		t.Errorf("sender = %q", rl.sender)
	}
	if rl.remoteAddr != "10.0.0.7:5555" {
		t.Errorf("remoteAddr = %q", rl.remoteAddr)
	}
	if string(rl.body) != body {
		t.Errorf("body = %s, want %s", rl.body, body)
	}
}

// No key in context means the operator credential authenticated the request,
// not that nobody did - the log has to say something attributable, not blank.
func TestUnscopedCallerLogsAsOperator(t *testing.T) {
	srv, port := upstream(t, nil)
	defer srv.Close()
	res := &fakeRes{recs: []deploy.Record{{RecipeID: "qwen38", HostPort: port}}}
	cat := fakeCat{"qwen38": {ID: "qwen38"}}
	gw := newGW(res, cat, "127.0.0.1")
	rl := &fakeReqLog{}
	gw.ReqLog = rl

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"qwen38","messages":[]}`))
	gw.Proxy(httptest.NewRecorder(), req)
	if rl.sender != "operator" {
		t.Errorf("sender = %q, want operator", rl.sender)
	}
}

// SCOPED TO CHAT COMPLETIONS ONLY. This gateway proxies embeddings, audio and
// more through the SAME Proxy function; the audit log asked for is
// specifically for chat completions and must not silently capture the rest.
func TestOtherProxiedPathsAreNotLogged(t *testing.T) {
	srv, port := upstream(t, nil)
	defer srv.Close()
	res := &fakeRes{recs: []deploy.Record{{RecipeID: "asr", HostPort: port}}}
	cat := fakeCat{"asr": {ID: "asr"}}
	gw := newGW(res, cat, "127.0.0.1")
	rl := &fakeReqLog{}
	gw.ReqLog = rl

	req := httptest.NewRequest("POST", "/v1/audio/transcriptions",
		strings.NewReader(`{"model":"asr"}`))
	gw.Proxy(httptest.NewRecorder(), req)
	if rl.calls != 0 {
		t.Errorf("Log called %d times for a non-chat-completions path", rl.calls)
	}
}

// A gateway with no logger configured is the normal case and must not panic.
func TestNoReqLogIsFine(t *testing.T) {
	srv, port := upstream(t, nil)
	defer srv.Close()
	res := &fakeRes{recs: []deploy.Record{{RecipeID: "qwen38", HostPort: port}}}
	cat := fakeCat{"qwen38": {ID: "qwen38"}}
	gw := newGW(res, cat, "127.0.0.1")
	gw.ReqLog = nil

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"qwen38","messages":[]}`))
	rr := httptest.NewRecorder()
	gw.Proxy(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
}

// LOGGED EVEN WHEN THE REQUEST IS REFUSED - a scoped key hitting a model it
// cannot reach still asked, and the audit trail is about what was asked, not
// only what succeeded.
func TestRefusedRequestsAreStillLogged(t *testing.T) {
	res := &fakeRes{recs: []deploy.Record{{RecipeID: "qwen38", HostPort: 9}}}
	cat := fakeCat{"qwen38": {ID: "qwen38"}}
	gw := newGW(res, cat, "127.0.0.1")
	rl := &fakeReqLog{}
	gw.ReqLog = rl

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"does-not-exist","messages":[]}`))
	rr := httptest.NewRecorder()
	gw.Proxy(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	if rl.calls != 1 {
		t.Errorf("Log called %d times for a refused request, want 1", rl.calls)
	}
}
