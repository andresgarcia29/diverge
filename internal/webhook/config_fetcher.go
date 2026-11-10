package webhook

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
)

// ConfigFetcher fetches .diverge.yaml from a source code repository.
type ConfigFetcher interface {
	FetchConfig(ctx context.Context, provider, project, ref string) ([]byte, error)
}

// errNoDotDiverge aborts an Environment reconcile for a project that has no
// .diverge.yaml when the handler is set to skip such projects.
var errNoDotDiverge = errors.New("project has no .diverge.yaml")

// previewEnvName generates a Kubernetes-safe, project-scoped Environment name.
// Uses a hash of the project path to prevent collisions across projects
// with the same MR/PR number (e.g., two repos both with MR #42).
func previewEnvName(prefix, project string, id int) string {
	h := sha256.Sum256([]byte(project))
	return fmt.Sprintf("%s-%x-%d", prefix, h[:4], id)
}
