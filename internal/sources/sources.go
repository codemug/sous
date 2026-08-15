// Package sources mirrors git repositories of recipes, read-only.
//
// Sous never writes into a mirror. That is what makes a fetch always a clean
// fast-forward and keeps the mirror a faithful copy of the repo; local edits
// live in overlays instead.
//
// WHY GIT IS SHELLED OUT TO WHEN DOCKER IS NOT: the reason Docker is driven
// through its API is that Compose's SEMANTICS caused real failures here -
// archived services that kept running, teardown that could not parse its own
// file, up -d that would not rebuild. Git's CLI has no equivalent trap,
// `git fetch --ff-only` is exactly the operation wanted, and vendoring a git
// implementation to avoid a subprocess would be a large dependency for no
// safety gain. Every invocation uses explicit argument vectors - nothing is
// interpolated into a shell - and paths are validated before use.
package sources

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/codemug/sous/internal/recipe"
	"gopkg.in/yaml.v3"
)

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

type Source struct {
	Name        string    `yaml:"name" json:"name"`
	URL         string    `yaml:"url" json:"url"`
	Ref         string    `yaml:"ref" json:"ref"`
	LastSHA     string    `yaml:"last_sha,omitempty" json:"last_sha,omitempty"`
	LastFetched time.Time `yaml:"last_fetched,omitempty" json:"last_fetched,omitempty"`
}

type Manager struct {
	Root string // holds one directory per source
}

func (m *Manager) dir(name string) (string, error) {
	if !nameRE.MatchString(name) {
		return "", fmt.Errorf("sources: invalid source name %q", name)
	}
	return filepath.Join(m.Root, name), nil
}

// Fetch clones the mirror if absent and fast-forwards it otherwise, returning
// the resulting sha. It never deploys anything: updating what is on offer and
// changing what is running are separate, explicit acts.
func (m *Manager) Fetch(ctx context.Context, s Source) (string, error) {
	dir, err := m.dir(s.Name)
	if err != nil {
		return "", err
	}
	ref := s.Ref
	if ref == "" {
		ref = "main"
	}

	if !isGitRepo(dir) {
		// A mirror is disposable. If the directory exists but is not a repo -
		// an interrupted clone, a hand-deleted .git - re-cloning is the
		// recovery, and it is cheaper than attempting repair.
		if err := os.RemoveAll(dir); err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
			return "", err
		}
		if out, err := git(ctx, "", "clone", "--depth", "1", "--branch", ref, "--", s.URL, dir); err != nil {
			return "", fmt.Errorf("sources: cloning %s: %w: %s", s.Name, err, out)
		}
	} else {
		if out, err := git(ctx, dir, "fetch", "--depth", "1", "origin", ref); err != nil {
			return "", fmt.Errorf("sources: fetching %s: %w: %s", s.Name, err, out)
		}
		// Hard reset rather than merge: the mirror is a copy, never a working
		// tree, so there is nothing local to preserve and nothing to conflict.
		if out, err := git(ctx, dir, "reset", "--hard", "origin/"+ref); err != nil {
			return "", fmt.Errorf("sources: updating %s: %w: %s", s.Name, err, out)
		}
	}

	sha, err := git(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("sources: reading %s head: %w", s.Name, err)
	}
	return strings.TrimSpace(sha), nil
}

// LoadRecipes reads every *.yaml in a mirror. An invalid recipe is an error
// naming its file, never a silent skip: a source that quietly loses entries
// looks smaller than it is, which is worse than failing loudly.
func (m *Manager) LoadRecipes(name string) ([]recipe.Recipe, error) {
	dir, err := m.dir(name)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("sources: %s has not been fetched: %w", name, err)
	}
	var out []recipe.Recipe
	for _, e := range entries {
		if e.IsDir() || (filepath.Ext(e.Name()) != ".yaml" && filepath.Ext(e.Name()) != ".yml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var r recipe.Recipe
		if err := yaml.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("sources: %s/%s: %w", name, e.Name(), err)
		}
		if err := r.Validate(); err != nil {
			return nil, fmt.Errorf("sources: %s/%s: %w", name, e.Name(), err)
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func isGitRepo(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && fi.IsDir()
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// A prompt would hang a background fetch forever rather than failing.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	return string(out), err
}
