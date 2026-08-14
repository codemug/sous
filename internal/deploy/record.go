package deploy

import (
	"time"

	"github.com/codemug/sous/internal/observe"
)

// Record is what this node granted a recipe. Recipes stay portable; everything
// machine-specific - the port that was free, the container that resulted, what
// the boot log reported - lands here.
type Record struct {
	RecipeID    string              `yaml:"recipe_id" json:"recipe_id"`
	HostPort    int                 `yaml:"host_port" json:"host_port"`
	ContainerID string              `yaml:"container_id" json:"container_id"`
	StartedAt   time.Time           `yaml:"started_at" json:"started_at"`
	Observation observe.Observation `yaml:"observation" json:"observation"`
}
