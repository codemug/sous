package deploy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/codemug/sous/internal/engine"
	"github.com/codemug/sous/internal/recipe"
)

// Phase is what a deployment is actually doing, as opposed to what its record
// says. It is DERIVED on every read and never stored.
//
// Storing it would be the same mistake the Drifted flag exists to correct: a
// record outlives the container it describes, so any state written down at
// deploy time starts lying the moment something dies outside Sous.
//
// The distinction that matters most here is Starting vs Ready. A vLLM model on
// this hardware runs for eight to ten minutes between "container is up" and
// "answers a request" - loading weights, compiling, capturing CUDA graphs. For
// that whole window the old dashboard showed it as deployed and running, which
// is true and useless: every client pointed at it got connection refused.
type Phase string

const (
	// PhaseStarting - container is up, the port is not answering yet.
	PhaseStarting Phase = "starting"
	// PhaseReady - the port answers. This is the only phase that means usable.
	PhaseReady Phase = "ready"
	// PhaseFailed - the container crashed, was OOM-killed, or is restarting.
	PhaseFailed Phase = "failed"
	// PhaseStopping - an undeploy is in flight. Held in memory, because it is
	// the one state with no evidence anywhere else: between the stop request
	// and the container disappearing, Docker still reports it as running.
	PhaseStopping Phase = "stopping"
	// PhaseGone - a record with no container behind it. What Drifted meant.
	PhaseGone Phase = "gone"
)

// Prober answers whether a model's port is serving.
//
// READINESS IS A PORT THAT ANSWERS, not a container that exists. Every kind
// Sous deploys speaks HTTP, and all of them 200 on some path long after the
// container is up - so this is the only check that distinguishes loading from
// serving without knowing anything about the model.
type Prober struct {
	// Host is the address deployments are bound to.
	Host string
	// Timeout is deliberately short. This runs on a dashboard refresh, and a
	// model that needs longer than this to answer a health check is not ready
	// by any definition a caller cares about.
	Timeout time.Duration

	mu    sync.Mutex
	cache map[int]probe
}

type probe struct {
	ok   bool
	when time.Time
}

// probeTTL keeps a dashboard refresh from turning into a burst of health
// requests against every model on the node. Short enough that a model going
// ready shows up promptly.
const probeTTL = 3 * time.Second

// DefaultHealthPath is what every image this project deploys exposes. A recipe
// whose image uses something else says so; see Recipe.HealthPath.
const DefaultHealthPath = "/health"

// Ready reports whether the health path answers 200, with a brief cache.
func (p *Prober) Ready(ctx context.Context, port int, path string) bool {
	if port <= 0 {
		return false
	}
	p.mu.Lock()
	if p.cache == nil {
		p.cache = map[int]probe{}
	}
	if c, ok := p.cache[port]; ok && time.Since(c.when) < probeTTL {
		p.mu.Unlock()
		return c.ok
	}
	p.mu.Unlock()

	ok := p.check(ctx, port, path)

	p.mu.Lock()
	p.cache[port] = probe{ok: ok, when: time.Now()}
	p.mu.Unlock()
	return ok
}

func (p *Prober) check(ctx context.Context, port int, path string) bool {
	to := p.Timeout
	if to <= 0 {
		to = 2 * time.Second
	}
	host := p.Host
	if host == "" {
		host = "127.0.0.1"
	}
	if path == "" {
		path = DefaultHealthPath
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()

	// 200 AND NOTHING ELSE. An earlier version treated any answer as proof the
	// server was up, which gets the interesting case backwards: a server that
	// binds its port before its engine has finished loading answers 404 or 503
	// on this path, and calling that ready sends traffic into a model that
	// cannot serve it. That is the exact failure the phase exists to prevent,
	// so the check has to be the strict one.
	return resp.StatusCode == http.StatusOK
}

// Phase computes one deployment's phase from live evidence.
//
// The order of these checks is the whole design. Stopping is asserted by Sous
// and beats everything, because during a stop Docker still reports the
// container as running and would otherwise be believed. Failure beats
// readiness, because a crash-looping container passes through "running"
// repeatedly and a probe that catches it mid-loop would report it healthy.
func (m *Manager) Phase(ctx context.Context, rec Record, st engine.ContainerState, known bool) Phase {
	if m.stopping(rec.RecipeID) {
		return PhaseStopping
	}
	if !known {
		// The runtime could not be reached. Claiming a phase from a record
		// alone is exactly the lie this type exists to avoid.
		return PhaseStarting
	}
	if st.Name == "" {
		return PhaseGone
	}
	if st.Crashed() {
		return PhaseFailed
	}
	if !st.Running() {
		return PhaseGone
	}
	if m.Probe != nil && m.Probe.Ready(ctx, rec.HostPort, m.healthPath(rec.RecipeID)) {
		return PhaseReady
	}
	return PhaseStarting
}

// markStopping and clearStopping bracket an in-flight undeploy.
func (m *Manager) markStopping(id string) {
	m.stopMu.Lock()
	defer m.stopMu.Unlock()
	if m.stopSet == nil {
		m.stopSet = map[string]time.Time{}
	}
	m.stopSet[id] = time.Now()
}

func (m *Manager) clearStopping(id string) {
	m.stopMu.Lock()
	defer m.stopMu.Unlock()
	delete(m.stopSet, id)
}

// stopMaxAge bounds how long a stop may be believed. If Sous is killed
// mid-undeploy the flag would otherwise be permanent, and a model stuck on
// "stopping" forever is worse than one briefly mislabelled.
const stopMaxAge = 5 * time.Minute

func (m *Manager) stopping(id string) bool {
	m.stopMu.Lock()
	defer m.stopMu.Unlock()
	t, ok := m.stopSet[id]
	if !ok {
		return false
	}
	if time.Since(t) > stopMaxAge {
		delete(m.stopSet, id)
		return false
	}
	return true
}

// UndeployAsync starts a stop and returns immediately.
//
// WHY NOT SYNCHRONOUS. Docker's stop carries a 60 second grace period, and a
// 61 GiB model uses most of it releasing the pool. Doing that inside the
// request handler meant the browser sat on a hanging POST for a minute with no
// indication anything was happening - which is indistinguishable from a broken
// button, and led to it being clicked again.
//
// The caller gets PhaseStopping on the next status poll and a model that
// disappears when the container actually does.
func (m *Manager) UndeployAsync(id string) error {
	if !recipe.ValidID(id) {
		return fmt.Errorf("undeploy: invalid id %q", id)
	}
	m.markStopping(id)
	go func() {
		defer m.clearStopping(id)
		// Detached from the request: the HTTP context is cancelled the moment
		// the handler returns, which would cancel the very stop it started.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		_ = m.Undeploy(ctx, id)
	}()
	return nil
}

// States delegates to the runtime so callers that only need to know what is
// live - the gateway - do not have to reach through Manager to Runtime and
// take a dependency on the whole engine.
func (m *Manager) States(ctx context.Context) (map[string]engine.ContainerState, error) {
	return m.Runtime.States(ctx)
}

// healthPath is the recipe's override, or the default.
//
// An override exists because Sous deploys third-party images it does not
// control, and this codebase has already been bitten once by assuming an image
// follows a convention: kokoro declares EXPOSE 8000 and listens on 8880, which
// produced a container every indicator called healthy and which answered
// nothing. Without an override, an image with no /health would sit on
// "starting" forever - visible, but permanently wrong.
func (m *Manager) healthPath(id string) string {
	if m.Catalog == nil {
		return DefaultHealthPath
	}
	r, err := m.Catalog.Get(id)
	if err != nil || r.HealthPath == "" {
		return DefaultHealthPath
	}
	return r.HealthPath
}
