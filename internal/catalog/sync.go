package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"

	"gopkg.in/yaml.v3"

	"github.com/codemug/sous/internal/recipe"
	"github.com/codemug/sous/internal/store"
)

// SyncResult reports what a seed sync did, per recipe id.
type SyncResult struct {
	Added   []string `json:"added"`
	Updated []string `json:"updated"`
	Current []string `json:"current"`
	Kept    []string `json:"kept"`
}

// seedMark is the digest of the recipe content Sous itself last wrote.
type seedMark struct {
	Digest string `yaml:"digest" json:"digest"`
}

func digestOf(r recipe.Recipe) (string, error) {
	b, err := yaml.Marshal(r)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// SyncSeeds brings an existing install's catalog up to date with the seeds
// compiled into this binary.
//
// SeedIfEmpty deliberately does nothing once any recipe exists, so an operator
// editing a recipe is never overwritten by a restart. The cost of that rule was
// that a NEW recipe, or a CORRECTED one, could never reach an install that had
// already been seeded - the fix shipped in a release and stayed there. Working
// around it meant deleting the recipe directory by hand on the node.
//
// The distinction that was missing is provenance. A recipe whose content still
// matches what Sous wrote is one nobody has touched, and updating it loses
// nothing. A recipe that differs is the operator's, and is left alone. That is
// the same rule a package manager applies to a modified config file, and it
// needs a record of what was written, which is what seedMark is.
//
// Installs seeded before marks existed have no record, so their recipes are
// indistinguishable from edited ones and are KEPT. That is the safe direction:
// missing recipes still arrive, and nothing an operator may have tuned is
// silently rewritten.
//
// force overrides the provenance check and overwrites everything. It exists so
// that policy ("do not clobber edits") stays separate from safety, and the
// caller can choose - the same split deploy already uses.
func (c *Catalog) SyncSeeds(force bool) (SyncResult, error) {
	var res SyncResult

	for _, seed := range Seeds() {
		seedDigest, err := digestOf(seed)
		if err != nil {
			return res, err
		}

		existing, err := c.Get(seed.ID)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return res, err
			}
			// Never seen here. Always safe to add: there is nothing to lose.
			if err := c.saveSeed(seed, seedDigest); err != nil {
				return res, err
			}
			res.Added = append(res.Added, seed.ID)
			continue
		}

		currentDigest, err := digestOf(existing)
		if err != nil {
			return res, err
		}
		if currentDigest == seedDigest {
			res.Current = append(res.Current, seed.ID)
			continue
		}

		if force || c.wroteThis(seed.ID, currentDigest) {
			if err := c.saveSeed(seed, seedDigest); err != nil {
				return res, err
			}
			res.Updated = append(res.Updated, seed.ID)
			continue
		}

		res.Kept = append(res.Kept, seed.ID)
	}

	return res, nil
}

// wroteThis reports whether the recipe on disk is byte-for-byte what Sous last
// wrote, which is what makes replacing it lossless.
func (c *Catalog) wroteThis(id, currentDigest string) bool {
	var m seedMark
	if err := c.s.ReadYAML(store.KindSeedMark, id, &m); err != nil {
		return false
	}
	return m.Digest != "" && m.Digest == currentDigest
}

func (c *Catalog) saveSeed(r recipe.Recipe, digest string) error {
	if err := c.Save(r); err != nil {
		return err
	}
	return c.s.WriteYAML(store.KindSeedMark, r.ID, seedMark{Digest: digest})
}
