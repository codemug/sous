package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/codemug/sous/internal/deploy"
	"github.com/codemug/sous/internal/engine"
	"github.com/codemug/sous/internal/recipe"
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
