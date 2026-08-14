package larder

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// GuardError is a refusal on policy grounds, carrying the reason so a caller
// can render it rather than reporting a bare failure.
type GuardError struct {
	Repo   string
	Reason string
}

func (e *GuardError) Error() string {
	return fmt.Sprintf("refusing to delete %s: %s", e.Repo, e.Reason)
}

// Delete removes a snapshot, subject to guards.
//
// There are two kinds of guard and force treats them differently:
//
//   - POLICY guards (protected rollback) express a judgement, and force is
//     exactly the escape hatch for a judgement the operator disagrees with.
//   - SAFETY guards (referenced by an active recipe, path escaping the hub)
//     are not overridable at all. A blocking tool with no escape gets routed
//     around in worse ways, but an escape that deletes the weights out from
//     under a running model is not an escape, it is a bug.
//
// The repo is looked up in entries rather than re-derived, so classification
// and deletion cannot disagree about what something is.
func Delete(hubDir, repo string, entries []Entry, force bool) (int64, error) {
	// Path safety first, before anything is looked up, and regardless of force.
	if repo == "" || strings.Contains(repo, "..") || strings.HasPrefix(repo, "/") ||
		strings.ContainsAny(repo, `\`) {
		return 0, fmt.Errorf("larder: unsafe repo id %q", repo)
	}

	var found *Entry
	for i := range entries {
		if entries[i].Repo == repo {
			found = &entries[i]
			break
		}
	}
	if found == nil {
		return 0, fmt.Errorf("larder: %s is not on disk", repo)
	}

	switch found.State {
	case StateReferenced:
		return 0, &GuardError{Repo: repo, Reason: fmt.Sprintf(
			"an active recipe references it (%s)", strings.Join(found.ReferencedBy, ", "))}
	case StateProtected:
		if !force {
			return 0, &GuardError{Repo: repo, Reason: fmt.Sprintf(
				"these are rollback weights for the archived recipe %s; deleting them "+
					"turns a redeploy into a re-download during an outage",
				strings.Join(found.ReferencedBy, ", "))}
		}
	}

	// Confirm the resolved directory really sits inside the hub. Symlinks are
	// defeated by resolving both sides.
	realHub, err := filepath.EvalSymlinks(hubDir)
	if err != nil {
		return 0, err
	}
	realDir, err := filepath.EvalSymlinks(found.Dir)
	if err != nil {
		return 0, err
	}
	if !strings.HasPrefix(realDir, realHub+string(os.PathSeparator)) {
		return 0, fmt.Errorf("larder: %s resolves outside %s", repo, hubDir)
	}

	if err := os.RemoveAll(realDir); err != nil {
		return 0, err
	}
	return found.Bytes, nil
}
