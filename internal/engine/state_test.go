package engine

import (
	"strings"
	"testing"

	"github.com/codemug/sous/internal/recipe"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
)

// Docker's name filter is a SUBSTRING match, so "sous-job-qwen" matches the
// deployment prefix "sous-". Nothing iterates the deployment map today, but the
// day something counts it, a downloader would be charged against the GPU pool
// it never touches - and it would look like a model nobody deployed.
func TestJobNamesAreNotDeploymentNames(t *testing.T) {
	job := JobName("qwen--qwen3.6")
	if !strings.HasPrefix(job, namePrefix) {
		t.Fatalf("premise wrong: %q no longer shares the deployment prefix", job)
	}
	if !strings.HasPrefix(job, JobPrefix) {
		t.Fatalf("job name %q lacks the job prefix", job)
	}
	// A deployment must never be mistaken for a job either.
	if strings.HasPrefix(ContainerName("qwen36"), JobPrefix) {
		t.Fatal("a deployment name matches the job prefix")
	}
}

// A souslet restarted after a reboot has nothing but Docker to learn a model's
// port from, so this is what turns Docker's two shapes for "what does this
// container publish" into the one port Sous gave it - or into an honest 0.
func TestHostPortIsTheOnePortDockerSaysTheContainerPublishes(t *testing.T) {
	bound := func(hostPorts ...string) nat.PortMap {
		bs := make([]nat.PortBinding, 0, len(hostPorts))
		for _, p := range hostPorts {
			bs = append(bs, nat.PortBinding{HostIP: "127.0.0.1", HostPort: p})
		}
		return nat.PortMap{"8000/tcp": bs}
	}
	for _, tc := range []struct {
		name       string
		listed     []container.Port
		configured nat.PortMap
		want       int
	}{
		{name: "running: the live binding",
			listed: []container.Port{{IP: "127.0.0.1", PrivatePort: 8000, PublicPort: 18003, Type: "tcp"}},
			want:   18003},
		{name: "running on 0.0.0.0: the IPv4 and IPv6 entries are one port",
			listed: []container.Port{
				{IP: "0.0.0.0", PrivatePort: 8000, PublicPort: 18003, Type: "tcp"},
				{IP: "::", PrivatePort: 8000, PublicPort: 18003, Type: "tcp"},
			},
			want: 18003},
		{name: "the live binding wins over the configured one",
			listed:     []container.Port{{PrivatePort: 8000, PublicPort: 18003, Type: "tcp"}},
			configured: bound("18009"),
			want:       18003},
		{name: "stopped or restarting: nothing listed, so the binding it was created with",
			configured: bound("18003"),
			want:       18003},
		{name: "exposed but not published",
			listed: []container.Port{{PrivatePort: 8000, Type: "tcp"}},
			want:   0},
		{name: "publishes nothing at all", want: 0},
		{name: "created with host port 0 and not running: Docker picks a new one each start, so unknown",
			configured: bound("0"),
			want:       0},
		{name: "created with no host port", configured: bound(""), want: 0},
		{name: "two different ports: not a container Sous made, so no guess",
			listed: []container.Port{
				{PrivatePort: 8000, PublicPort: 18003, Type: "tcp"},
				{PrivatePort: 8001, PublicPort: 18004, Type: "tcp"},
			},
			want: 0},
		{name: "udp is not something a proxied HTTP request can reach",
			listed: []container.Port{{PrivatePort: 8000, PublicPort: 18003, Type: "udp"}},
			want:   0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostPort(tc.listed, tc.configured); got != tc.want {
				t.Fatalf("hostPort = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestDeclaredFootprintReadsBackWhatBuildSpecLabelled(t *testing.T) {
	r := recipe.Recipe{
		ID: "qwen38", Kind: recipe.KindVLLM, Modality: recipe.ModalityText,
		Model: "Inferact/Qwen3.8-27B-NVFP4", Image: "vllm/vllm-openai@sha256:abc",
		Declared: recipe.Footprint{WeightsGiB: 24.87, KVGiB: 6.5},
	}
	s, err := BuildSpec(r, 18003, "/models")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := ContainerState{Labels: s.Labels}.DeclaredFootprint()
	if !ok || got != r.Declared {
		t.Fatalf("DeclaredFootprint = (%+v, %v), want (%+v, true)", got, ok, r.Declared)
	}
}

// A container created before the labels existed, or with one that does not
// parse, has an UNKNOWN footprint - not a zero one, and not a guess.
func TestDeclaredFootprintIsUnknownWithoutBothLabels(t *testing.T) {
	for name, labels := range map[string]map[string]string{
		"no labels at all (an older souslet's container)": nil,
		"weights only": {WeightsLabel: "24.87"},
		"kv only":      {KVLabel: "6.5"},
		"garbage":      {WeightsLabel: "lots", KVLabel: "6.5"},
	} {
		if f, ok := (ContainerState{Labels: labels}).DeclaredFootprint(); ok {
			t.Errorf("%s: DeclaredFootprint = (%+v, true), want unknown", name, f)
		}
	}
}
