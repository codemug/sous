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
}

const (
	// Under Prometheus's default 10s scrape timeout with room to spare, so a
	// hung model costs the scrape its own samples rather than the whole scrape.
	defaultMetricsTimeout = 5 * time.Second
	// vLLM's own exposition is tens of KiB; this only stops a container that
	// answers /metrics with something else entirely from being held in memory
	// whole.
	defaultMetricsMaxBytes = 8 << 20
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

	// Concurrently, each under its own deadline, so the scrape takes as long
	// as the slowest model's timeout rather than the sum of them. The caller's
	// own request is the parent: a scraper that gave up stops the fetches too.
	targets := m.targets()
	results := make([]scrapeResult, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			ctx, cancel := context.WithTimeout(r.Context(), m.timeout())
			defer cancel()
			results[i].target = t
			if body, err := m.fetch(ctx, t.node, t.recipe); err == nil {
				results[i].fams, _ = parseExposition(body, t.node, t.recipe)
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

	w.Header().Set("Content-Type", metricsContentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b.Bytes())
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
func (m *Metrics) fetch(ctx context.Context, nodeID, recipeID string) ([]byte, error) {
	stream, err := m.GRPC.OpenProxyStream(nodeID)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	// ProxyStream's calls take no context; closing the stream is what unblocks
	// whichever of them is waiting when the deadline passes. Without this a
	// model that never answers would hold this scrape until its node
	// disconnected.
	defer context.AfterFunc(ctx, stream.Close)()

	body, err := json.Marshal(map[string]string{"model": recipeID})
	if err != nil {
		return nil, err
	}
	if err := stream.Send(&pb.HTTPRequestHead{
		Method: http.MethodGet, Path: "/metrics",
		Headers: map[string]string{"Content-Type": "application/json"},
	}); err != nil {
		return nil, err
	}
	if err := sendChunkedProxyBody(stream, body); err != nil {
		return nil, err
	}
	head, err := stream.RecvHead()
	if err != nil {
		return nil, err
	}
	// 0 is read as 200, as proxyOverGRPC reads it.
	if s := head.GetStatus(); s != 0 && s != http.StatusOK {
		return nil, fmt.Errorf("%s on %s answered /metrics with %d", recipeID, nodeID, s)
	}
	var out []byte
	for {
		chunk, err := stream.RecvChunk()
		if err != nil {
			return nil, err
		}
		// Refused rather than truncated: the first 8 MiB of an exposition is
		// not a smaller valid one, it is a body cut off mid-line.
		if int64(len(out)+len(chunk.GetData())) > m.maxBytes() {
			return nil, fmt.Errorf("%s on %s answered /metrics with more than %d bytes", recipeID, nodeID, m.maxBytes())
		}
		out = append(out, chunk.GetData()...)
		if chunk.GetEof() {
			return out, nil
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
func parseExposition(body []byte, node, recipeID string) (*exposition, error) {
	e := &exposition{}
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
	if _, err := strconv.ParseFloat(f[0], 64); err != nil {
		return "", "", fmt.Errorf("%s: value %q is not a number", name, f[0])
	}
	if len(f) == 2 {
		if _, err := strconv.ParseInt(f[1], 10, 64); err != nil {
			return "", "", fmt.Errorf("%s: timestamp %q is not an integer", name, f[1])
		}
	}

	have := make(map[string]bool, len(labels))
	for _, l := range labels {
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
