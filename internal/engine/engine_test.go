package engine

import (
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
