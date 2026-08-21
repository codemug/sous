package engine

import (
	"strings"
	"testing"
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
