package recipe

import (
	"fmt"
	"regexp"
)

// idRE also constrains what can become a container name and a filename, which
// is why it is this narrow.
var idRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

func ValidID(s string) bool { return idRE.MatchString(s) }

// portArgs are refused outright. A recipe describes a model; where it lands is
// a placement decision made at deploy time against the actual host, so that a
// port conflict is reported by the planner rather than discovered as a failed
// bind after a six-minute load.
var portArgs = map[string]bool{"port": true, "host-port": true, "hostport": true}

func (r Recipe) Validate() error {
	if !ValidID(r.ID) {
		return fmt.Errorf("recipe: invalid id %q (want %s)", r.ID, idRE)
	}
	switch r.Kind {
	case KindVLLM, KindTransformers, KindContainer:
	default:
		return fmt.Errorf("recipe %s: unknown kind %q", r.ID, r.Kind)
	}
	switch r.Modality {
	case ModalityText, ModalityOmni, ModalityASR, ModalityTTS:
	default:
		return fmt.Errorf("recipe %s: unknown modality %q", r.ID, r.Modality)
	}
	if r.Image == "" {
		return fmt.Errorf("recipe %s: image is required", r.ID)
	}
	if r.Kind == KindTransformers {
		if r.Build == "" {
			return fmt.Errorf("recipe %s: kind transformers requires a build context", r.ID)
		}
		if len(r.Entrypoint) == 0 {
			return fmt.Errorf("recipe %s: kind transformers requires an explicit entrypoint "+
				"(an inherited ENTRYPOINT turns the command into arguments for the base image)", r.ID)
		}
	}
	for k := range r.Args {
		if portArgs[k] {
			return fmt.Errorf("recipe %s: %q may not be set in a recipe; "+
				"host ports are allocated at deploy time", r.ID, k)
		}
	}
	if r.Declared.WeightsGiB < 0 || r.Declared.KVGiB < 0 {
		return fmt.Errorf("recipe %s: declared footprint may not be negative", r.ID)
	}
	return nil
}
