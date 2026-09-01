// weights.go is the weight-deletion guard, relocated here from
// internal/larder/delete.go's Delete function (and the directory-walking
// half of internal/larder/larder.go's Scan it depends on) as part of the
// multi-node plan's Task 11 - see that file's own doc comment for the full
// POLICY-vs-SAFETY-guard reasoning this carries over:
//
//   - POLICY guards (the original's StateProtected, an archived recipe's
//     rollback weights) express a judgement, and force is the escape hatch
//     for a judgement the operator disagrees with.
//   - SAFETY guards (the original's StateReferenced, a currently-deployed
//     repo; path escape) are not overridable at all - an escape that deletes
//     weights out from under a running model is not an escape, it is a bug.
//
// ONE REAL ADAPTATION, not a redesign: the original Delete took an
// Entry.State computed by Scan from a full recipe catalog (every recipe,
// archived or not, referencing a repo). souslet keeps no catalog of its own
// - DeployCommand always carries a recipe's full YAML whole, "so souslet
// needs no catalog of its own" per that message's own proto comment, and
// HandleUndeploy forgets it again the moment the recipe is undeployed. That
// makes the ORIGINAL's StateProtected classification (a repo referenced only
// by an ARCHIVED recipe elsewhere in the catalog, kept as rollback
// insurance) genuinely unanswerable here: there is no local record of what
// "archived" even means for a recipe this node cannot currently see, and
// never could without a catalog sync this design deliberately does not add
// (see the package doc in handlers.go: "Deliberately no deploy.Manager and
// no store.Store here").
//
// The one guard souslet CAN answer honestly, from its own live state, is the
// original's StateReferenced check - "is a recipe on this node deployed with
// this repo right now" - and that is exactly the guard still enforced below,
// unconditionally, force included, with identical severity to the original.
// A repo that is merely cached and not currently deployed is deletable here
// without force: a repo the original would have called StateProtected (only
// an archived recipe references it, nothing running) is indistinguishable,
// from souslet's vantage point, from one it would have called StateStale
// (nothing references it at all) - both are simply "not currently deployed
// on this node". This is a deliberate, disclosed simplification of the
// relocated behavior, not an oversight; see the multi-node plan's Task 11
// report for the full reasoning.
package grpcclient

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	pb "github.com/codemug/sous/internal/pb/souslet/v1"
)

// GuardError mirrors internal/larder's GuardError exactly: a refusal on
// guard grounds, carrying the reason so a caller can render it rather than a
// bare failure.
type GuardError struct {
	Repo   string
	Reason string
}

func (e *GuardError) Error() string {
	return fmt.Sprintf("refusing to delete %s: %s", e.Repo, e.Reason)
}

// repoFromWeightsDir converts HuggingFace's cache naming back to a repo id -
// relocated unchanged from larder.RepoFromDir:
// models--Qwen--Qwen3.8-27B-FP8 -> Qwen/Qwen3.8-27B-FP8.
func repoFromWeightsDir(name string) string {
	return strings.ReplaceAll(strings.TrimPrefix(name, "models--"), "--", "/")
}

// hubDir is ModelDir/hub, the same convention fetch.Manager's python
// downloader and internal/httpapi's larderView both use (HF_HOME/hub) -
// see fetch.go's own doc comment on ModelDir for the shared convention.
func hubDir(modelDir string) string {
	return filepath.Join(modelDir, "hub")
}

// findWeightsDir walks hub looking for the snapshot directory matching repo,
// mirroring larder.Scan's own directory walk closely enough that "found"
// here means exactly what "on disk" meant there. A missing hub directory is
// not an error, matching Scan's own doc comment: a fresh node has downloaded
// nothing yet.
func findWeightsDir(hub, repo string) (string, error) {
	entries, err := os.ReadDir(hub)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	for _, e := range entries {
		// The hub also holds xet/ and modules/, which are not snapshots -
		// same exclusion Scan applies.
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "models--") {
			continue
		}
		if repoFromWeightsDir(e.Name()) == repo {
			return filepath.Join(hub, e.Name()), nil
		}
	}
	return "", nil
}

// scanWeightRepos lists every repo id cached under h.ModelDir/hub, for
// Snapshot's CachedWeightRepos - the same directory walk findWeightsDir does
// for one repo, generalized to all of them. Sizes are not measured here (no
// caller of Snapshot needs bytes), unlike larder.Scan's own Entry.Bytes.
func (h *Handlers) scanWeightRepos() ([]string, error) {
	hub := hubDir(h.ModelDir)
	entries, err := os.ReadDir(hub)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "models--") {
			continue
		}
		out = append(out, repoFromWeightsDir(e.Name()))
	}
	return out, nil
}

// repoIsDeployed reports whether repo is the model of any recipe this
// process currently has deployed - the one piece of catalog-shaped
// knowledge deleteWeights needs, sourced from Handlers' own live local state
// (currentlyDeployed, populated by HandleDeploy and cleared by
// HandleUndeploy) rather than a passed-in list, the way
// internal/larder.Scan's caller (internal/httpapi's larderView, via
// s.mgr.List()) used to supply one.
func (h *Handlers) repoIsDeployed(repo string) bool {
	h.footprintsMu.Lock()
	defer h.footprintsMu.Unlock()
	for _, model := range h.currentlyDeployed {
		if model == repo {
			return true
		}
	}
	return false
}

// dirSize relocated unchanged from larder.dirSize: symlinks are not
// followed, because HuggingFace's blob layout links snapshot files to blobs
// inside the same tree, and following them would count the same bytes
// twice.
func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// deleteWeights removes repo's cached weights from ModelDir/hub, subject to
// the guards this file's package doc comment describes - relocated, not
// redesigned, minus the POLICY guard (StateProtected) that needed a recipe
// catalog souslet does not keep. force is accepted for wire compatibility
// with pb.DeleteWeightsCommand.Force, but - exactly as in the original,
// where force never overrode a SAFETY guard either - it has no effect on the
// one guard enforced here.
func (h *Handlers) deleteWeights(repo string, force bool) (int64, error) {
	// Path safety first, before anything is looked up, and regardless of
	// force - relocated unchanged from larder.Delete.
	if repo == "" || strings.Contains(repo, "..") || strings.HasPrefix(repo, "/") ||
		strings.ContainsAny(repo, `\`) {
		return 0, fmt.Errorf("grpcclient: unsafe repo id %q", repo)
	}

	// SAFETY guard: never delete a repo backing a live deployment on this
	// node, force included - see this file's package doc comment.
	if h.repoIsDeployed(repo) {
		return 0, &GuardError{Repo: repo, Reason: "a recipe on this node is currently deployed with it"}
	}

	hub := hubDir(h.ModelDir)
	dir, err := findWeightsDir(hub, repo)
	if err != nil {
		return 0, err
	}
	if dir == "" {
		return 0, fmt.Errorf("grpcclient: %s is not on disk", repo)
	}

	size, err := dirSize(dir)
	if err != nil {
		return 0, err
	}

	// Confirm the resolved directory really sits inside the hub. Symlinks
	// are defeated by resolving both sides - relocated unchanged from
	// larder.Delete.
	realHub, err := filepath.EvalSymlinks(hub)
	if err != nil {
		return 0, err
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return 0, err
	}
	if !strings.HasPrefix(realDir, realHub+string(os.PathSeparator)) {
		return 0, fmt.Errorf("grpcclient: %s resolves outside %s", repo, hub)
	}

	if err := os.RemoveAll(realDir); err != nil {
		return 0, err
	}
	return size, nil
}

// HandleDeleteWeights is a thin wrapper around deleteWeights - dispatch
// only, never a reimplementation of the guard rules (relocated from
// handlers.go, where it was the Task 5 placeholder's caller).
func (h *Handlers) HandleDeleteWeights(ctx context.Context, cmd *pb.DeleteWeightsCommand) *pb.DeleteWeightsResult {
	freed, err := h.deleteWeights(cmd.Repo, cmd.Force)
	if err != nil {
		return &pb.DeleteWeightsResult{Repo: cmd.Repo, Error: err.Error()}
	}
	return &pb.DeleteWeightsResult{Repo: cmd.Repo, BytesFreed: freed}
}
