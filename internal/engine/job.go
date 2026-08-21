package engine

import (
	"context"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

// JobPrefix marks a container that does work and exits, as opposed to a model
// that serves. Kept distinct from the deployment prefix so a job can never be
// mistaken for a deployment by anything that lists containers - including the
// capacity planner, which would otherwise count a downloader's memory against
// the GPU pool it does not use.
const JobPrefix = "sous-job-"

// JobName builds the container name for a job.
func JobName(id string) string { return JobPrefix + id }

// JobSpec is a container that runs to completion.
//
// SEPARATE FROM Spec ON PURPOSE, and not a flag on it. A deployment binds a
// port and carries RestartPolicy "unless-stopped" so models survive a reboot.
// Both are wrong here and the second is actively dangerous: a download that
// finishes successfully and exits 0 would be restarted by that policy and
// download again, forever.
type JobSpec struct {
	Name  string
	Image string
	// Entrypoint MUST usually be set. A job runs in an image built to serve
	// something - the vLLM image's entrypoint is the API server - so leaving it
	// alone makes Cmd arrive as ARGUMENTS to that server rather than as the
	// command to run. The observed failure was "Failed to infer device type":
	// vLLM starting up on a container that was only ever meant to download a
	// file.
	Entrypoint []string
	Cmd        []string
	Env        []string
	Binds      []string
	// Labels carry facts the container name cannot. A Docker name must be
	// lowercase, so anything case-sensitive - a HuggingFace repo id, which
	// Qwen/Qwen3.6-35B-A3B-FP8 very much is - cannot survive a round trip
	// through it. Reconstructing an id from a name produces something that
	// looks right and 404s.
	Labels map[string]string
}

// StartJob creates and starts a one-shot container, returning its id.
//
// It does NOT wait. A model download is tens of gigabytes and takes twenty
// minutes; holding a request open for that is the same mistake undeploy used to
// make. Progress is read afterwards from the container's own state and logs.
func (d *Docker) StartJob(ctx context.Context, s JobSpec) (string, error) {
	cfg := &container.Config{
		Image:  s.Image,
		Cmd:    s.Cmd,
		Env:    s.Env,
		Labels: s.Labels,
	}
	if len(s.Entrypoint) > 0 {
		cfg.Entrypoint = s.Entrypoint
	}
	host := &container.HostConfig{
		Binds: s.Binds,
		// NO RestartPolicy. The zero value is "no", which is the only correct
		// answer for a job: success means exit 0, and restarting on success
		// would loop forever.
		//
		// NO port binding either - a job serves nothing, and binding one would
		// consume a port from the range deployments allocate from.
	}
	created, err := d.cli.ContainerCreate(ctx, cfg, host, nil, nil, s.Name)
	if err != nil {
		return "", err
	}
	if err := d.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return "", err
	}
	return created.ID, nil
}

// JobStates returns every job container, keyed by name.
//
// Jobs are listed with All so a finished one is still visible: the exit code is
// the result, and a job that vanished the moment it succeeded would leave no way
// to tell success from never-started.
func (d *Docker) JobStates(ctx context.Context) (map[string]ContainerState, error) {
	list, err := d.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("name", JobPrefix)),
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]ContainerState, len(list))
	for _, c := range list {
		for _, n := range c.Names {
			n = strings.TrimPrefix(n, "/")
			if !strings.HasPrefix(n, JobPrefix) {
				continue
			}
			st := ContainerState{Name: n, Status: c.State, Labels: c.Labels}
			if ins, err := d.cli.ContainerInspect(ctx, c.ID); err == nil && ins.State != nil {
				st.ExitCode = ins.State.ExitCode
				st.Restarts = ins.RestartCount
				st.OOMKilled = ins.State.OOMKilled
			}
			out[n] = st
		}
	}
	return out, nil
}

// RemoveJob deletes a job container, whether or not it is still running.
func (d *Docker) RemoveJob(ctx context.Context, name string) error {
	err := d.cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true})
	if err != nil && !client.IsErrNotFound(err) {
		return err
	}
	return nil
}
