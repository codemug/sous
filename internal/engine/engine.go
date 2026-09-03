package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
)

// Docker implements deploy.Runtime against the Engine API.
type Docker struct {
	cli      *client.Client
	bindHost string
	// gpuDriver selects how a GPU Spec requests a device - "cdi" (the zero
	// value, and the only thing this package supported before this field
	// existed) or "nvidia". Not every node exposes the GPU the same way:
	// GB10 (asus-gx10) has no nvidia runtime at all, only CDI, while a
	// standard NVIDIA Container Toolkit install (aorus-ubuntu) uses the
	// nvidia runtime and has no CDI spec registered. Neither request style
	// works on the other node.
	gpuDriver string
}

func New(bindHost, gpuDriver string) (*Docker, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &Docker{cli: cli, bindHost: bindHost, gpuDriver: gpuDriver}, nil
}

func toDockerConfig(s Spec, bindHost, gpuDriver string) (*container.Config, *container.HostConfig) {
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
		if gpuDriver == "nvidia" {
			// The standard NVIDIA Container Toolkit path. `docker run
			// --gpus all` itself leaves Driver empty and lets the daemon
			// resolve it by capability; naming "nvidia" explicitly here
			// resolves to the exact same registered driver on any host
			// where the toolkit is installed (moby registers it under
			// that literal name - see daemon/devices_nvidia_linux.go),
			// just without relying on there being only one GPU driver
			// registered to default to. Count -1 and the gpu capability
			// match the CLI's own translation exactly either way.
			host.DeviceRequests = []container.DeviceRequest{{
				Driver:       "nvidia",
				Count:        -1,
				Capabilities: [][]string{{"gpu"}},
			}}
		} else {
			// CDI, not the nvidia runtime. `docker info` on GB10 lists only
			// runc, so every --gpus recipe fails there. capabilities is
			// required alongside the driver or the request is rejected.
			// This is also the default (gpuDriver == "") for backward
			// compatibility with every node running this before the field
			// existed.
			host.DeviceRequests = []container.DeviceRequest{{
				Driver:       "cdi",
				DeviceIDs:    []string{"nvidia.com/gpu=all"},
				Capabilities: [][]string{{"gpu"}},
			}}
		}
		// vLLM leans on shared memory for worker IPC and the default 64 MB is
		// not enough; the failure is an opaque worker crash.
		host.IpcMode = container.IPCModeHost
	}
	return cfg, host
}

func (d *Docker) Start(ctx context.Context, s Spec) (string, error) {
	if err := d.ensureImage(ctx, s.Image); err != nil {
		return "", err
	}
	cfg, host := toDockerConfig(s, d.bindHost, d.gpuDriver)
	created, err := d.cli.ContainerCreate(ctx, cfg, host, nil, nil, s.Name)
	if err != nil {
		return "", err
	}
	if err := d.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return "", err
	}
	return created.ID, nil
}

// ensureImage pulls ref if it is not already present locally. The Engine
// API's ContainerCreate, unlike `docker run`, never pulls a missing image
// itself - it fails outright with "No such image". This went unnoticed
// through every deploy this project has ever made, because every node
// Sous had run on already had its images cached (from the single-node
// Sous era, or from this fleet's own precedent of pre-pulling vLLM images
// by hand). It surfaced for real on aorus-ubuntu's first-ever deployment:
// a genuinely cold Docker install, with nothing cached at all.
//
// Checks local presence FIRST rather than pulling unconditionally on
// every call - matching `docker run`'s own default ("pull if missing",
// not "pull always") and avoiding a needless registry round-trip on the
// overwhelmingly common case where the image core recipes (which pin
// digests, not floating tags) already have cached.
func (d *Docker) ensureImage(ctx context.Context, ref string) error {
	if _, err := d.cli.ImageInspect(ctx, ref); err == nil {
		return nil
	} else if !client.IsErrNotFound(err) {
		return fmt.Errorf("engine: inspect image %s: %w", ref, err)
	}

	rc, err := d.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("engine: pull image %s: %w", ref, err)
	}
	defer rc.Close()

	if err := drainPullStream(rc); err != nil {
		return fmt.Errorf("engine: pull image %s: %w", ref, err)
	}
	return nil
}

// drainPullStream reads an ImagePull response to completion, which is
// required for the pull to actually finish (it is not synchronous until
// the stream is read) - and, unlike a plain io.Copy(io.Discard, r), also
// catches a registry-side failure (auth, missing manifest, ...), which
// Docker reports as an "error" field INSIDE this JSON stream rather than
// as a Go error from ImagePull itself. Split out from ensureImage so the
// decode/error-detection logic - the actual subtle part - is testable
// against a crafted byte stream, with no real Docker daemon or network
// access required.
func drainPullStream(r io.Reader) error {
	dec := json.NewDecoder(r)
	for {
		var msg struct {
			Error string `json:"error"`
		}
		if err := dec.Decode(&msg); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("reading progress: %w", err)
		}
		if msg.Error != "" {
			return fmt.Errorf("%s", msg.Error)
		}
	}
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

// Logs returns a container's output as PLAIN TEXT.
//
// Docker's log stream for a non-TTY container is MULTIPLEXED: every chunk is
// prefixed with an 8-byte header carrying the stream id and the frame length.
// Handing that to a caller unchanged puts raw binary through whatever consumes
// it - in a web page it produces invalid UTF-8 and a corrupted document, which
// is how this surfaced: the logs panel rendered nothing and the page around it
// broke, with no error anywhere to explain why.
//
// stdcopy.StdCopy is the demultiplexer. Both streams are folded into one
// destination because interleaved order is what a person reads a log for -
// separating them would put the traceback in a different pane from the line
// that caused it.
func (d *Docker) Logs(ctx context.Context, name string) (io.ReadCloser, error) {
	raw, err := d.cli.ContainerLogs(ctx, name, container.LogsOptions{
		ShowStdout: true, ShowStderr: true,
	})
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		defer raw.Close()
		_, err := stdcopy.StdCopy(pw, pw, raw)
		// CloseWithError(nil) is equivalent to Close, so a clean EOF stays a
		// clean EOF for the reader.
		_ = pw.CloseWithError(err)
	}()
	return pr, nil
}

func (d *Docker) Running(ctx context.Context) ([]string, error) {
	list, err := d.cli.ContainerList(ctx, container.ListOptions{
		Filters: filters.NewArgs(filters.Arg("name", namePrefix)),
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list))
	for _, c := range list {
		for _, n := range c.Names {
			n = strings.TrimPrefix(n, "/")
			// Same reason as States: the name filter is a substring match, so
			// job containers match the deployment prefix too.
			if strings.HasPrefix(n, JobPrefix) {
				continue
			}
			out = append(out, n)
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
