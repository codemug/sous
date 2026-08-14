// Package engine translates a recipe into a container specification and drives
// the Docker Engine API.
//
// Sous never shells out to `docker` or `docker compose`. Every Compose trap
// this fleet hit is a property of Compose semantics rather than of containers:
// a service archived behind `profiles:` keeps running because --remove-orphans
// only removes services absent from the file; teardown fails when the file
// uses ${VAR:?} guards because compose must parse it to stop anything; and
// `up -d` will not rebuild after a Dockerfile change because it checks tag
// presence. Talking to the Engine API directly means none of these exist.
package engine

import (
	"fmt"
	"sort"

	"github.com/codemug/sous/internal/recipe"
)

// containerPort is the port Sous makes services listen on INSIDE the
// container. For kinds Sous generates a command for it is injected; for
// KindContainer the real value is read from image metadata at deploy time.
// It is deliberately not a recipe field: nothing about ports is authored.
const containerPort = 8000

type Spec struct {
	Name          string
	Image         string
	Entrypoint    []string
	Cmd           []string
	Env           []string
	ContainerPort int
	HostPort      int
	Binds         []string
	GPU           bool
}

func BuildSpec(r recipe.Recipe, hostPort int, modelDir string) (Spec, error) {
	if err := r.Validate(); err != nil {
		return Spec{}, err
	}
	s := Spec{
		Name:          "sous-" + r.ID,
		Image:         r.Image,
		HostPort:      hostPort,
		ContainerPort: containerPort,
		Binds:         []string{modelDir + ":/root/.cache/huggingface"},
		// A recipe declaring zero weights is CPU-only and must not request a
		// GPU. Kokoro is the live example, and that is a design choice: TTS on
		// the CPU leaves the memory bandwidth for the LLM, which is what
		// decode is actually bound by.
		GPU: r.Declared.WeightsGiB > 0,
	}
	for k, v := range r.Env {
		s.Env = append(s.Env, fmt.Sprintf("%s=%s", k, v))
	}
	sort.Strings(s.Env)

	switch r.Kind {
	case recipe.KindVLLM:
		s.GPU = true // a vLLM recipe without weights is a mistake, not a CPU service
		s.Cmd = append(s.Cmd, "--model="+r.Model)
		if len(r.ServedAs) > 0 {
			s.Cmd = append(s.Cmd, "--served-model-name")
			s.Cmd = append(s.Cmd, r.ServedAs...)
		}
		s.Cmd = append(s.Cmd, renderArgs(r.Args)...)
		s.Cmd = append(s.Cmd, fmt.Sprintf("--port=%d", containerPort))
	case recipe.KindTransformers:
		// Explicit entrypoint, never Cmd: an inherited ENTRYPOINT would turn
		// this into arguments for the base image's binary.
		s.Entrypoint = append([]string(nil), r.Entrypoint...)
	case recipe.KindContainer:
		// Nothing synthesised. The image is used exactly as published, because
		// Sous cannot reason about the internals of an image it did not build.
	}
	return s, nil
}

// renderArgs produces deterministic flags. Go map iteration is randomised, and
// a command that differs between builds makes container recreation
// non-reproducible and diffs meaningless.
//
// A bare `true` becomes a valueless flag: vLLM rejects
// --enable-prefix-caching=true.
func renderArgs(args map[string]any) []string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]string, 0, len(keys))
	for _, k := range keys {
		switch v := args[k].(type) {
		case bool:
			if v {
				out = append(out, "--"+k)
			}
		default:
			out = append(out, fmt.Sprintf("--%s=%v", k, v))
		}
	}
	return out
}
