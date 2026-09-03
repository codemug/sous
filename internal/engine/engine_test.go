package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/docker/go-connections/nat"
)

func TestPortBindingUsesHostPortAndContainerPort(t *testing.T) {
	s := Spec{Name: "sous-x", Image: "img", ContainerPort: 8000, HostPort: 18001}
	_, hostCfg := toDockerConfig(s, "100.119.51.26", "cdi")
	key := nat.Port("8000/tcp")
	binds, ok := hostCfg.PortBindings[key]
	if !ok || len(binds) != 1 {
		t.Fatalf("no binding for %s: %+v", key, hostCfg.PortBindings)
	}
	if binds[0].HostPort != "18001" {
		t.Fatalf("host port: want 18001, got %s", binds[0].HostPort)
	}
	// Binding the tailnet IP rather than 0.0.0.0 is the network boundary that
	// the security posture rests on.
	if binds[0].HostIP != "100.119.51.26" {
		t.Fatalf("bind IP: want the tailnet IP, got %q", binds[0].HostIP)
	}
}

// GB10 exposes the GPU through CDI, not the nvidia runtime: `docker info`
// lists only runc on this box, and every --gpus recipe found online fails here.
func TestGPUUsesCDIDeviceRequest(t *testing.T) {
	_, hostCfg := toDockerConfig(
		Spec{Name: "n", Image: "i", GPU: true, ContainerPort: 8000}, "127.0.0.1", "cdi")
	if len(hostCfg.DeviceRequests) == 0 {
		t.Fatal("GPU spec produced no device request")
	}
	req := hostCfg.DeviceRequests[0]
	if req.Driver != "cdi" {
		t.Fatalf("want the cdi driver, got %q", req.Driver)
	}
	found := false
	for _, d := range req.DeviceIDs {
		if d == "nvidia.com/gpu=all" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want CDI device id, got %+v", req)
	}
}

// GB10 has no nvidia runtime; a standard NVIDIA Container Toolkit install
// (aorus-ubuntu) has no CDI spec registered - each node needs the request
// style the OTHER one would reject.
func TestGPUUsesNvidiaDeviceRequest(t *testing.T) {
	_, hostCfg := toDockerConfig(
		Spec{Name: "n", Image: "i", GPU: true, ContainerPort: 8000}, "127.0.0.1", "nvidia")
	if len(hostCfg.DeviceRequests) == 0 {
		t.Fatal("GPU spec produced no device request")
	}
	req := hostCfg.DeviceRequests[0]
	if req.Driver != "nvidia" {
		t.Fatalf("want the nvidia driver, got %q", req.Driver)
	}
	if req.Count != -1 {
		t.Fatalf("want Count -1 (all), got %d", req.Count)
	}
	if len(req.DeviceIDs) != 0 {
		t.Fatalf("nvidia driver requests must not carry CDI-style DeviceIDs, got %+v", req.DeviceIDs)
	}
}

func TestEmptyGPUDriverDefaultsToCDI(t *testing.T) {
	// Every caller before this field existed got CDI unconditionally -
	// engine.New("", "") must keep behaving exactly like that, not silently
	// request no device at all.
	_, hostCfg := toDockerConfig(
		Spec{Name: "n", Image: "i", GPU: true, ContainerPort: 8000}, "127.0.0.1", "")
	if len(hostCfg.DeviceRequests) == 0 || hostCfg.DeviceRequests[0].Driver != "cdi" {
		t.Fatalf("empty gpuDriver must default to cdi, got %+v", hostCfg.DeviceRequests)
	}
}

func TestCPUOnlySpecRequestsNoDevices(t *testing.T) {
	_, hostCfg := toDockerConfig(
		Spec{Name: "n", Image: "i", GPU: false, ContainerPort: 8000}, "127.0.0.1", "cdi")
	if len(hostCfg.DeviceRequests) != 0 {
		t.Fatal("CPU-only spec must not request a GPU")
	}
	if string(hostCfg.IpcMode) == "host" {
		t.Fatal("CPU-only spec should not need host IPC")
	}
}

func TestIPCHostIsSetForGPUWorkloads(t *testing.T) {
	// vLLM uses shared memory heavily for worker IPC; the default 64 MB shm
	// is not enough and the failure is an opaque worker crash.
	_, hostCfg := toDockerConfig(
		Spec{Name: "n", Image: "i", GPU: true, ContainerPort: 8000}, "127.0.0.1", "cdi")
	if string(hostCfg.IpcMode) != "host" {
		t.Fatalf("want ipc host, got %q", hostCfg.IpcMode)
	}
}

func TestEntrypointOverrideReachesTheConfig(t *testing.T) {
	s := Spec{Name: "n", Image: "i", ContainerPort: 8000,
		Entrypoint: []string{"python3", "-m", "uvicorn", "app:app"}}
	cfg, _ := toDockerConfig(s, "127.0.0.1", "cdi")
	if len(cfg.Entrypoint) != 4 || cfg.Entrypoint[0] != "python3" {
		t.Fatalf("entrypoint lost: %v", cfg.Entrypoint)
	}
}

func TestNoEntrypointLeavesImageDefault(t *testing.T) {
	cfg, _ := toDockerConfig(Spec{Name: "n", Image: "i", ContainerPort: 8000}, "127.0.0.1", "cdi")
	if len(cfg.Entrypoint) != 0 {
		t.Fatalf("must not invent an entrypoint: %v", cfg.Entrypoint)
	}
}

// A real docker pull's progress stream, one JSON object per line, no
// embedded error - captured in shape from an actual `docker pull` (status,
// progressDetail, id fields), not invented.
const realPullStreamNoError = `{"status":"Pulling from library/alpine","id":"3.21"}
{"status":"Pulling fs layer","progressDetail":{},"id":"9b18e9b68314"}
{"status":"Downloading","progressDetail":{"current":1024,"total":3072},"progress":"[====>   ]  1024B/3072B","id":"9b18e9b68314"}
{"status":"Download complete","progressDetail":{},"id":"9b18e9b68314"}
{"status":"Pull complete","progressDetail":{},"id":"9b18e9b68314"}
{"status":"Digest: sha256:abc123"}
{"status":"Status: Downloaded newer image for alpine:3.21"}
`

func TestDrainPullStreamSucceedsOnARealNoErrorStream(t *testing.T) {
	if err := drainPullStream(strings.NewReader(realPullStreamNoError)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDrainPullStreamCatchesAnErrorEmbeddedMidStream(t *testing.T) {
	// The exact bug ensureImage exists to avoid: a registry-side failure
	// (bad ref, auth, ...) arrives as an "error" field INSIDE the JSON
	// stream, after several genuine progress lines - not as a Go error
	// from ImagePull itself. A plain io.Copy(io.Discard, r) would drain
	// this to EOF and report success.
	stream := `{"status":"Pulling from library/alpine","id":"3.21"}
{"status":"Pulling fs layer","progressDetail":{},"id":"9b18e9b68314"}
{"errorDetail":{"message":"manifest unknown: manifest unknown"},"error":"manifest unknown: manifest unknown"}
`
	err := drainPullStream(strings.NewReader(stream))
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "manifest unknown") {
		t.Fatalf("error should surface the registry's own message, got: %v", err)
	}
}

func TestDrainPullStreamHandlesAnEmptyStream(t *testing.T) {
	if err := drainPullStream(strings.NewReader("")); err != nil {
		t.Fatalf("unexpected error on an empty stream: %v", err)
	}
}

func TestDrainPullStreamSurfacesMalformedJSON(t *testing.T) {
	err := drainPullStream(strings.NewReader("{not json"))
	if err == nil {
		t.Fatal("expected an error for malformed JSON, got nil")
	}
}

// TestEnsureImageSkipsAnAlreadyCachedImage is a real integration test
// against an actual Docker daemon, not a fake - ensureImage's whole
// premise (check ImageInspect before ever calling ImagePull) isn't
// meaningfully testable through drainPullStream alone, since that
// covers only what happens once a pull stream exists. Skips cleanly if
// no daemon is reachable or the fixture image isn't cached, rather than
// failing a CI environment without Docker access - matching this
// project's existing pattern of degrading gracefully for environment
// limits (e.g. -race being unavailable in sandboxes without a C
// toolchain) rather than papering over the gap with a fake.
func TestEnsureImageSkipsAnAlreadyCachedImage(t *testing.T) {
	d, err := New("", "cdi")
	if err != nil {
		t.Skipf("no local Docker daemon reachable: %v", err)
	}
	const fixtureImage = "alpine:3.21"
	if _, err := d.cli.ImageInspect(context.Background(), fixtureImage); err != nil {
		t.Skipf("fixture image %s not already cached locally: %v", fixtureImage, err)
	}
	if err := d.ensureImage(context.Background(), fixtureImage); err != nil {
		t.Fatalf("ensureImage on an already-cached image should not error: %v", err)
	}
}

func TestBindsAndRestartPolicy(t *testing.T) {
	s := Spec{Name: "n", Image: "i", ContainerPort: 8000,
		Binds: []string{"/models:/root/.cache/huggingface"}}
	_, hostCfg := toDockerConfig(s, "127.0.0.1", "cdi")
	if len(hostCfg.Binds) != 1 {
		t.Fatalf("binds lost: %v", hostCfg.Binds)
	}
	// Containers must come back after a node reboot without Sous being up.
	if hostCfg.RestartPolicy.Name != "unless-stopped" {
		t.Fatalf("want unless-stopped, got %q", hostCfg.RestartPolicy.Name)
	}
}
