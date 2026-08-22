package alias

import (
	"strings"
	"testing"

	"github.com/codemug/sous/internal/recipe"
	"github.com/codemug/sous/internal/store"
)

type fakeCat struct{ rs []recipe.Recipe }

func (f fakeCat) List() ([]recipe.Recipe, error) { return f.rs, nil }

func newManager(t *testing.T) *Manager {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Manager{Store: s, Cat: fakeCat{rs: []recipe.Recipe{
		{ID: "qwen38", ServedAs: []string{"qwen", "qwen38"}},
		{ID: "qwen38-dflash2", ServedAs: []string{"dflash2"}},
		{ID: "asr"},
	}}}
}

func TestSetAndRead(t *testing.T) {
	m := newManager(t)
	if err := m.Set("qwen38-dflash2", []string{"fast", "default"}); err != nil {
		t.Fatal(err)
	}
	got := m.Of("qwen38-dflash2")
	if strings.Join(got, ",") != "default,fast" {
		t.Errorf("Of() = %v, want the two names sorted", got)
	}
}

// THE RULE THE FEATURE WAS ASKED FOR. The same alias on two models would make
// one name resolve to whichever the gateway happened to iterate first.
func TestAnAliasCannotBeTakenTwice(t *testing.T) {
	m := newManager(t)
	if err := m.Set("qwen38", []string{"fast"}); err != nil {
		t.Fatal(err)
	}
	err := m.Set("qwen38-dflash2", []string{"fast"})
	if err == nil {
		t.Fatal("the same alias was accepted on a second model")
	}
	if !strings.Contains(err.Error(), "qwen38") {
		t.Errorf("the error does not name the model already holding it: %v", err)
	}
	// And the loser keeps nothing, rather than half a list.
	if got := m.Of("qwen38-dflash2"); len(got) != 0 {
		t.Errorf("a refused Set stored %v", got)
	}
}

// A REFUSED SET MUST NOT DESTROY THE EXISTING LIST. The whole list is replaced
// on success, so a partial failure that cleared it first would lose names over
// a typo.
func TestARefusedSetLeavesTheCurrentListAlone(t *testing.T) {
	m := newManager(t)
	if err := m.Set("qwen38", []string{"fast"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Set("qwen38-dflash2", []string{"quick"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Set("qwen38-dflash2", []string{"quick", "fast"}); err == nil {
		t.Fatal("a list containing a taken name was accepted")
	}
	if got := m.Of("qwen38-dflash2"); strings.Join(got, ",") != "quick" {
		t.Errorf("Of() = %v - the refused Set clobbered the good list", got)
	}
}

// A model may of course keep its own aliases when its list is rewritten.
func TestRewritingAModelsOwnListIsNotACollision(t *testing.T) {
	m := newManager(t)
	if err := m.Set("qwen38", []string{"fast", "big"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Set("qwen38", []string{"fast", "huge"}); err != nil {
		t.Fatalf("a model could not keep its own alias: %v", err)
	}
	if got := m.Of("qwen38"); strings.Join(got, ",") != "fast,huge" {
		t.Errorf("Of() = %v", got)
	}
}

// WIDER THAN ALIAS-VERSUS-ALIAS. The gateway resolves aliases before recipe
// ids, so an alias equal to another model's id would silently SHADOW that
// model rather than fail - traffic for it would quietly land elsewhere.
func TestAnAliasCannotShadowAnotherModelsIDOrServedAs(t *testing.T) {
	m := newManager(t)
	for _, name := range []string{"asr", "qwen", "dflash2"} {
		if err := m.Set("qwen38", []string{name}); err == nil {
			t.Errorf("alias %q was allowed to shadow another model", name)
		}
	}
}

// An alias equal to a name the model already answers to is a no-op that reads
// as a working alias, which is worse than a refusal.
func TestAnAliasCannotDuplicateTheModelsOwnNames(t *testing.T) {
	m := newManager(t)
	for _, name := range []string{"qwen38", "qwen"} {
		if err := m.Set("qwen38", []string{name}); err == nil {
			t.Errorf("alias %q duplicates a name qwen38 already answers to", name)
		}
	}
}

func TestUnknownRecipeIsRefused(t *testing.T) {
	m := newManager(t)
	if err := m.Set("no-such-model", []string{"x"}); err == nil {
		t.Error("aliases were set on a recipe that does not exist")
	}
}

// Case-insensitively, because that is how the gateway resolves. Two aliases
// differing only in case are one name listed twice in /v1/models.
func TestCollisionsAreCaseInsensitive(t *testing.T) {
	m := newManager(t)
	if err := m.Set("qwen38", []string{"Fast"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Set("qwen38-dflash2", []string{"fast"}); err == nil {
		t.Error("FAST and fast were treated as different names")
	}
	if err := m.Set("asr", []string{"a", "A"}); err == nil {
		t.Error("a list containing one name twice was accepted")
	}
}

func TestNamesThatCannotBeModelNamesAreRefused(t *testing.T) {
	m := newManager(t)
	for _, bad := range []string{"two words", "with/slash", `quo"te`, strings.Repeat("x", 65)} {
		if err := m.Set("asr", []string{bad}); err == nil {
			t.Errorf("Set(%q) was accepted", bad)
		}
	}
}

// An empty submission is how a list is cleared.
func TestEmptyListClears(t *testing.T) {
	m := newManager(t)
	if err := m.Set("asr", []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Set("asr", []string{"  ", ""}); err != nil {
		t.Fatal(err)
	}
	if got := m.Of("asr"); len(got) != 0 {
		t.Errorf("Of() = %v after clearing", got)
	}
	// And the freed name is available again.
	if err := m.Set("qwen38", []string{"x"}); err != nil {
		t.Errorf("a cleared alias stayed reserved: %v", err)
	}
}

func TestAllListsEveryModelsAliases(t *testing.T) {
	m := newManager(t)
	if err := m.Set("qwen38", []string{"big"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Set("asr", []string{"ears"}); err != nil {
		t.Fatal(err)
	}
	all, err := m.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all["qwen38"][0] != "big" || all["asr"][0] != "ears" {
		t.Errorf("All() = %v", all)
	}
}
