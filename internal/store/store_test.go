package store

import (
	"os"
	"path/filepath"
	"testing"
)

type sample struct {
	ID   string `yaml:"id"`
	Size int    `yaml:"size"`
}

func TestWriteReadRoundTrip(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.WriteYAML(KindRecipe, "qwen38", sample{ID: "qwen38", Size: 25}); err != nil {
		t.Fatalf("WriteYAML: %v", err)
	}
	var got sample
	if err := s.ReadYAML(KindRecipe, "qwen38", &got); err != nil {
		t.Fatalf("ReadYAML: %v", err)
	}
	if got.ID != "qwen38" || got.Size != 25 {
		t.Fatalf("round trip lost data: %+v", got)
	}
}

func TestWriteIsAtomic(t *testing.T) {
	root := t.TempDir()
	s, _ := New(root)
	if err := s.WriteYAML(KindRecipe, "a", sample{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	// No temp files may survive a successful write. A half-written recipe is
	// how a crash-looping model becomes unfixable by hand.
	entries, _ := os.ReadDir(filepath.Join(root, "recipes"))
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".yaml" {
			t.Fatalf("stray non-yaml file left behind: %s", e.Name())
		}
	}
}

func TestListReturnsNamesWithoutExtension(t *testing.T) {
	s, _ := New(t.TempDir())
	if err := s.WriteYAML(KindRecipe, "b", sample{ID: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteYAML(KindRecipe, "a", sample{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	names, err := s.List(KindRecipe)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Fatalf("want sorted [a b], got %v", names)
	}
}

func TestRejectsNameEscapingRoot(t *testing.T) {
	s, _ := New(t.TempDir())
	for _, bad := range []string{"../escape", "a/b", "/abs", ".."} {
		if err := s.WriteYAML(KindRecipe, bad, sample{}); err == nil {
			t.Fatalf("accepted dangerous name %q", bad)
		}
	}
}

func TestDeleteRemovesFile(t *testing.T) {
	s, _ := New(t.TempDir())
	if err := s.WriteYAML(KindDeployment, "x", sample{ID: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(KindDeployment, "x"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.ReadYAML(KindDeployment, "x", &sample{}); err == nil {
		t.Fatal("read succeeded after delete")
	}
}
