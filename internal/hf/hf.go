// Package hf holds the HuggingFace token used to pull gated weights.
//
// WHY THIS IS NOT AN API KEY. internal/apikey stores a HASH: Sous mints those
// secrets, shows them once, and only ever needs to check whether a presented
// one matches. This token is the opposite - it is issued by someone else, Sous
// never validates it, and it has to be handed to a container in the clear every
// time a download runs. It therefore has to be stored recoverably, which is a
// weaker position, and the mitigations are different: 0600 on disk, never
// rendered back to a page, and never written into a recipe.
//
// NOT IN THE RECIPE, deliberately. Recipes are published to git - the catalog
// in this repo is generated from them - so a token in recipe.Env would be a
// credential in version control the first time anyone ran the seed rewrite. It
// is injected at container-creation time instead, and the recipe never sees it.
package hf

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Prefix is what a HuggingFace user access token starts with.
//
// Checked on the way in rather than on the way out: a token pasted with a
// trailing newline, or a username pasted by mistake, otherwise fails much later
// as an opaque 401 inside a download container, which is a miserable thing to
// diagnose from a log tail.
const Prefix = "hf_"

// Store is the token on disk.
type Store struct {
	path string
	mu   sync.RWMutex
}

// New opens the store under the data directory.
func New(dataDir string) (*Store, error) {
	dir := filepath.Join(dataDir, "secrets")
	// 0700: the directory listing alone should not be world-readable.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{path: filepath.Join(dir, "huggingface")}, nil
}

// Token returns the stored token, or "" when none is set.
func (s *Store) Token() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, err := os.ReadFile(s.path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Set stores a token, replacing any existing one.
func (s *Store) Set(tok string) error {
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return errors.New("hf: empty token")
	}
	if !strings.HasPrefix(tok, Prefix) {
		return errors.New("hf: a HuggingFace user access token starts with " + Prefix +
			"; this looks like something else")
	}
	if strings.ContainsAny(tok, " \t\n") {
		return errors.New("hf: token contains whitespace")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Written through a temp file in the same directory so a crash mid-write
	// cannot leave a truncated token that fails as a 401 rather than as a
	// missing file. 0600 from creation, never widened.
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".hf-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(tok + "\n"); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

// Clear removes the token. Absent is not an error: the point is the end state.
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Set reports whether a token is configured.
func (s *Store) Configured() bool { return s.Token() != "" }

// Hint is the only form of the token that may leave this process.
//
// Enough to tell two tokens apart when someone is checking WHICH one is
// installed, and not enough to use. The full value is never rendered - unlike
// an API key, there is no one-time reveal, because Sous did not mint this and
// the operator already has it wherever they copied it from.
func (s *Store) Hint() string {
	t := s.Token()
	if t == "" {
		return ""
	}
	if len(t) <= len(Prefix)+4 {
		return Prefix + "…"
	}
	return Prefix + "…" + t[len(t)-4:]
}

// Env is what gets injected into a container that may need to pull weights.
//
// BOTH NAMES. huggingface_hub reads HF_TOKEN; older code paths and some vLLM
// versions still read HUGGING_FACE_HUB_TOKEN. Setting one and not the other
// produces a 401 that depends on which library version happens to be in the
// image, which is the kind of bug that looks like a bad token.
func (s *Store) Env() []string {
	t := s.Token()
	if t == "" {
		return nil
	}
	return []string{"HF_TOKEN=" + t, "HUGGING_FACE_HUB_TOKEN=" + t}
}
