package engine

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/codemug/sous/internal/recipe"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/go-connections/nat"
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
	// Labels carry facts a container NAME cannot, because names must be
	// lowercase and some identifiers are case-sensitive.
	Labels map[string]string
	// HostPort is the one host port Docker says this container publishes, or
	// 0 when it publishes none (or more than one - see hostPort). Docker, not
	// whichever process created the container, is where this lives: the
	// restart policy brings a model back on this port after a reboot whether
	// or not anything remembers giving it out, and a souslet restarted
	// alongside it has nowhere else to learn it from.
	HostPort int
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
			// JOBS ARE NOT DEPLOYMENTS. Docker's name filter is a substring
			// match and "sous-job-…" matches "sous-", so a downloader would
			// otherwise be listed here. Nothing reads this map by iteration
			// TODAY - callers look up an exact recipe name - but the day
			// something counts it, a download would be charged against the GPU
			// pool it never touches.
			if strings.HasPrefix(n, JobPrefix) {
				continue
			}
			st := ContainerState{Name: n, Status: c.State, Labels: c.Labels}
			// ContainerList does not carry exit code or restart count, so the
			// detail is fetched only for containers that are not plainly
			// running - which is the small minority, and never the steady state.
			// It does not carry a stopped container's ports either (those are
			// only listed while bound), so the same inspect supplies the
			// binding it was created with.
			var configured nat.PortMap
			if c.State != "running" {
				if ins, err := d.cli.ContainerInspect(ctx, c.ID); err == nil && ins.State != nil {
					st.ExitCode = ins.State.ExitCode
					st.Restarts = ins.RestartCount
					st.OOMKilled = ins.State.OOMKilled
					if ins.HostConfig != nil {
						configured = ins.HostConfig.PortBindings
					}
				}
			}
			st.HostPort = hostPort(c.Ports, configured)
			out[n] = st
		}
	}
	return out, nil
}

// hostPort reduces what Docker reports about one container's published ports
// to the single TCP host port Sous gave it: the LIVE binding when the
// container is up (listed), otherwise the binding it was CREATED with
// (configured) - a stopped or restarting container is listed with no ports at
// all, yet Docker will start it on exactly that one again.
//
// Zero means "unknown", never a guess: nothing published; a binding of "0" or
// "" on a container that is not up (Docker picks a fresh ephemeral port on
// every start, so the last one says nothing about the next - which is how
// every drag-and-drop souslet deploy was created before souslet allocated
// ports itself); or MORE THAN ONE distinct port, which
// toDockerConfig never creates, so it is not a container whose port Sous can
// know. The IPv4 and IPv6 entries of one 0.0.0.0 binding are one port, not
// two.
func hostPort(listed []container.Port, configured nat.PortMap) int {
	var found []int
	for _, p := range listed {
		if p.Type == "tcp" && p.PublicPort != 0 {
			found = append(found, int(p.PublicPort))
		}
	}
	if len(found) == 0 {
		for cp, bindings := range configured {
			if cp.Proto() != "tcp" {
				continue
			}
			for _, b := range bindings {
				if n, err := strconv.Atoi(b.HostPort); err == nil && n > 0 {
					found = append(found, n)
				}
			}
		}
	}
	slices.Sort(found)
	if found = slices.Compact(found); len(found) != 1 {
		return 0
	}
	return found[0]
}

// DeclaredFootprint reads back the footprint BuildSpec labelled the container
// with. ok is false for a container without BOTH labels - one created before
// they existed - or with one that does not parse: its footprint is UNKNOWN,
// which a caller must keep distinct from a declared zero rather than fill in.
func (c ContainerState) DeclaredFootprint() (f recipe.Footprint, ok bool) {
	w, werr := strconv.ParseFloat(c.Labels[WeightsLabel], 64)
	kv, kverr := strconv.ParseFloat(c.Labels[KVLabel], 64)
	if werr != nil || kverr != nil {
		return recipe.Footprint{}, false
	}
	return recipe.Footprint{WeightsGiB: w, KVGiB: kv}, true
}
