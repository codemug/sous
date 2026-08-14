package deploy

import "github.com/codemug/sous/internal/engine"

// Compile-time assertion that the Docker client satisfies Runtime.
var _ Runtime = (*engine.Docker)(nil)
