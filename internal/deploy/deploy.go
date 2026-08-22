// Package deploy coordinates everything else. It is the only package that
// knows the ordering rules, and those rules are not stylistic:
//
//   - loads are SERIALISED, because two vLLM processes memory-profiling the
//     same unified pool concurrently is a documented crash on this hardware:
//     "Error in memory profiling ... other processes sharing the same container
//     release GPU memory while vLLM is profiling";
//   - page cache is DROPPED before every start, because vLLM sizes KV from
//     CUDA-reported free memory and the kernel does not count reclaimable page
//     cache as free, so 25-35 GiB of just-read safetensors looks like memory
//     that is gone. This node has OOM'd on a SMALLER model for that reason;
//   - an existing container is STOPPED before its replacement starts, because
//     the outgoing one holds both the port and the memory the incoming one
//     needs, and the resulting errors name the wrong model.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sync"
	"time"

	"github.com/codemug/sous/internal/capacity"
	"github.com/codemug/sous/internal/catalog"
	"github.com/codemug/sous/internal/engine"
	"github.com/codemug/sous/internal/observe"
	"github.com/codemug/sous/internal/ports"
	"github.com/codemug/sous/internal/recipe"
	"github.com/codemug/sous/internal/store"
)

type Runtime interface {
	Start(ctx context.Context, spec engine.Spec) (containerID string, err error)
	Stop(ctx context.Context, name string) error
	Logs(ctx context.Context, name string) (io.ReadCloser, error)
	Running(ctx context.Context) ([]string, error)
	// States reports every Sous container INCLUDING stopped and crashed ones.
	// Running() answers only "does a container with this name exist", which
	// cannot separate a model still loading from one serving, nor a clean stop
	// from a crash loop - four situations that need four different responses
	// and used to look identical.
	States(ctx context.Context) (map[string]engine.ContainerState, error)
	// ImageExposedPort reports the port an image listens on inside the
	// container. Only KindContainer needs it: for the kinds Sous generates a
	// command for, it sets the port itself.
	ImageExposedPort(ctx context.Context, ref string) (int, error)
}

// CapacityError is distinct so callers can render the margin and the way out
// rather than a flat failure.
type CapacityError struct {
	RecipeID string
	Result   capacity.Result
}

func (e *CapacityError) Error() string {
	return fmt.Sprintf("%s does not fit: short by %.1f GiB; free one of %v",
		e.RecipeID, -e.Result.MarginGiB, e.Result.MustFree)
}

type Manager struct {
	Store    *store.Store
	Catalog  *catalog.Catalog
	Runtime  Runtime
	Planner  capacity.Planner
	Ports    ports.Allocator
	BindHost string
	ModelDir string

	// Secrets supplies credentials a container needs but a recipe must not
	// carry. Optional; nil means none are injected.
	//
	// NOT recipe.Env, deliberately: recipes are published to git, so a token
	// living there would be a credential in version control the first time
	// anyone regenerated the catalog.
	Secrets interface{ Env() []string }

	// DropCaches is injectable so tests do not need root and so the reason it
	// exists stays visible at the call site.
	DropCaches func() error

	// Probe decides whether a deployment is READY as opposed to merely up.
	// Optional: without it a running container is reported as starting
	// forever, which is wrong but never claims something is usable when it
	// is not.
	Probe *Prober

	mu sync.Mutex // serialises loads; see the package comment

	// stopSet holds undeploys that have been asked for but not finished. It is
	// the one piece of state with no evidence anywhere else - during a stop,
	// Docker still reports the container as running.
	stopMu  sync.Mutex
	stopSet map[string]time.Time
}

func (m *Manager) Deploy(ctx context.Context, id string, wantPort int, force bool) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	r, err := m.Catalog.Get(id)
	if err != nil {
		return Record{}, err
	}

	resident, err := m.residentExcept(id)
	if err != nil {
		return Record{}, err
	}
	plan := m.Planner.Plan(resident, capacity.Entry{ID: r.ID, GiB: m.footprint(r)})
	if !plan.Fits && !force {
		return Record{}, &CapacityError{RecipeID: id, Result: plan}
	}

	port := wantPort
	if port == 0 {
		if port, err = m.Ports.Free(m.BindHost); err != nil {
			return Record{}, err
		}
	} else if !m.Ports.IsFree(m.BindHost, port) {
		return Record{}, fmt.Errorf("deploy %s: port %d is already in use", id, port)
	}

	spec, err := engine.BuildSpec(r, port, m.ModelDir)
	if err != nil {
		return Record{}, err
	}

	// After BuildSpec rather than inside it: BuildSpec is a pure function of a
	// recipe, and it is what the export and diff paths render. Injecting here
	// keeps the secret out of anything that can be printed, compared or
	// committed.
	if m.Secrets != nil {
		spec.Env = append(spec.Env, m.Secrets.Env()...)
	}

	// A third-party image listens where its author chose. BuildSpec defaults to
	// 8000 because that is what Sous itself commands vLLM to use, but for
	// KindContainer nothing here controls the port - kokoro serves on 8880 -
	// and mapping the host port to the wrong container port produces a
	// container that starts, reports healthy, and answers nothing.
	//
	// Read it from the image rather than making the recipe declare it: a port
	// is a property of the image, and a recipe that restated it would go stale
	// the first time the image changed.
	if r.Kind == recipe.KindContainer {
		// The recipe wins when it says anything, because it is the only place
		// that can correct an image whose EXPOSE metadata disagrees with the
		// process inside it.
		if r.ContainerPort > 0 {
			spec.ContainerPort = r.ContainerPort
		} else if cp, err := m.Runtime.ImageExposedPort(ctx, r.Image); err == nil && cp > 0 {
			spec.ContainerPort = cp
		}
		// A failure here is not fatal: an image with no EXPOSE still has to be
		// deployable, and 8000 is as good a guess as any. The deployment is
		// visibly broken either way, which is better than refusing to start.
	}

	// Stop first. Always. A profile or a marker file cannot be relied on to
	// have stopped anything.
	var prev Record
	if err := m.Store.ReadYAML(store.KindDeployment, id, &prev); err == nil {
		if err := m.Runtime.Stop(ctx, spec.Name); err != nil {
			return Record{}, fmt.Errorf("deploy %s: stopping the previous container: %w", id, err)
		}
	}

	if m.DropCaches != nil {
		if err := m.DropCaches(); err != nil {
			return Record{}, fmt.Errorf("deploy %s: dropping page cache: %w", id, err)
		}
	}

	cid, err := m.Runtime.Start(ctx, spec)
	if err != nil {
		return Record{}, fmt.Errorf("deploy %s: %w", id, err)
	}

	rec := Record{RecipeID: id, HostPort: port, ContainerID: cid, StartedAt: time.Now()}
	if rc, err := m.Runtime.Logs(ctx, spec.Name); err == nil {
		rec.Observation = observe.ParseBootLog(id, rc)
		rc.Close()
		// Observations are node-local and never written back into the recipe.
		_ = m.Store.WriteYAML(store.KindObservation, id, rec.Observation)
	}
	if err := m.Store.WriteYAML(store.KindDeployment, id, rec); err != nil {
		return Record{}, err
	}
	return rec, nil
}

func (m *Manager) Undeploy(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !recipe.ValidID(id) {
		return fmt.Errorf("undeploy: invalid id %q", id)
	}
	if err := m.Runtime.Stop(ctx, engine.ContainerName(id)); err != nil {
		return err
	}
	// A MISSING RECORD IS SUCCESS. Callers of Undeploy are trying to reach a
	// state, not perform an event, and "already gone" is that state.
	//
	// Returning an error here took the fleet's chat model down. A trial window
	// stopped qwen36 to make room, failed for an unrelated reason, and its
	// cleanup path called Undeploy a second time - which answered 500 for a
	// record it had itself already deleted. The script read that as a failed
	// restore and gave up, leaving the model down.
	//
	// Deliberately NOT fixed in Store.Delete: recipe deletion needs a missing
	// file to stay an error, or deleting a mistyped id would report success.
	if err := m.Store.Delete(store.KindDeployment, id); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Plan answers "would this fit" without touching a container, so the UI can
// show a margin before anything irreversible happens.
func (m *Manager) Plan(id string) (capacity.Result, error) {
	r, err := m.Catalog.Get(id)
	if err != nil {
		return capacity.Result{}, err
	}
	resident, err := m.residentExcept(id)
	if err != nil {
		return capacity.Result{}, err
	}
	return m.Planner.Plan(resident, capacity.Entry{ID: r.ID, GiB: m.footprint(r)}), nil
}

func (m *Manager) List() ([]Record, error) {
	names, err := m.Store.List(store.KindDeployment)
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(names))
	for _, n := range names {
		var rec Record
		if err := m.Store.ReadYAML(store.KindDeployment, n, &rec); err != nil {
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

// footprint prefers measured truth over the author's estimate. This is the
// entire reason observations are written back after a load.
func (m *Manager) footprint(r recipe.Recipe) float64 {
	var o observe.Observation
	if err := m.Store.ReadYAML(store.KindObservation, r.ID, &o); err == nil {
		if total := o.WeightsGiB + o.KVGiB; total > 0 {
			return total
		}
	}
	return r.Declared.TotalGiB()
}

// residentExcept omits id, because a redeploy replaces itself rather than
// adding to the pool. Counting it twice would refuse every redeploy of a model
// large enough to matter.
func (m *Manager) residentExcept(id string) ([]capacity.Entry, error) {
	names, err := m.Store.List(store.KindDeployment)
	if err != nil {
		return nil, err
	}
	var out []capacity.Entry
	for _, n := range names {
		if n == id {
			continue
		}
		r, err := m.Catalog.Get(n)
		if err != nil {
			continue
		}
		out = append(out, capacity.Entry{ID: n, GiB: m.footprint(r)})
	}
	return out, nil
}
