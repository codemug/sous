package sources

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// upstreamRepo builds a real git repository holding recipe YAML. Real git,
// no network, no mock: the point is to exercise the CLI Sous actually drives.
func upstreamRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", ".")
	run("commit", "-q", "-m", "initial")
	return dir
}

func commitMore(t *testing.T, repo, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-q", "-m", "more"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

const goodRecipe = `id: community-qwen
kind: vllm
modality: text
model: Qwen/Qwen3.8-27B-FP8
image: vllm/vllm-openai@sha256:abc
declared:
  weights_gib: 28.9
  kv_gib: 30
args:
  max-model-len: 131072
notes: from the community catalog
`

func TestFetchClonesThenFastForwards(t *testing.T) {
	up := upstreamRepo(t, map[string]string{"qwen.yaml": goodRecipe})
	m := &Manager{Root: t.TempDir()}
	src := Source{Name: "community", URL: up, Ref: "main"}

	sha1, err := m.Fetch(context.Background(), src)
	if err != nil {
		t.Fatalf("first fetch (clone): %v", err)
	}
	if len(sha1) < 7 {
		t.Fatalf("implausible sha %q", sha1)
	}

	sha2, err := m.Fetch(context.Background(), src)
	if err != nil {
		t.Fatalf("second fetch (no change): %v", err)
	}
	if sha1 != sha2 {
		t.Fatalf("sha changed with no upstream commit: %s -> %s", sha1, sha2)
	}

	commitMore(t, up, "another.yaml", strings.Replace(goodRecipe, "community-qwen", "second", 1))
	sha3, err := m.Fetch(context.Background(), src)
	if err != nil {
		t.Fatalf("third fetch (upstream moved): %v", err)
	}
	if sha3 == sha2 {
		t.Fatal("sha did not move after an upstream commit")
	}
}

func TestLoadRecipesReadsAndValidates(t *testing.T) {
	up := upstreamRepo(t, map[string]string{"qwen.yaml": goodRecipe})
	m := &Manager{Root: t.TempDir()}
	if _, err := m.Fetch(context.Background(), Source{Name: "community", URL: up, Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	rs, err := m.LoadRecipes("community")
	if err != nil {
		t.Fatalf("LoadRecipes: %v", err)
	}
	if len(rs) != 1 {
		t.Fatalf("want 1 recipe, got %d", len(rs))
	}
	if rs[0].ID != "community-qwen" || rs[0].Model != "Qwen/Qwen3.8-27B-FP8" {
		t.Fatalf("recipe not parsed: %+v", rs[0])
	}
}

// An invalid recipe must be reported with its filename. Silently skipping it
// makes a source look smaller than it is, which is worse than failing.
func TestInvalidRecipeIsReportedWithFilename(t *testing.T) {
	up := upstreamRepo(t, map[string]string{
		"good.yaml": goodRecipe,
		"bad.yaml":  "id: Bad Id\nkind: vllm\nmodality: text\nimage: x\n",
	})
	m := &Manager{Root: t.TempDir()}
	if _, err := m.Fetch(context.Background(), Source{Name: "community", URL: up, Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	_, err := m.LoadRecipes("community")
	if err == nil {
		t.Fatal("an invalid recipe was accepted")
	}
	if !strings.Contains(err.Error(), "bad.yaml") {
		t.Fatalf("error must name the file, got: %v", err)
	}
}

func TestRejectsUnsafeSourceName(t *testing.T) {
	m := &Manager{Root: t.TempDir()}
	for _, bad := range []string{"../escape", "a/b", "", "UPPER", ".."} {
		if _, err := m.Fetch(context.Background(), Source{Name: bad, URL: "x", Ref: "main"}); err == nil {
			t.Fatalf("accepted unsafe source name %q", bad)
		}
		if _, err := m.LoadRecipes(bad); err == nil {
			t.Fatalf("LoadRecipes accepted unsafe name %q", bad)
		}
	}
}

func TestLoadRecipesOnMissingMirrorErrors(t *testing.T) {
	m := &Manager{Root: t.TempDir()}
	if _, err := m.LoadRecipes("never-fetched"); err == nil {
		t.Fatal("expected an error for a source that was never fetched")
	}
}

func TestFetchOnBadURLErrors(t *testing.T) {
	m := &Manager{Root: t.TempDir()}
	_, err := m.Fetch(context.Background(),
		Source{Name: "broken", URL: filepath.Join(t.TempDir(), "nope"), Ref: "main"})
	if err == nil {
		t.Fatal("cloning a nonexistent repo should error")
	}
}

// A mirror is disposable: deleting it and re-cloning is the recovery path, so
// a corrupted mirror must not wedge the source permanently.
func TestFetchRecoversFromACorruptedMirror(t *testing.T) {
	up := upstreamRepo(t, map[string]string{"qwen.yaml": goodRecipe})
	root := t.TempDir()
	m := &Manager{Root: root}
	src := Source{Name: "community", URL: up, Ref: "main"}
	if _, err := m.Fetch(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	// Corrupt it: remove .git but leave the directory.
	if err := os.RemoveAll(filepath.Join(root, "community", ".git")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Fetch(context.Background(), src); err != nil {
		t.Fatalf("fetch did not recover from a corrupted mirror: %v", err)
	}
}
