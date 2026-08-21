// Package apikey issues credentials that can call models and nothing else.
//
// WHY A SECOND KIND OF CREDENTIAL. SOUS_API_TOKEN and the operator password are
// root-equivalent by construction: anything holding one can deploy, undeploy,
// delete a recipe, or empty the larder. Handing that to a notebook so it can
// call /v1/chat/completions gives the notebook the ability to destroy the node,
// and there is no way to walk it back short of rotating the token and breaking
// every other caller at the same time.
//
// So keys issued here unlock the INFERENCE surface only. A leaked key spends
// GPU time; it cannot change what is deployed. That asymmetry is the whole
// point, and it is enforced in one place - Scope - rather than by remembering
// to check at each handler.
//
// STORED AS A HASH, never plaintext. The full key is returned exactly once, at
// creation, and cannot be recovered afterwards: a store that can show a key
// back to a browser can show it to anyone who reaches the store, and this one
// is a directory of YAML on a node that already runs models for other people.
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Prefix marks a Sous key on sight.
//
// "sk-" leads because that is what OpenAI-compatible clients expect and some
// validate; "sous-" follows so a key found in a log or an env file names the
// system that issued it, which is the difference between revoking it in a
// minute and hunting for its owner.
const Prefix = "sk-sous-"

// Key is one issued credential, as stored. The secret itself is NOT here.
type Key struct {
	// ID is the stable handle used to revoke. Safe to log and display.
	ID string `yaml:"id" json:"id"`
	// Name is what a person calls it - "voice demo", "usman's laptop". A key
	// nobody can attribute is a key nobody dares revoke.
	Name string `yaml:"name" json:"name"`
	// Hint is the last few characters of the secret, so an operator can match a
	// key they hold against a row in the list without revealing anything: the
	// tail alone is useless without the rest.
	Hint string `yaml:"hint" json:"hint"`
	// Hash is SHA-256 of the full secret.
	Hash string `yaml:"hash" json:"-"`

	CreatedAt time.Time `yaml:"created_at" json:"created_at"`
	// LastUsedAt answers the question that decides whether a key can be
	// revoked safely. Zero means never used, which is the easiest case of all.
	LastUsedAt time.Time `yaml:"last_used_at,omitempty" json:"last_used_at,omitempty"`
	// Disabled revokes without deleting, so the record of what existed and when
	// it was last used survives the revocation.
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`
}

// Active reports whether this key may still authenticate.
func (k Key) Active() bool { return !k.Disabled }

// Generate mints a new key, returning the record to store and the secret to
// show the operator once.
func Generate(name string) (Key, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Key{}, "", errors.New("a key needs a name; an unattributable key is one nobody dares revoke")
	}
	if len(name) > 64 {
		return Key{}, "", errors.New("name is too long (max 64)")
	}

	// 32 bytes of CSPRNG. base64url keeps it copy-pasteable and free of
	// characters that need escaping in a shell or a YAML scalar.
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Key{}, "", fmt.Errorf("apikey: entropy unavailable: %w", err)
	}
	secret := Prefix + base64.RawURLEncoding.EncodeToString(raw)

	idb := make([]byte, 8)
	if _, err := rand.Read(idb); err != nil {
		return Key{}, "", fmt.Errorf("apikey: entropy unavailable: %w", err)
	}

	return Key{
		ID:        hex.EncodeToString(idb),
		Name:      name,
		Hint:      tail(secret),
		Hash:      Hash(secret),
		CreatedAt: time.Now().UTC(),
	}, secret, nil
}

// Hash is the stored form. Exported so the verifier and the issuer cannot drift
// apart into two different definitions of the same thing.
func Hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// tail is the display hint: enough to recognise a key you already hold,
// nowhere near enough to reconstruct one.
func tail(secret string) string {
	if len(secret) <= 6 {
		return ""
	}
	return secret[len(secret)-6:]
}

// Looks reports whether a string is shaped like a Sous key. Used to skip the
// key lookup entirely for credentials that are plainly something else, not as
// any kind of check - a well-formed string proves nothing.
func Looks(s string) bool { return strings.HasPrefix(s, Prefix) }

// Verify finds the active key matching a presented secret.
//
// Every candidate is compared in constant time and the loop does NOT stop at
// the first match, so the work done is the same whether the secret matches the
// first key, the last, or none - otherwise the time taken leaks the position of
// a key in the store, and with enough attempts, its existence.
func Verify(keys []Key, secret string) (Key, bool) {
	want := Hash(secret)
	var found Key
	ok := false
	for _, k := range keys {
		match := subtle.ConstantTimeCompare([]byte(k.Hash), []byte(want)) == 1
		if match && k.Active() {
			found, ok = k, true
		}
	}
	return found, ok
}
