package engine

import (
	"context"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
)

// ContainerState is what the runtime knows about one container.
//
// WHY THIS EXISTS RATHER THAN A LIST OF NAMES. Running() answers "is there a
// container with this name", which cannot tell a model that is still loading
// apart from one that is serving, nor a container that exited cleanly apart
// from one that is crash-looping. Those are four different things an operator
// needs to act on differently, and all four looked identical on the dashboard.
type ContainerState struct {
	Name string
	// Status is Docker's own word: created, running, restarting, exited,
	// paused, dead. Kept verbatim rather than mapped, because the mapping
	// belongs to whoever is deciding what to show.
	Status string
	// ExitCode is meaningful only once Status is "exited". Zero there means a
	// deliberate stop; anything else means it died.
	ExitCode int
	// Restarts is how many times Docker has restarted it. A container that is
	// "running" with a climbing restart count is crash-looping, which reads as
	// healthy to anything that only checks whether it exists.
	Restarts int
	// OOMKilled is called out separately because on this node it is the most
	// likely way a model dies, and the exit code alone does not say so.
	OOMKilled bool
}

// Running reports true for a container Docker considers up. It says nothing
// about whether the process inside is ready to serve.
func (c ContainerState) Running() bool { return c.Status == "running" }

// Crashed reports a container that stopped in a way nobody asked for.
func (c ContainerState) Crashed() bool {
	if c.OOMKilled {
		return true
	}
	if c.Status == "exited" && c.ExitCode != 0 {
		return true
	}
	// A container Docker is restarting has already failed at least once; the
	// restart policy is hiding it.
	return c.Status == "restarting" || c.Status == "dead"
}

// States returns every Sous-managed container, keyed by name.
//
// ONE CALL, NOT ONE PER MODEL. A dashboard refresh that inspects each
// deployment separately turns into N Docker round trips, and the list endpoint
// already carries everything needed.
func (d *Docker) States(ctx context.Context) (map[string]ContainerState, error) {
	list, err := d.cli.ContainerList(ctx, container.ListOptions{
		All: true, // stopped and crashed containers are the interesting ones
		Filters: filters.NewArgs(
			filters.Arg("name", namePrefix),
		),
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]ContainerState, len(list))
	for _, c := range list {
		for _, n := range c.Names {
			// Docker returns names with a leading slash.
			n = strings.TrimPrefix(n, "/")
			if !strings.HasPrefix(n, namePrefix) {
				continue
			}
			st := ContainerState{Name: n, Status: c.State}
			// ContainerList does not carry exit code or restart count, so the
			// detail is fetched only for containers that are not plainly
			// running - which is the small minority, and never the steady state.
			if c.State != "running" {
				if ins, err := d.cli.ContainerInspect(ctx, c.ID); err == nil && ins.State != nil {
					st.ExitCode = ins.State.ExitCode
					st.Restarts = ins.RestartCount
					st.OOMKilled = ins.State.OOMKilled
				}
			}
			out[n] = st
		}
	}
	return out, nil
}
