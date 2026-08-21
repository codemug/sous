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
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

type Gateway struct {
	Res  Resolver
	Cat  Catalog
	Host string

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
	now := g.now().Unix()
	data := make([]modelObj, 0, len(rs))
	for _, rt := range rs {
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
		// Multipart audio requests carry the model as a form field rather than
		// JSON, which is how the ASR endpoint is called.
		if v := r.FormValue("model"); v != "" {
			name = v
		}
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
