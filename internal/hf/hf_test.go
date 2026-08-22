package hf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSetAndRead(t *testing.T) {
	s := newStore(t)
	if s.Configured() {
		t.Fatal("a fresh store reports a token")
	}
	if err := s.Set("hf_abcdefghijklmnop"); err != nil {
		t.Fatal(err)
	}
	if got := s.Token(); got != "hf_abcdefghijklmnop" {
		t.Errorf("Token() = %q", got)
	}
	if !s.Configured() {
		t.Error("Configured() is false after Set")
	}
}

// A token pasted out of a browser arrives with a newline more often than not.
func TestWhitespaceIsTrimmedNotRejected(t *testing.T) {
	s := newStore(t)
	if err := s.Set("  hf_abcdefghijklmnop\n"); err != nil {
		t.Fatal(err)
	}
	if got := s.Token(); got != "hf_abcdefghijklmnop" {
		t.Errorf("Token() = %q - surrounding whitespace survived", got)
	}
}

// VALIDATED ON THE WAY IN. The alternative is an opaque 401 inside a download
// container ten minutes later, which reads as "my token is wrong" when what
// actually happened is that a username or a URL was pasted.
func TestRejectsThingsThatAreNotTokens(t *testing.T) {
	s := newStore(t)
	for _, bad := range []string{"", "   ", "usmanshahid", "sk-sous-abcdef",
		"hf_with space", "https://huggingface.co/settings/tokens"} {
		if err := s.Set(bad); err == nil {
			t.Errorf("Set(%q) was accepted", bad)
		}
	}
	if s.Configured() {
		t.Error("a rejected value was stored anyway")
	}
}

// A REJECTED SET MUST NOT DESTROY A WORKING TOKEN. Someone fixing a typo should
// not end up with no credential at all.
func TestFailedSetLeavesTheExistingTokenAlone(t *testing.T) {
	s := newStore(t)
	if err := s.Set("hf_goodtokenvalue123"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("not-a-token"); err == nil {
		t.Fatal("bad token accepted")
	}
	if got := s.Token(); got != "hf_goodtokenvalue123" {
		t.Errorf("Token() = %q - a rejected Set clobbered the good one", got)
	}
}

// It is a credential in a file. 0600, and never widened by the temp-file dance.
func TestFileIsNotReadableByAnyoneElse(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("hf_abcdefghijklmnop"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "secrets", "huggingface"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file is %o, want 600", perm)
	}
	di, err := os.Stat(filepath.Join(dir, "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("secrets dir is %o, want 700", perm)
	}
}

// The hint identifies WHICH token is installed without being usable as one.
func TestHintIsNotTheToken(t *testing.T) {
	s := newStore(t)
	if h := s.Hint(); h != "" {
		t.Errorf("Hint() = %q with no token", h)
	}
	full := "hf_abcdefghijklmnopqrst"
	if err := s.Set(full); err != nil {
		t.Fatal(err)
	}
	h := s.Hint()
	if strings.Contains(h, "abcdefghijklmnop") || h == full {
		t.Fatalf("Hint() = %q leaks the token", h)
	}
	if !strings.HasSuffix(h, "qrst") {
		t.Errorf("Hint() = %q cannot distinguish two tokens", h)
	}
}

// BOTH NAMES. huggingface_hub reads HF_TOKEN; older paths and some vLLM builds
// read HUGGING_FACE_HUB_TOKEN. Setting one produces a 401 that depends on which
// library version is in the image.
func TestEnvCarriesBothVariableNames(t *testing.T) {
	s := newStore(t)
	if env := s.Env(); env != nil {
		t.Errorf("Env() = %v with no token configured", env)
	}
	if err := s.Set("hf_abcdefghijklmnop"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(s.Env(), " ")
	for _, want := range []string{"HF_TOKEN=hf_abcdefghijklmnop",
		"HUGGING_FACE_HUB_TOKEN=hf_abcdefghijklmnop"} {
		if !strings.Contains(got, want) {
			t.Errorf("Env() = %q, missing %q", got, want)
		}
	}
}

func TestClearIsIdempotent(t *testing.T) {
	s := newStore(t)
	if err := s.Clear(); err != nil {
		t.Errorf("Clear() on an empty store: %v", err)
	}
	if err := s.Set("hf_abcdefghijklmnop"); err != nil {
		t.Fatal(err)
	}
	if err := s.Clear(); err != nil {
		t.Fatal(err)
	}
	if s.Configured() {
		t.Error("still configured after Clear")
	}
	if err := s.Clear(); err != nil {
		t.Errorf("second Clear(): %v", err)
	}
}
