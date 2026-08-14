// Package larder reconciles downloaded model weights against the catalog.
//
// Weights live in a bind mount outside git, so nothing in any repo records
// what is actually on the node. Measured on gx10 2026-08-14: 292 GB of
// weights, of which 206 GB belonged to models nobody was serving. Two thirds
// of the disk was invisible, and finding that out required writing a one-off
// script. This package is that script, made permanent and given guards.
//
// The listing is a RECONCILIATION, not a directory listing: sizes come from
// walking the disk rather than from a model card, because the card describes
// the repo while the disk holds what actually landed.
package larder

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codemug/sous/internal/recipe"
)

type State string

const (
	// StateReferenced: an active recipe names it, or it is deployed right now.
	StateReferenced State = "referenced"
	// StateProtected: only an archived recipe names it. These are the rollback
	// weights - the difference between a redeploy and a 25 GB re-download
	// during an outage.
	StateProtected State = "protected"
	// StateStale: nothing names it.
	StateStale State = "stale"
)

type Entry struct {
	Repo         string   `json:"repo"`
	Dir          string   `json:"dir"`
	Bytes        int64    `json:"bytes"`
	State        State    `json:"state"`
	ReferencedBy []string `json:"referenced_by,omitempty"`
}

// RepoFromDir converts HuggingFace's cache naming back to a repo id:
// models--Qwen--Qwen3.8-27B-FP8 -> Qwen/Qwen3.8-27B-FP8.
func RepoFromDir(name string) string {
	return strings.ReplaceAll(strings.TrimPrefix(name, "models--"), "--", "/")
}

// Scan walks hubDir and classifies every snapshot. A missing hub directory is
// not an error: a fresh node simply has not downloaded anything yet.
func Scan(hubDir string, recipes []recipe.Recipe, deployed []string) ([]Entry, error) {
	entries, err := os.ReadDir(hubDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	deployedSet := map[string]bool{}
	for _, id := range deployed {
		deployedSet[id] = true
	}

	var out []Entry
	for _, e := range entries {
		// The hub also holds xet/ and modules/, which are not snapshots.
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "models--") {
			continue
		}
		dir := filepath.Join(hubDir, e.Name())
		size, err := dirSize(dir)
		if err != nil {
			return nil, err
		}
		repo := RepoFromDir(e.Name())

		ent := Entry{Repo: repo, Dir: dir, Bytes: size, State: StateStale}
		for _, r := range recipes {
			if r.Model != repo {
				continue
			}
			ent.ReferencedBy = append(ent.ReferencedBy, r.ID)
			switch {
			case deployedSet[r.ID], !r.Archived:
				ent.State = StateReferenced
			case ent.State != StateReferenced:
				// Archived only - protected unless something else claims it.
				ent.State = StateProtected
			}
		}
		out = append(out, ent)
	}

	// Largest first: on a node with 206 GB of stale weights, the operator
	// wants the 65 GB entry at the top, not alphabetical order.
	sort.Slice(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out, nil
}

func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// Symlinks are not followed: HuggingFace's blob layout links snapshot
		// files to blobs inside the same tree, and following them would count
		// the same bytes twice.
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

func Total(entries []Entry) int64 {
	var n int64
	for _, e := range entries {
		n += e.Bytes
	}
	return n
}

// Reclaimable counts only what can be deleted without force.
func Reclaimable(entries []Entry) int64 {
	var n int64
	for _, e := range entries {
		if e.State == StateStale {
			n += e.Bytes
		}
	}
	return n
}
