// Package gateway fronts every deployed model behind one OpenAI-compatible API.
//
// THE PROBLEM IT SOLVES. Sous allocates a port per model, so every client has
// to know which model lives on which port, and that mapping changes whenever
// anything is redeployed. The voice demo hardcodes three ports; a notebook
// pointed at :8000 silently talks to whatever happens to be there now. Worse,
// a port answers connection-refused for the eight to ten minutes a model spends
// loading, so "is it up" is a question every client ends up implementing badly.
//
// One endpoint, model chosen by name, removes all of that: clients name a
// model the way they would name one at any OpenAI-compatible provider, and the
// gateway resolves it to whatever is deployed right now.
//
// WHAT IT DELIBERATELY DOES NOT DO. It does not queue, retry, load-balance, or
// fall back to another model. Envoy AI Gateway does those things because it
// fronts many replicas of many providers; this fronts one node where each model
// is a singleton and a request that cannot be served should say so immediately
// rather than being held. A 503 naming the phase is more useful to a caller
// than a hang, and far more useful than a silent redirect to a different model
// than the one asked for.
//
// NOT A SURFACE GAP. Envoy covers this API surface completely, text-to-speech
// included since their v0.6.0. What it cannot know is which models are deployed
// on this node right now and whether they have finished loading - that lives in
// Sous's deployment records, which is why the routing table and the readiness
// signal here are the same data structure.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/codemug/sous/internal/auth"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codemug/sous/internal/deploy"
	"github.com/codemug/sous/internal/engine"
	"github.com/codemug/sous/internal/grpcserver"
	"github.com/codemug/sous/internal/nodecatalog"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
	"github.com/codemug/sous/internal/recipe"
)

// Resolver is the slice of Sous the gateway needs: what is deployed, and is it
// actually serving. Narrow on purpose - the gateway has no business deploying
// anything, and a type that cannot deploy cannot be talked into it.
type Resolver interface {
	List() ([]deploy.Record, error)
	Phase(ctx context.Context, rec deploy.Record, st engine.ContainerState, known bool) deploy.Phase
	States(ctx context.Context) (map[string]engine.ContainerState, error)
}

// Catalog supplies the recipe behind a deployment, for its aliases.
type Catalog interface {
	Get(id string) (recipe.Recipe, error)
}

// Aliases supplies the extra names a deployed model answers to. Optional; nil
// means a model is reachable only by its recipe id and its served_as.
type Aliases interface {
	Of(recipeID string) []string
}

// RequestLog records every request reaching /v1/chat/completions - who sent
// it and exactly what they sent - for later audit. Optional; nil means
// nothing is logged.
//
// SENDER AND BODY ONLY, no destination or outcome. Which container answered
// and whether it succeeded are decisions this package already makes and logs
// nowhere else either; the audit trail this exists for is "who asked for
// what", not a general request log.
type RequestLog interface {
	Log(sender, remoteAddr, model string, body []byte)
}

type Gateway struct {
	Res    Resolver
	Cat    Catalog
	Alias  Aliases
	ReqLog RequestLog
	Host   string

	// Nodes and GRPC are the multi-node routing path, additive alongside
	// Res/Cat/Alias/Host's existing local-forward path - the same
	// migration posture Tasks 7/8 already established for httpapi.Server's
	// gsrv/nodes fields: both nil is the normal single-node case (every
	// pre-existing test in this file constructs a Gateway without them,
	// and must keep working exactly as before), both set is sous-api's
	// multi-node case. Proxy branches on their presence BEFORE touching
	// Res/Cat at all, so this path works even when Res/Cat are nil (no
	// local deploy.Manager exists in a pure multi-node deployment) - see
	// Proxy's doc comment for exactly what this path does and does not
	// carry over from the local-forward path (aliasing, phase-gating).
	Nodes *nodecatalog.Catalog
	GRPC  *grpcserver.Server

	// Now is injectable so tests do not sleep.
	Now func() time.Time
}

// route is one resolved destination.
type route struct {
	RecipeID string
	Port     int
	Phase    deploy.Phase
	// Upstream is the name the model actually answers to, which is not always
	// the name the caller used.
	Upstream string
	Modality string
	Aliases  []string
}

// Routes lists every deployment with the names it can be reached by.
func (g *Gateway) Routes(ctx context.Context) ([]route, error) {
	ds, err := g.Res.List()
	if err != nil {
		return nil, err
	}
	states := map[string]engine.ContainerState{}
	known := false
	if st, err := g.Res.States(ctx); err == nil {
		states, known = st, true
	}

	out := make([]route, 0, len(ds))
	for _, d := range ds {
		r := route{RecipeID: d.RecipeID, Port: d.HostPort}
		r.Phase = g.Res.Phase(ctx, d, states[engine.ContainerName(d.RecipeID)], known)
		if rec, err := g.Cat.Get(d.RecipeID); err == nil {
			r.Aliases = rec.ServedAs
			r.Modality = string(rec.Modality)
			if len(rec.ServedAs) > 0 {
				r.Upstream = rec.ServedAs[0]
			} else {
				r.Upstream = rec.Model
			}
		}
		// Operator aliases come AFTER the recipe's own names, and Upstream is
		// left alone. Upstream is what the model actually answers to, so a
		// request arriving under an alias is rewritten to the served_as before
		// it is forwarded - an alias is a name for callers, never a name the
		// engine has to know about.
		if g.Alias != nil {
			r.Aliases = append(r.Aliases, g.Alias.Of(d.RecipeID)...)
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RecipeID < out[j].RecipeID })
	return out, nil
}

// resolve maps a requested model name to a destination.
//
// Aliases are matched before recipe ids, because an alias is what a recipe
// chose to be called and an id is an implementation detail. Both work, so a
// caller who knows only the dashboard can still address a model.
func (g *Gateway) resolve(ctx context.Context, name string) (route, error) {
	rs, err := g.Routes(ctx)
	if err != nil {
		return route{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return route{}, fmt.Errorf("no model named in the request")
	}
	var byID *route
	for i := range rs {
		for _, a := range rs[i].Aliases {
			if strings.EqualFold(a, name) {
				return rs[i], nil
			}
		}
		if strings.EqualFold(rs[i].RecipeID, name) {
			byID = &rs[i]
		}
	}
	if byID != nil {
		return *byID, nil
	}
	return route{}, notFound(name, rs)
}

type errNoModel struct {
	name      string
	available []string
}

func (e errNoModel) Error() string {
	if len(e.available) == 0 {
		return fmt.Sprintf("no model named %q, and nothing is deployed", e.name)
	}
	return fmt.Sprintf("no model named %q. Deployed: %s", e.name, strings.Join(e.available, ", "))
}

// notFound names what IS available. A bare 404 makes the caller go and read the
// dashboard to find out what they should have asked for.
func notFound(name string, rs []route) error {
	var avail []string
	for _, r := range rs {
		if len(r.Aliases) > 0 {
			avail = append(avail, r.Aliases...)
		} else {
			avail = append(avail, r.RecipeID)
		}
	}
	sort.Strings(avail)
	return errNoModel{name: name, available: avail}
}

// ---- OpenAI surface ------------------------------------------------------

type modelObj struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`

	// Beyond the OpenAI shape, and deliberately: a client choosing a model
	// wants to know whether it can be used right now, and the phase is the
	// honest answer. Extra fields are ignored by every OpenAI client.
	Phase    string `json:"phase"`
	RecipeID string `json:"recipe_id"`
	Modality string `json:"modality,omitempty"`
	Port     int    `json:"port,omitempty"`
}

// ListModels answers GET /v1/models.
//
// EVERY deployment is listed, not only the ready ones, with its phase attached.
// Hiding a starting model would make a client that polls /v1/models conclude it
// does not exist, which is a worse answer than "it exists and is not ready".
func (g *Gateway) ListModels(w http.ResponseWriter, r *http.Request) {
	rs, err := g.Routes(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	// A scoped key sees only its own models. Listing the rest would advertise
	// models every request for them is going to be refused for.
	var allow []string
	if k, ok := auth.FromContext(r.Context()); ok {
		allow = k.Models
	}

	now := g.now().Unix()
	data := make([]modelObj, 0, len(rs))
	for _, rt := range rs {
		if len(allow) > 0 && !allowedBy(allow, rt.RecipeID, rt) {
			continue
		}
		names := rt.Aliases
		if len(names) == 0 {
			names = []string{rt.RecipeID}
		}
		for _, n := range names {
			data = append(data, modelObj{
				ID: n, Object: "model", Created: now, OwnedBy: "sous",
				Phase: string(rt.Phase), RecipeID: rt.RecipeID,
				Modality: rt.Modality, Port: rt.Port,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (g *Gateway) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// Proxy handles every inference path by resolving the model and forwarding.
func (g *Gateway) Proxy(w http.ResponseWriter, r *http.Request) {
	// The body has to be read to learn which model is wanted, so it is buffered
	// and replayed. Only the REQUEST is buffered - responses stream untouched,
	// which is what SSE needs.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "could not read request body: "+err.Error())
		return
	}

	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &probe)

	name := probe.Model
	if name == "" {
		// MULTIPART. Audio transcription sends the model as a form field, not
		// JSON. The body has already been drained into the buffer above, so the
		// request has to be handed a fresh reader before anything can parse it -
		// calling FormValue on the drained original silently finds nothing, and
		// the caller gets told it named no model when it named one correctly.
		name = multipartModel(r, body)
	}

	// LOGGED ON RECEIPT, before resolve/scope/phase decide anything - a
	// request that names a model that does not exist, or one a scoped key
	// cannot reach, was still received and is still the thing an audit trail
	// needs to answer "who asked for what". Scoped to exactly one path: this
	// is an audit log for chat completions, not a general request log for
	// every OpenAI-shaped route this gateway proxies.
	if g.ReqLog != nil && r.URL.Path == "/v1/chat/completions" {
		sender := "operator"
		if k, ok := auth.FromContext(r.Context()); ok {
			sender = k.Name
		}
		g.ReqLog.Log(sender, r.RemoteAddr, name, body)
	}

	// MULTI-NODE PATH. Checked before touching Res/Cat at all, so it works
	// even when this Gateway carries no local deploy.Manager (sous-api's
	// eventual end state per the design doc's migration plan - see the
	// Nodes/GRPC field doc). Deliberately does not reuse resolve()'s
	// alias/phase machinery: nodecatalog only knows a recipe ID and
	// Docker's own raw phase string, not this package's richer
	// deploy.Phase vocabulary or the operator alias store, so a proxied
	// request's declared model IS the recipe ID directly here, with no
	// served-model rewrite - the same simplification
	// grpcclient.forwardToLocalContainer's own doc comment already notes
	// on the souslet side of this same request.
	if g.Nodes != nil && g.GRPC != nil {
		g.proxyOverGRPC(w, r, name, body)
		return
	}

	rt, err := g.resolve(r.Context(), name)
	if err != nil {
		var nm errNoModel
		if ok := asNoModel(err, &nm); ok {
			writeErr(w, http.StatusNotFound, "model_not_found", nm.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}

	// SCOPE IS THE FIRST GATE. A key limited to particular models must be
	// refused here even for a model that exists and is ready - 403, not 404,
	// because the caller should learn that the model is real and that their
	// credential is the problem, not go hunting for a typo.
	if k, ok := auth.FromContext(r.Context()); ok && len(k.Models) > 0 {
		if !allowedBy(k.Models, name, rt) {
			writeErr(w, http.StatusForbidden, "model_not_permitted",
				fmt.Sprintf("this key may not use %q", name))
			return
		}
	}

	// PHASE IS THE GATE. This is the whole reason the phase work came first: a
	// caller gets a description of why it cannot be served rather than a
	// connection refused from a port that is up but still loading.
	if rt.Phase != deploy.PhaseReady {
		switch rt.Phase {
		case deploy.PhaseStarting:
			// Retry-After is a real number, not a guess: models on this node
			// take minutes to load and a client retrying every second is just
			// noise.
			w.Header().Set("Retry-After", "30")
			writeErr(w, http.StatusServiceUnavailable, "model_starting",
				fmt.Sprintf("%q is still loading and is not accepting requests yet", name))
		case deploy.PhaseStopping:
			writeErr(w, http.StatusServiceUnavailable, "model_stopping",
				fmt.Sprintf("%q is being undeployed", name))
		case deploy.PhaseFailed:
			writeErr(w, http.StatusServiceUnavailable, "model_failed",
				fmt.Sprintf("%q failed to start; check its logs", name))
		default:
			writeErr(w, http.StatusServiceUnavailable, "model_unavailable",
				fmt.Sprintf("%q is deployed but has no running container", name))
		}
		return
	}

	// REWRITE THE MODEL NAME. vLLM answers to what it was started with -
	// --served-model-name, which is the recipe's first alias. A caller who used
	// the recipe id would otherwise get a 404 from the upstream for a model the
	// gateway just told them exists.
	if probe.Model != "" && rt.Upstream != "" && !strings.EqualFold(probe.Model, rt.Upstream) {
		if rewritten, err := rewriteModel(body, rt.Upstream); err == nil {
			body = rewritten
		}
	}

	target := &url.URL{Scheme: "http", Host: net.JoinHostPort(g.host(), strconv.Itoa(rt.Port))}
	prox := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
			// The upstream is a local container that has no idea what a
			// gateway is; forwarding hop headers only confuses its logs.
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Cookie")
		},
		// -1 flushes every write immediately. Without it Go buffers, and an SSE
		// stream arrives in one lump at the end - which turns token streaming
		// into a long pause followed by a wall of text.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeErr(w, http.StatusBadGateway, "upstream_error",
				fmt.Sprintf("%s on :%d did not answer: %v", rt.RecipeID, rt.Port, err))
		},
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Del("Content-Length")
	prox.ServeHTTP(w, r)
}

// proxyOverGRPC is Proxy's multi-node forward: instead of dialing a local
// container port, it opens a ProxyStream to whichever connected node
// currently reports name as a deployed recipe (nodecatalog.NodeFor) and
// relays the request over that node's gRPC connection, copying the
// response back onto w AS IT ARRIVES - flushing after every chunk, the same
// way the local ReverseProxy path's FlushInterval: -1 does - so SSE/
// streaming inference responses keep working end to end regardless of which
// machine actually runs the model.
//
// KNOWN, DISCLOSED SIMPLIFICATION: unlike the local-forward path below, this
// does not phase-gate (nodecatalog's DeploymentState.Phase is Docker's raw
// status string, not this package's richer starting/ready/failed
// vocabulary - there is no equivalent-quality "still loading" signal to act
// on here yet) and does not consult Alias/Cat for served-model rewriting -
// name is forwarded exactly as the caller sent it, and must match a recipe
// ID directly. Scope enforcement (auth.FromContext) is intentionally still
// skipped too, matching what the local path does for an unscoped caller;
// wiring a scope check in here as well is straightforward future work, not
// done in this task because nothing in this task's brief or tests exercises
// it and the file list does not include the auth-scoping change that would
// need review alongside it.
func (g *Gateway) proxyOverGRPC(w http.ResponseWriter, r *http.Request, name string, body []byte) {
	name = strings.TrimSpace(name)
	if name == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "no model named in the request")
		return
	}

	nodeID, ok := g.Nodes.NodeFor(name)
	if !ok {
		writeErr(w, http.StatusNotFound, "model_not_found",
			fmt.Sprintf("no connected node is running %q", name))
		return
	}

	// OpenProxyStream fails immediately if nodeID has no live gRPC
	// connection - the exact "fail fast, don't buffer" guarantee Send
	// already gives command dispatch, now extended to proxied HTTP: a
	// node that crashed after its last snapshot (so the catalog still
	// lists it) but before grpcserver noticed cannot silently hang this
	// request.
	stream, err := g.GRPC.OpenProxyStream(nodeID)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "model_unavailable",
			fmt.Sprintf("%s is not currently reachable: %v", nodeID, err))
		return
	}
	// Always released: on every return path below, including a caller that
	// stops reading before the response finishes. RecvHead/RecvChunk also
	// self-close on their own terminal conditions (see ProxyStream's doc);
	// Close is idempotent, so this is a blanket safety net, not a double
	// release.
	defer stream.Close()

	headers := make(map[string]string, len(r.Header))
	for k := range r.Header {
		switch k {
		case "Authorization", "Cookie", "Content-Length", "Host":
			// Same reasoning as the local-forward path just below: the
			// upstream is a local process on nodeID that has no use for
			// auth/hop headers meant for the gateway itself, and the body
			// was already fully buffered here (Content-Length would be
			// stale, Host is for this hop not that one).
			continue
		}
		headers[k] = r.Header.Get(k)
	}
	if err := stream.Send(&pb.HTTPRequestHead{Method: r.Method, Path: r.URL.RequestURI(), Headers: headers}); err != nil {
		writeErr(w, http.StatusBadGateway, "upstream_error",
			fmt.Sprintf("%s did not accept the request: %v", nodeID, err))
		return
	}
	if err := sendChunkedProxyBody(stream, body); err != nil {
		writeErr(w, http.StatusBadGateway, "upstream_error",
			fmt.Sprintf("%s did not accept the request body: %v", nodeID, err))
		return
	}

	head, err := stream.RecvHead()
	if err != nil {
		writeErr(w, http.StatusBadGateway, "upstream_error",
			fmt.Sprintf("%s did not answer: %v", nodeID, err))
		return
	}
	for k, v := range head.GetHeaders() {
		w.Header().Set(k, v)
	}
	status := int(head.GetStatus())
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	fl, canFlush := w.(http.Flusher)

	for {
		chunk, err := stream.RecvChunk()
		if err != nil {
			// The status line and headers are already written to w by this
			// point, so the only honest thing left to do on a mid-stream
			// failure (node disconnected, souslet reported an error) is
			// stop - a second status/error body now would be invisible to
			// most clients and would corrupt a response already in
			// progress.
			return
		}
		if len(chunk.GetData()) > 0 {
			if _, werr := w.Write(chunk.GetData()); werr != nil {
				// THE ORIGINAL CLIENT IS GONE (connection reset, browser
				// navigation, client-side cancel - all normal for a long
				// LLM/TTS/ASR generation someone gave up waiting on).
				// Unlike a plain forwarding loop that ignores this, stop
				// relaying immediately and let the deferred stream.Close()
				// release this stream_id, rather than continuing to drain a
				// response nobody will ever read. httputil.ReverseProxy's
				// own copyBuffer already checks its write error the same
				// way; this loop previously discarded it, which was both a
				// resource-usage bug and a factually wrong comment (it
				// claimed to mirror ReverseProxy's behavior while doing the
				// opposite) - both fixed here.
				//
				// DISCLOSED, DEFERRED LIMITATION: this bounds the GATEWAY
				// side only. It does not (yet) tell souslet to stop -
				// there's no Cancel/Abort message in the wire protocol
				// (proto/souslet/v1/souslet.proto, Task 1's already-
				// committed schema) to carry that signal, and adding one is
				// out of this fix's scope. souslet keeps forwarding
				// whatever the local model container produces until that
				// response completes naturally or the node disconnects; the
				// model itself may keep generating and burning GPU compute
				// for a request nobody's listening to anymore. A real fix
				// needs a new proto message type and is a good candidate
				// for a dedicated follow-up task.
				return
			}
			// Flushed after every chunk, exactly like the local path's
			// FlushInterval: -1 - without this, Go's own response buffering
			// would hold token-by-token SSE output until the whole
			// response completes, turning streaming into one long pause
			// followed by a wall of text.
			if canFlush {
				fl.Flush()
			}
		}
		if chunk.GetEof() {
			return
		}
		if r.Context().Err() != nil {
			// Caught here too, not just via a failed Write: a client can
			// disconnect between two chunks (e.g. right after a write that
			// happened to still succeed, or during an empty/keep-alive
			// chunk with no data to write at all), and this loop should not
			// wait for the NEXT write to notice - same reasoning and same
			// disclosed limitation as the write-error branch above.
			return
		}
	}
}

// proxyChunkSize matches handleProxyRequest's own response-side read size
// (internal/grpcclient/client.go) - not load-bearing that the two match
// exactly, just consistent, so anyone tracing a proxied request's frames on
// the wire sees one convention rather than two.
const proxyChunkSize = 4096

// sendChunkedProxyBody sends body as a series of fixed-size
// HTTPRequestChunk messages instead of one big one. This matters for
// correctness, not just style: grpc-go defaults to a 4MB max receive
// message size, and this package's own maxRequestBytes (32MB, "audio
// uploads are the large case") already documents that bodies well past 4MB
// are the expected case, not an edge case - a single oversized message
// would fail with ResourceExhausted inside souslet's connectOnce receive
// loop, and per Run's reconnect-on-any-stream-error design, that doesn't
// just fail the one request, it drops the ENTIRE node's gRPC connection,
// taking every other in-flight request and deployment on it down too.
// Mirrors handleProxyRequest's response-side chunking (client.go) exactly,
// just in the opposite direction.
func sendChunkedProxyBody(stream *grpcserver.ProxyStream, body []byte) error {
	if len(body) == 0 {
		return stream.SendChunk(nil, true)
	}
	for offset := 0; offset < len(body); offset += proxyChunkSize {
		end := offset + proxyChunkSize
		if end > len(body) {
			end = len(body)
		}
		if err := stream.SendChunk(body[offset:end], end == len(body)); err != nil {
			return err
		}
	}
	return nil
}

const maxRequestBytes = 32 << 20 // audio uploads are the large case

func (g *Gateway) host() string {
	if g.Host == "" {
		return "127.0.0.1"
	}
	return g.Host
}

// rewriteModel replaces the model field without disturbing anything else in the
// body - re-encoding from a typed struct would silently drop every field this
// gateway does not know about, which is most of them.
func rewriteModel(body []byte, to string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	enc, err := json.Marshal(to)
	if err != nil {
		return nil, err
	}
	m["model"] = enc
	return json.Marshal(m)
}

func asNoModel(err error, out *errNoModel) bool {
	if e, ok := err.(errNoModel); ok {
		*out = e
		return true
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr uses OpenAI's error envelope, because clients parse it. A bare
// string here makes every SDK report "unknown error".
func writeErr(w http.ResponseWriter, code int, kind, msg string) {
	writeJSON(w, code, map[string]any{
		"error": map[string]any{"message": msg, "type": kind, "code": kind},
	})
}

// multipartModel pulls the model out of a multipart form body.
//
// It parses a COPY and leaves r untouched, because the request still has to be
// forwarded upstream byte for byte - an audio upload re-encoded by a round trip
// through ParseMultipartForm is not the file the caller sent.
func multipartModel(r *http.Request, body []byte) string {
	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "multipart/form-data") {
		return ""
	}
	_, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return ""
	}
	boundary := params["boundary"]
	if boundary == "" {
		return ""
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := mr.NextPart()
		if err != nil {
			return ""
		}
		if part.FormName() != "model" {
			_ = part.Close()
			continue
		}
		// Bounded: a model name is short, and this is attacker-influenced input
		// on a path that has not authenticated the body yet.
		v, err := io.ReadAll(io.LimitReader(part, 1<<10))
		_ = part.Close()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(v))
	}
}

// allowedBy reports whether an allowlist covers this route.
//
// Matched against the ALIASES and the recipe id, not just the name the caller
// typed: a key scoped to "ornith" must keep working when someone addresses the
// same model as "ornith15", or the scope becomes a spelling test.
func allowedBy(allow []string, asked string, rt route) bool {
	names := append([]string{asked, rt.RecipeID}, rt.Aliases...)
	for _, a := range allow {
		for _, n := range names {
			if strings.EqualFold(a, n) {
				return true
			}
		}
	}
	return false
}

// authWithKey is a test seam: it puts a scoped key on a context the way the
// auth middleware does, without the gateway importing test code.
func authWithKey(ctx context.Context, models []string) context.Context {
	return auth.WithKeyForTest(ctx, auth.KeyInfo{Name: "test", Models: models})
}
