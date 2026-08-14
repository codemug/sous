package engine

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

// Docker implements deploy.Runtime against the Engine API.
type Docker struct {
	cli      *client.Client
	bindHost string
}

func New(bindHost string) (*Docker, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &Docker{cli: cli, bindHost: bindHost}, nil
}

func toDockerConfig(s Spec, bindHost string) (*container.Config, *container.HostConfig) {
	cp := nat.Port(fmt.Sprintf("%d/tcp", s.ContainerPort))

	cfg := &container.Config{
		Image:        s.Image,
		Cmd:          s.Cmd,
		Env:          s.Env,
		ExposedPorts: nat.PortSet{cp: struct{}{}},
	}
	if len(s.Entrypoint) > 0 {
		cfg.Entrypoint = s.Entrypoint
	}

	host := &container.HostConfig{
		PortBindings: nat.PortMap{cp: []nat.PortBinding{
			// The tailnet IP, never 0.0.0.0. This is the network boundary the
			// security posture rests on.
			{HostIP: bindHost, HostPort: strconv.Itoa(s.HostPort)},
		}},
		Binds: s.Binds,
		// Deployments must survive a node reboot without Sous being up: models
		// running is the safe state, and Sous only decides transitions.
		RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
	}

	if s.GPU {
		// CDI, not the nvidia runtime. `docker info` on this box lists only
		// runc, so every --gpus recipe fails here. capabilities is required
		// alongside the driver or the request is rejected.
		host.DeviceRequests = []container.DeviceRequest{{
			Driver:       "cdi",
			DeviceIDs:    []string{"nvidia.com/gpu=all"},
			Capabilities: [][]string{{"gpu"}},
		}}
		// vLLM leans on shared memory for worker IPC and the default 64 MB is
		// not enough; the failure is an opaque worker crash.
		host.IpcMode = container.IPCModeHost
	}
	return cfg, host
}

func (d *Docker) Start(ctx context.Context, s Spec) (string, error) {
	cfg, host := toDockerConfig(s, d.bindHost)
	created, err := d.cli.ContainerCreate(ctx, cfg, host, nil, nil, s.Name)
	if err != nil {
		return "", err
	}
	if err := d.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return "", err
	}
	return created.ID, nil
}

// Stop stops and removes. Leaving a stopped container behind would make the
// next create fail on the name, which is the kind of failure that gets
// misread as a problem with the new model.
func (d *Docker) Stop(ctx context.Context, name string) error {
	timeout := 60 // vLLM needs a moment to release the pool
	if err := d.cli.ContainerStop(ctx, name, container.StopOptions{Timeout: &timeout}); err != nil {
		if !client.IsErrNotFound(err) {
			return err
		}
	}
	if err := d.cli.ContainerRemove(ctx, name, container.RemoveOptions{}); err != nil {
		if !client.IsErrNotFound(err) {
			return err
		}
	}
	return nil
}

func (d *Docker) Logs(ctx context.Context, name string) (io.ReadCloser, error) {
	return d.cli.ContainerLogs(ctx, name, container.LogsOptions{
		ShowStdout: true, ShowStderr: true,
	})
}

func (d *Docker) Running(ctx context.Context) ([]string, error) {
	list, err := d.cli.ContainerList(ctx, container.ListOptions{
		Filters: filters.NewArgs(filters.Arg("name", "sous-")),
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list))
	for _, c := range list {
		for _, n := range c.Names {
			out = append(out, strings.TrimPrefix(n, "/"))
		}
	}
	return out, nil
}

// ImageExposedPort reads the container port from image metadata, which is how
// a third-party image avoids having to declare a port Sous does not control.
func (d *Docker) ImageExposedPort(ctx context.Context, ref string) (int, error) {
	insp, err := d.cli.ImageInspect(ctx, ref)
	if err != nil {
		return 0, err
	}
	// Image inspect returns these as plain strings ("8880/tcp"), unlike the
	// container config's nat.PortSet, so convert before reading the number.
	for p := range insp.Config.ExposedPorts {
		if n := nat.Port(p).Int(); n > 0 {
			return n, nil
		}
	}
	return 0, fmt.Errorf("engine: image %s exposes no port", ref)
}
