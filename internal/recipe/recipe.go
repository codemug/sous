// Package recipe defines the portable half of a model deployment: everything
// true of the model anywhere, and nothing true only of this machine.
//
// Measured values live in observations, never here. Shipping
// weights_gib: 24.87 or cuda_graphs: PIECEWISE inside a recipe would make both
// lies on the next box - they are facts about this hardware running this
// engine build, not about the model.
package recipe

type Kind string

const (
	KindVLLM         Kind = "vllm"
	KindTransformers Kind = "transformers"
	KindContainer    Kind = "container"
)

type Modality string

const (
	ModalityText Modality = "text"
	ModalityOmni Modality = "omni"
	ModalityASR  Modality = "asr"
	ModalityTTS  Modality = "tts"
)

// Footprint is a DECLARED estimate. Truth arrives in an observe.Observation
// after a successful load, and capacity prefers that when it exists.
type Footprint struct {
	WeightsGiB float64 `yaml:"weights_gib" json:"weights_gib"`
	KVGiB      float64 `yaml:"kv_gib" json:"kv_gib"`
}

func (f Footprint) TotalGiB() float64 { return f.WeightsGiB + f.KVGiB }

type Recipe struct {
	ID       string   `yaml:"id" json:"id"`
	Kind     Kind     `yaml:"kind" json:"kind"`
	Modality Modality `yaml:"modality" json:"modality"`
	Model    string   `yaml:"model,omitempty" json:"model,omitempty"`
	Image    string   `yaml:"image" json:"image"`
	ServedAs []string `yaml:"served_as,omitempty" json:"served_as,omitempty"`

	// Build and Entrypoint apply to KindTransformers. The entrypoint override
	// is mandatory there: the vLLM base image sets ENTRYPOINT ["vllm","serve"],
	// which turns a derived service's command into arguments to vLLM. That cost
	// 46 crash loops before it was found, and a Dockerfile ENTRYPOINT [] did
	// not survive the rebuild.
	Build      string   `yaml:"build,omitempty" json:"build,omitempty"`
	Entrypoint []string `yaml:"entrypoint,omitempty" json:"entrypoint,omitempty"`

	Declared Footprint         `yaml:"declared" json:"declared"`
	Args     map[string]any    `yaml:"args,omitempty" json:"args,omitempty"`
	Env      map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	Notes    string            `yaml:"notes,omitempty" json:"notes,omitempty"`

	// Archived keeps a recipe in the catalog without offering it for service.
	// Negative results are worth keeping: they stop someone re-deriving why a
	// configuration lost.
	Archived bool `yaml:"archived,omitempty" json:"archived,omitempty"`
}
