// Package store owns the on-disk layout. Everything Sous knows is a file, so
// that a broken Sous can be repaired with an editor - which is the whole
// reason this is not a database. Hand-editing a model's flags while the API is
// unhealthy is a thing that actually had to happen four times in one day of
// managing this node by hand.
package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type Kind string

const (
	KindRecipe      Kind = "recipes"
	KindObservation Kind = "observations"
	KindDeployment  Kind = "deployments"
	KindExport      Kind = "exports"
	KindSource      Kind = "sources"
	// Overlays are stored flat as "<source>--<recipe>" rather than nested, so
	// the store's single-segment name guard keeps applying unchanged.
	KindOverlay Kind = "overlays"
)

var allKinds = []Kind{KindRecipe, KindObservation, KindDeployment,
	KindExport, KindSource, KindOverlay}

type Store struct{ root string }

func New(root string) (*Store, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	for _, k := range allKinds {
		if err := os.MkdirAll(filepath.Join(abs, string(k)), 0o750); err != nil {
			return nil, err
		}
	}
	return &Store{root: abs}, nil
}

func (s *Store) Dir(k Kind) string { return filepath.Join(s.root, string(k)) }

// path validates name and resolves it, refusing anything that escapes the
// kind's directory. Rejection happens before any filesystem call, because a
// name reaching the filesystem at all is already too late.
func (s *Store) path(k Kind, name string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return "", fmt.Errorf("store: unsafe name %q", name)
	}
	dir := s.Dir(k)
	p := filepath.Join(dir, name+".yaml")
	if !strings.HasPrefix(filepath.Clean(p), dir+string(os.PathSeparator)) {
		return "", fmt.Errorf("store: name %q escapes %s", name, dir)
	}
	return p, nil
}

// WriteYAML writes via a temp file and rename. A partially written recipe is
// worse than no recipe: it parses as something, and what it parses as is
// whatever the write got through before it stopped.
func (s *Store) WriteYAML(k Kind, name string, v any) error {
	p, err := s.path(k, name)
	if err != nil {
		return err
	}
	b, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once the rename has succeeded
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o640); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

func (s *Store) ReadYAML(k Kind, name string, v any) error {
	p, err := s.path(k, name)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(b, v)
}

func (s *Store) List(k Kind) ([]string, error) {
	entries, err := os.ReadDir(s.Dir(k))
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ".yaml"))
	}
	sort.Strings(out)
	return out, nil
}

func (s *Store) Delete(k Kind, name string) error {
	p, err := s.path(k, name)
	if err != nil {
		return err
	}
	return os.Remove(p)
}
