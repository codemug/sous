package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/codemug/sous/internal/grpcserver"
	"github.com/codemug/sous/internal/nodecatalog"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
	"github.com/codemug/sous/internal/recipe"
)

// Metrics serves one Prometheus scrape that federates the /metrics of every
// vLLM model on every connected node.
//
// WHY THROUGH HERE. A souslet publishes each model on its node's loopback and
// picks the port at deploy time, so a scraper can neither reach a model's own
// /metrics nor know where it is this week. sous-api already reaches every
// model - the gateway's proxy stream to each node - so it is the one place a
// scrape of every model can be answered from.
//
// A TYPE OF ITS OWN, not a Gateway method, because it is served on a listener
// with no authentication (see cmd/sous-api's -metrics-listen). A *Gateway on
// that listener is one route away from proxying inference unauthenticated;
// this type can only scrape, so it cannot be wired into doing anything else.
//
// NO PROMETHEUS CLIENT LIBRARY. Nothing here is instrumented - the samples are
// the models' own, rewritten line by line - so the library would bring a
// dependency tree for a text format that takes a page to read and write.
type Metrics struct {
	Nodes *nodecatalog.Catalog
	GRPC  *grpcserver.Server
	Cat   Catalog

	// Timeout and MaxBytes bound each model's fetch on its own; zero means
	// the defaults below. Settable so a test can hang a model without waiting
	// out the real timeout.
	Timeout  time.Duration
	MaxBytes int64
	// MaxSamples caps the samples taken from one model on one scrape; zero
	// means defaultMetricsMaxSamples.
	MaxSamples int
	// MinInterval is how long one scrape's answer is reused for. Zero means
	// defaultMetricsMinInterval; negative means never reuse one.
	MinInterval time.Duration
	// Now is the clock the reuse and the backoff below read; nil means
	// time.Now.
	Now func() time.Time

	// turn is the scrape lock: whoever has put a value in it is scraping. A
	// channel rather than a mutex so that a caller waiting for it can give up
	// when its client does.
	//
	// The listener is unauthenticated, so how often it is asked is not this
	// process's to decide; how much work each asking can start on the nodes
	// is. One fan-out at a time, each bounded by Timeout, and at most one per
	// MinInterval.
	once sync.Once
	turn chan struct{}
	// last is the previous scrape's answer and lastAt when it was made. Both
	// are touched only while holding turn. last is never written to again
	// once made, which is what makes handing it out after releasing turn safe.
	last   []byte
	lastAt time.Time

	// backoff holds the models that timed out, and until when to leave them
	// alone. See skip.
	backoffMu sync.Mutex
	backoff   map[scrapeTarget]backoffState
}

type backoffState struct {
	until    time.Time
	timeouts int
}

const (
	// A model that timed out is next asked after this long, doubling with
	// each further timeout up to the cap.
	metricsBackoffBase = 30 * time.Second
	metricsBackoffMax  = 5 * time.Minute
)

func (m *Metrics) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// skip reports whether t timed out recently enough to be left alone.
//
// A SOUSLET CANNOT CANCEL A REQUEST IT IS FORWARDING. When a fetch here gives
// up, this side is freed, but the node's request to the container stays open
// until the container answers it. A model that never answers, asked on every
// scrape, would collect one more stuck request on its node every scrape
// interval for as long as it stays hung. Backing off bounds that to a few an
// hour, and costs a recovering model at most metricsBackoffMax of missing
// metrics.
//
// ONLY A TIMEOUT. A refusal or an error envelope comes straight back and
// leaves nothing open, so those are asked again on the next scrape and are
// seen the moment they recover.
func (m *Metrics) skip(t scrapeTarget) bool {
	m.backoffMu.Lock()
	defer m.backoffMu.Unlock()
	st, ok := m.backoff[t]
	return ok && m.now().Before(st.until)
}

func (m *Metrics) timedOut(t scrapeTarget) {
	m.backoffMu.Lock()
	defer m.backoffMu.Unlock()
	if m.backoff == nil {
		m.backoff = map[scrapeTarget]backoffState{}
	}
	st := m.backoff[t]
	d := metricsBackoffMax
	// 30s << 4 is already past the cap; the bound also keeps the shift from
	// overflowing after days of timeouts.
	if st.timeouts < 4 {
		d = metricsBackoffBase << st.timeouts
	}
	m.backoff[t] = backoffState{until: m.now().Add(d), timeouts: st.timeouts + 1}
}

// answered forgets t's timeouts. Also called for a model that failed some
// other way: it answered, which is all the backoff is about.
func (m *Metrics) answered(t scrapeTarget) {
	m.backoffMu.Lock()
	defer m.backoffMu.Unlock()
	delete(m.backoff, t)
}

// forget drops the backoff of every model that is no longer a target, so a
// recipe undeployed while hung does not stay in the map for ever.
func (m *Metrics) forget(current []scrapeTarget) {
	m.backoffMu.Lock()
	defer m.backoffMu.Unlock()
	if len(m.backoff) == 0 {
		return
	}
	keep := make(map[scrapeTarget]bool, len(current))
	for _, t := range current {
		keep[t] = true
	}
	for t := range m.backoff {
		if !keep[t] {
			delete(m.backoff, t)
		}
	}
}

const (
	// Under Prometheus's default 10s scrape timeout with room to spare, so a
	// hung model costs the scrape its own samples rather than the whole scrape.
	defaultMetricsTimeout = 5 * time.Second
	// vLLM's own exposition is tens of KiB; this only stops a container that
	// answers /metrics with something else entirely from being held in memory
	// whole.
	defaultMetricsMaxBytes = 8 << 20

	// A real vLLM serves about 600 samples. The byte cap alone is not enough:
	// 8 MiB of the shortest possible sample is two million lines, each of
	// which grows by the labels added here - 112 MiB of output from one model
	// when it was measured.
	defaultMetricsMaxSamples = 50_000

	// Shorter than any sane scrape interval, so a store scraping every 15s
	// always gets a fresh answer, and long enough that a loop hammering the
	// port starts one fan-out to the nodes every few seconds rather than one
	// per request.
	defaultMetricsMinInterval = 5 * time.Second
)

// metricsContentType is the classic text format. Not OpenMetrics: the
// request to the model sends no Accept header, so that is what vLLM answers
// with, and what is relayed is what was received.
const metricsContentType = "text/plain; version=0.0.4; charset=utf-8"

// ServeHTTP answers GET /metrics and nothing else. It always answers 200 with
// whatever could be fetched: a model that fails is a 0 in
// sous_model_scrape_up, never a failed scrape, because one broken container
// must not blind the scraper to every other model on the fleet.
func (m *Metrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Checked by hand rather than registered on a ServeMux: a "GET /metrics"
	// pattern also answers HEAD, and HEAD would then fan a scrape out to every
	// node to send back no body.
	if r.URL.Path != "/metrics" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, ok := m.scrape(r.Context())
	if !ok {
		return // the client left while waiting its turn; there is nobody to answer
	}
	// WRITTEN AFTER THE TURN IS GIVEN UP, never under it. A client that asks
	// and then does not read would otherwise hold the lock for as long as it
	// cared to, and every later scrape with it.
	w.Header().Set("Content-Type", metricsContentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// scrape returns the answer to one scrape, and false if ctx ended before it
// had a turn.
//
// Inside MinInterval of the last scrape, the answer is that scrape's.
func (m *Metrics) scrape(ctx context.Context) ([]byte, bool) {
	m.once.Do(func() { m.turn = make(chan struct{}, 1) })
	select {
	case m.turn <- struct{}{}:
	case <-ctx.Done():
		return nil, false
	}
	defer func() { <-m.turn }()

	if iv := m.minInterval(); iv > 0 && m.last != nil && m.now().Sub(m.lastAt) < iv {
		return m.last, true
	}
	m.last, m.lastAt = m.collect(), m.now()
	return m.last, true
}

// collect asks every target for its metrics and builds the answer.
//
// Concurrently, each under its own deadline, so it takes as long as the
// slowest model's timeout rather than the sum of them.
//
// THE FETCHES BELONG TO THE SCRAPE, NOT TO WHOEVER ASKED FOR IT. They used to
// run under the request's context, so a scraper that hung up cancelled them
// - and a cancelled fetch of a hung model looked like a model that had
// answered, which cleared its backoff. Ten scrapes that each gave up early
// left ten stuck requests on the node. Now a fetch runs to its own deadline
// whatever the caller does, and its result is there for the next one.
func (m *Metrics) collect() []byte {
	targets := m.targets()
	m.forget(targets)
	results := make([]scrapeResult, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		results[i].target = t
		if m.skip(t) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), m.timeout())
			defer cancel()
			body, sent, err := m.fetch(ctx, t.node, t.recipe)
			switch classify(err, sent, errors.Is(ctx.Err(), context.DeadlineExceeded)) {
			case modelTimedOut:
				m.timedOut(t)
			case modelAnswered:
				m.answered(t)
			}
			if err == nil {
				// A 200 with no sample in it is not a model reporting metrics.
				if fams, perr := parseExposition(body, t.node, t.recipe, m.maxSamples()); perr == nil && fams.samples() > 0 {
					results[i].fams = fams
				}
			}
			results[i].took = time.Since(start)
		}()
	}
	wg.Wait()

	all := &exposition{}
	for _, res := range results {
		if res.fams != nil {
			all.merge(res.fams)
		}
	}
	var b bytes.Buffer
	all.write(&b)

	b.WriteString("# HELP sous_model_scrape_up 1 if this model's own /metrics was fetched through its node and parsed on this scrape, else 0.\n")
	b.WriteString("# TYPE sous_model_scrape_up gauge\n")
	for _, res := range results {
		up := 0
		if res.fams != nil {
			up = 1
		}
		fmt.Fprintf(&b, "sous_model_scrape_up{%s} %d\n", res.target.labels(), up)
	}
	b.WriteString("# HELP sous_model_scrape_duration_seconds How long fetching and parsing this model's /metrics took on this scrape.\n")
	b.WriteString("# TYPE sous_model_scrape_duration_seconds gauge\n")
	for _, res := range results {
		fmt.Fprintf(&b, "sous_model_scrape_duration_seconds{%s} %s\n",
			res.target.labels(), strconv.FormatFloat(res.took.Seconds(), 'f', -1, 64))
	}
	// Every node the catalog knows, connected or not. A disconnected node has
	// no models in the scrape above (it is not asked), and without this a
	// node that dropped off looks the same as one that never ran anything.
	b.WriteString("# HELP sous_node_connected 1 if this node's souslet is connected to sous-api, 0 if it is known but disconnected.\n")
	b.WriteString("# TYPE sous_node_connected gauge\n")
	if m.Nodes != nil {
		ns := m.Nodes.All()
		sort.Slice(ns, func(i, j int) bool { return ns[i].NodeID < ns[j].NodeID })
		for _, n := range ns {
			c := 0
			if n.Connected {
				c = 1
			}
			fmt.Fprintf(&b, "sous_node_connected{%s} %d\n", labelPair("node", n.NodeID), c)
		}
	}
	return b.Bytes()
}

// outcome is what one fetch says about its model, for the backoff.
type outcome int

const (
	// modelAnswered: it replied, with metrics or with a refusal. Nothing is
	// left open on the node.
	modelAnswered outcome = iota
	// modelTimedOut: it was asked and the deadline passed first. The node is
	// still holding that request.
	modelTimedOut
	// modelUnknown: the request never got as far as the node - its send
	// queue was full, or it had gone. That says nothing about the model, so
	// the backoff is left as it was.
	modelUnknown
)

func classify(err error, sent, deadlinePassed bool) outcome {
	switch {
	case err != nil && !sent:
		return modelUnknown
	case err != nil && deadlinePassed:
		return modelTimedOut
	}
	return modelAnswered
}

type scrapeTarget struct{ node, recipe string }

func (t scrapeTarget) labels() string {
	return labelPair("node", t.node) + "," + labelPair("recipe", t.recipe)
}

type scrapeResult struct {
	target scrapeTarget
	fams   *exposition // nil when the fetch or the parse failed
	took   time.Duration
}

// targets is every vLLM model on a connected node, against the node requests
// for it are routed to - Placements, so a recipe on two nodes is scraped once,
// from the same node the gateway would send its traffic to.
//
// VLLM ONLY. vLLM serves Prometheus metrics at /metrics; a container or
// transformers recipe may answer that path with anything, or hold it open.
// A recipe missing from the catalog cannot be said to be vLLM, so it is
// skipped rather than guessed at.
func (m *Metrics) targets() []scrapeTarget {
	if m.Nodes == nil || m.GRPC == nil || m.Cat == nil {
		return nil
	}
	var out []scrapeTarget
	for _, p := range m.Nodes.Placements() {
		id := p.Deployment.GetRecipeId()
		rec, err := m.Cat.Get(id)
		if err != nil || rec.Kind != recipe.KindVLLM {
			continue
		}
		out = append(out, scrapeTarget{node: p.NodeID, recipe: id})
	}
	return out
}

// fetch asks nodeID's souslet for recipeID's /metrics over the same proxy
// stream inference travels on, and returns the body only if all of it arrived
// with a 200.
//
// THE REQUEST IS SHAPED FOR THE SOUSLET ALREADY DEPLOYED (v0.22.5), which must
// not need an upgrade for this: it forwards the method and path verbatim to
// the container's loopback port, and finds that port by the "model" in a JSON
// body - the head carries no model field. So the request is GET /metrics with
// a JSON body naming the recipe. vLLM ignores a body on GET.
//
// NOTHING FROM THE SCRAPER IS FORWARDED. Not its credentials (there should be
// none on this listener, but a scraper configured with one by mistake must not
// hand it to a container), and not its Accept: an OpenMetrics answer would be
// relayed under a text-format Content-Type.
//
// sent reports whether the whole request was handed to the node's connection
// before anything went wrong - the difference between a model that did not
// answer and one that was never asked.
func (m *Metrics) fetch(ctx context.Context, nodeID, recipeID string) (out []byte, sent bool, err error) {
	stream, err := m.GRPC.OpenProxyStream(nodeID)
	if err != nil {
		return nil, false, err
	}
	defer stream.Close()
	// Closing the stream is what unblocks a call waiting on it, so the
	// deadline is enforced by closing it. The returned stop is deferred so a
	// fetch that finished in time does not leave the callback armed.
	defer context.AfterFunc(ctx, stream.Close)()

	body, err := json.Marshal(map[string]string{"model": recipeID})
	if err != nil {
		return nil, false, err
	}
	if err := stream.Send(&pb.HTTPRequestHead{
		Method: http.MethodGet, Path: "/metrics",
		Headers: map[string]string{"Content-Type": "application/json"},
	}); err != nil {
		return nil, false, err
	}
	if err := sendChunkedProxyBody(stream, body); err != nil {
		return nil, false, err
	}
	head, err := stream.RecvHead()
	if err != nil {
		return nil, true, err
	}
	// A souslet that sets no status means 200, as it does on the inference
	// path.
	if s := head.GetStatus(); s != 0 && s != http.StatusOK {
		return nil, true, fmt.Errorf("%s on %s answered /metrics with %d", recipeID, nodeID, s)
	}
	for {
		chunk, err := stream.RecvChunk()
		if err != nil {
			return nil, true, err
		}
		// Checked before appending, so a body past the cap is never held.
		if int64(len(out)+len(chunk.GetData())) > m.maxBytes() {
			return nil, true, fmt.Errorf("%s on %s answered /metrics with more than %d bytes", recipeID, nodeID, m.maxBytes())
		}
		out = append(out, chunk.GetData()...)
		if chunk.GetEof() {
			return out, true, nil
		}
	}
}

func (m *Metrics) timeout() time.Duration {
	if m.Timeout > 0 {
		return m.Timeout
	}
	return defaultMetricsTimeout
}

func (m *Metrics) maxBytes() int64 {
	if m.MaxBytes > 0 {
		return m.MaxBytes
	}
	return defaultMetricsMaxBytes
}

func (m *Metrics) maxSamples() int {
	if m.MaxSamples > 0 {
		return m.MaxSamples
	}
	return defaultMetricsMaxSamples
}

func (m *Metrics) minInterval() time.Duration {
	if m.MinInterval != 0 {
		return m.MinInterval
	}
	return defaultMetricsMinInterval
}

// ---- the text format ----------------------------------------------------

// exposition is metric families in the order first seen. Each is emitted as
// one block - its HELP, its TYPE, then every model's samples for it -
// because a repeated HELP or TYPE for one family is a parse error to a strict
// scraper, and a family split around another one is to the stricter ones.
type exposition struct {
	order []string
	fams  map[string]*family
}

type family struct {
	help, typ string // the comment lines to emit; "" if none was seen
	kind      string // the TYPE's type word, to tell which samples are this family's
	samples   []string
}

func (e *exposition) samples() int {
	n := 0
	for _, f := range e.fams {
		n += len(f.samples)
	}
	return n
}

func (e *exposition) get(name string) *family {
	if e.fams == nil {
		e.fams = map[string]*family{}
	}
	f, ok := e.fams[name]
	if !ok {
		f = &family{}
		e.fams[name] = f
		e.order = append(e.order, name)
	}
	return f
}

// merge adds o's families to e. HELP and TYPE: first seen wins, so two
// models disagreeing on a family's help text still produce one declaration.
func (e *exposition) merge(o *exposition) {
	for _, name := range o.order {
		src, dst := o.fams[name], e.get(name)
		if dst.help == "" {
			dst.help = src.help
		}
		if dst.typ == "" {
			dst.typ, dst.kind = src.typ, src.kind
		}
		dst.samples = append(dst.samples, src.samples...)
	}
}

func (e *exposition) write(b *bytes.Buffer) {
	for _, name := range e.order {
		f := e.fams[name]
		for _, l := range [][]string{{f.help}, {f.typ}, f.samples} {
			for _, s := range l {
				if s != "" {
					b.WriteString(s)
					b.WriteByte('\n')
				}
			}
		}
	}
}

// parseExposition reads one model's text-format body into families, with
// node and recipe added to every sample. HELP and TYPE lines are kept, every
// other comment dropped.
//
// ALL OR NOTHING. A line that is not a sample fails the whole body, as it
// would fail a Prometheus scrape of that model directly: half of a model
// served as though it were all of it is worse than none of it and a 0 in
// sous_model_scrape_up.
//
// AND STRICT, where a real parser is. This output is one scrape for every
// model, and a store that rejects a scrape on its first malformed line would
// lose all of them to one container's bad one. So what the standard parser
// refuses is refused here, for that model alone: invalid UTF-8, an unknown
// TYPE, a bad escape, a repeated or reserved label name, a value Go would
// read as a float and the format would not.
//
// maxSamples bounds how many samples are taken; zero means no bound.
func parseExposition(body []byte, node, recipeID string, maxSamples int) (*exposition, error) {
	if !utf8.Valid(body) {
		return nil, errors.New("not valid UTF-8")
	}
	e := &exposition{}
	n := 0
	// The family the last HELP or TYPE declared. A histogram's _bucket, _sum
	// and _count samples are that family's, not three families of their own.
	cur := ""
	for i, line := range strings.Split(string(body), "\n") {
		line = strings.TrimLeft(strings.TrimRight(line, " \t\r"), " \t")
		if line == "" {
			continue
		}
		if line[0] == '#' {
			kw, name, rest, ok := metaLine(line)
			if !ok {
				continue
			}
			if kw == "TYPE" && !validType(rest) {
				return nil, fmt.Errorf("line %d: %q is not a metric type", i+1, rest)
			}
			if kw == "HELP" && !validHelp(rest) {
				return nil, fmt.Errorf("line %d: HELP for %s has an escape the format does not have", i+1, name)
			}
			f := e.get(name)
			cur = name
			if kw == "HELP" && f.help == "" {
				f.help = strings.TrimRight("# HELP "+name+" "+rest, " ")
			}
			if kw == "TYPE" && f.typ == "" {
				f.typ, f.kind = "# TYPE "+name+" "+rest, rest
			}
			continue
		}
		name, out, err := relabel(line, node, recipeID)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		if n++; maxSamples > 0 && n > maxSamples {
			return nil, fmt.Errorf("more than %d samples", maxSamples)
		}
		fam := name
		if cur != "" && sampleOf(name, cur, e.fams[cur].kind) {
			fam = cur
		}
		f := e.get(fam)
		f.samples = append(f.samples, out)
	}
	return e, nil
}

// metaLine splits "# HELP name doc" or "# TYPE name type". Any other comment
// - including one of those two missing its name, or a TYPE missing its type -
// is not one, and is dropped like any other comment.
func metaLine(line string) (kw, name, rest string, ok bool) {
	kw, s := cutBlank(strings.TrimLeft(line[1:], " \t"))
	if kw != "HELP" && kw != "TYPE" {
		return "", "", "", false
	}
	name, rest = cutBlank(s)
	if !validMetricName(name) || (kw == "TYPE" && rest == "") {
		return "", "", "", false
	}
	return kw, name, rest, true
}

// sampleOf reports whether a sample called name belongs to the family fam of
// type kind: the family's own name, or one of the suffixed series a histogram
// or summary is written out as.
func sampleOf(name, fam, kind string) bool {
	if name == fam {
		return true
	}
	suffix, ok := strings.CutPrefix(name, fam)
	if !ok {
		return false
	}
	switch kind {
	case "histogram":
		return suffix == "_bucket" || suffix == "_sum" || suffix == "_count"
	case "summary":
		return suffix == "_sum" || suffix == "_count"
	}
	return false
}

type label struct{ name, value string }

// relabel rewrites one sample line with node and recipe added to its labels,
// and returns the metric name with it.
//
// The name ends at the first brace or blank - a brace inside a quoted label
// value is the value's, which is why labels are walked rather than split.
// Incoming values are carried through exactly as escaped on the way in; only
// the two values added here are escaped, by labelPair.
//
// A NODE OR RECIPE LABEL ALREADY ON THE SAMPLE is renamed exported_node /
// exported_recipe (prefixed again while that too is taken) - the rename
// Prometheus itself applies when a target label would clash with an exposed
// one. A sample with a label name twice is a parse error, and dropping the
// incoming one would lose what the model said.
func relabel(line, node, recipeID string) (string, string, error) {
	if !utf8.ValidString(line) {
		return "", "", errors.New("not valid UTF-8")
	}
	s := strings.TrimLeft(line, " \t")
	end := strings.IndexAny(s, "{ \t")
	if end < 0 {
		return "", "", errors.New("a sample with no value")
	}
	name := s[:end]
	if !validMetricName(name) {
		return "", "", fmt.Errorf("%q is not a metric name", name)
	}
	s = strings.TrimLeft(s[end:], " \t")
	var labels []label
	if strings.HasPrefix(s, "{") {
		var err error
		if labels, s, err = parseLabels(s[1:]); err != nil {
			return "", "", fmt.Errorf("%s: %w", name, err)
		}
	}
	// What is left is the value and, optionally, a timestamp - both kept as
	// written, but checked, so text that merely starts with a word is not
	// passed off as a sample.
	f := strings.Fields(s)
	if len(f) == 0 || len(f) > 2 {
		return "", "", fmt.Errorf("%s: want a value and an optional timestamp, got %q", name, s)
	}
	if !validValue(f[0]) {
		return "", "", fmt.Errorf("%s: value %q is not a number", name, f[0])
	}
	if len(f) == 2 {
		if _, err := strconv.ParseInt(f[1], 10, 64); err != nil {
			return "", "", fmt.Errorf("%s: timestamp %q is not an integer", name, f[1])
		}
	}

	have := make(map[string]bool, len(labels))
	for _, l := range labels {
		if have[l.name] {
			return "", "", fmt.Errorf("%s: label %s given twice", name, l.name)
		}
		// Names starting __ are the store's own; __name__ in particular
		// would let a sample call itself a different metric.
		if strings.HasPrefix(l.name, "__") {
			return "", "", fmt.Errorf("%s: label name %s is reserved", name, l.name)
		}
		have[l.name] = true
	}
	for i, l := range labels {
		if l.name != "node" && l.name != "recipe" {
			continue
		}
		n := l.name
		for have[n] || n == "node" || n == "recipe" {
			n = "exported_" + n
		}
		have[n] = true
		labels[i].name = n
	}

	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('{')
	for _, l := range labels {
		b.WriteString(l.name)
		b.WriteString(`="`)
		b.WriteString(l.value)
		b.WriteString(`",`)
	}
	b.WriteString(labelPair("node", node))
	b.WriteByte(',')
	b.WriteString(labelPair("recipe", recipeID))
	b.WriteString("} ")
	b.WriteString(strings.Join(f, " "))
	return name, b.String(), nil
}

// parseLabels reads `name="value", ...}` - s starts just past the opening
// brace - and returns the labels with their values still escaped, and what
// follows the closing brace.
func parseLabels(s string) ([]label, string, error) {
	var out []label
	for {
		s = strings.TrimLeft(s, " \t")
		if s == "" {
			return nil, "", errors.New("labels not closed")
		}
		if s[0] == '}' {
			return out, s[1:], nil
		}
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			return nil, "", errors.New("a label with no value")
		}
		name := strings.TrimRight(s[:eq], " \t")
		if !validLabelName(name) {
			return nil, "", fmt.Errorf("%q is not a label name", name)
		}
		s = strings.TrimLeft(s[eq+1:], " \t")
		if !strings.HasPrefix(s, `"`) {
			return nil, "", fmt.Errorf("label %s: value is not quoted", name)
		}
		// The value runs to the first quote that is not escaped.
		i := 1
		for ; i < len(s) && s[i] != '"'; i++ {
			if s[i] == '\\' {
				i++
				if i >= len(s) || (s[i] != '\\' && s[i] != '"' && s[i] != 'n') {
					return nil, "", fmt.Errorf("label %s: an escape the format does not have", name)
				}
			}
		}
		if i >= len(s) {
			return nil, "", fmt.Errorf("label %s: value not closed", name)
		}
		out = append(out, label{name: name, value: s[1:i]})
		s = strings.TrimLeft(s[i+1:], " \t")
		switch {
		case strings.HasPrefix(s, ","):
			s = s[1:]
		case strings.HasPrefix(s, "}"):
		default:
			return nil, "", fmt.Errorf("label %s: want , or } after its value", name)
		}
	}
}

// labelValueEscaper is the text format's escaping for a label value:
// backslash, double quote and line feed, nothing else.
var labelValueEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func labelPair(name, value string) string {
	return name + `="` + labelValueEscaper.Replace(value) + `"`
}

// cutBlank splits s at its first run of blanks.
func cutBlank(s string) (before, after string) {
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimLeft(s[i:], " \t")
}

// validValue is the exposition format's idea of a number, which is narrower
// than Go's: strconv also reads hex floats ("0x1p3") and digit separators
// ("1_0"). This is the check the standard parser makes before it calls
// strconv, for the same reason.
func validValue(s string) bool {
	if strings.ContainsAny(s, "pP_") {
		return false
	}
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

func validType(s string) bool {
	switch s {
	case "counter", "gauge", "histogram", "summary", "untyped":
		return true
	}
	return false
}

// validHelp accepts the two escapes HELP text has, \\ and \n, and no other.
func validHelp(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			continue
		}
		i++
		if i >= len(s) || (s[i] != '\\' && s[i] != 'n') {
			return false
		}
	}
	return true
}

func validMetricName(s string) bool {
	return validName(s, true)
}

func validLabelName(s string) bool {
	return validName(s, false)
}

// validName is [a-zA-Z_:][a-zA-Z0-9_:]* for a metric, the same without the
// colons for a label. vLLM's own names use the colon (vllm:...).
func validName(s string, colon bool) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		switch {
		case c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
		case c == ':' && colon:
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
