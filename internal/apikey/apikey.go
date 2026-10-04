// Package apikey issues credentials that can call models and, unless asked
// otherwise by name, nothing else.
//
// WHY A SECOND KIND OF CREDENTIAL. SOUS_API_TOKEN and the operator password are
// root-equivalent by construction: anything holding one can deploy, undeploy,
// delete a recipe, or empty the larder. Handing that to a notebook so it can
// call /v1/chat/completions gives the notebook the ability to destroy the node,
// and there is no way to walk it back short of rotating the token and breaking
// every other caller at the same time.
//
// So a key issued here unlocks the INFERENCE surface only. A leaked key spends
// GPU time; it cannot change what is deployed. That asymmetry is the whole
// point, and it is enforced in one place - Scope - rather than by remembering
// to check at each handler.
//
// AND THEN ADMIN KEYS, for the caller the paragraph above leaves out: the
// script that genuinely has to deploy. It needs a root-equivalent credential
// whatever is done here, and the only one on offer was the single shared
// token - unnamed, unlisted, with no record of when it was last used, and
// revocable only by breaking everyone at once. An admin key is that same power
// with the properties a key has: a name, a row in the list, a last-used time,
// and a revoke button of its own.
//
// It is the exception and is built to stay one. Generate and Create still
// issue inference keys and take no argument that could make them anything
// else; an admin key comes from GenerateAdmin and CreateAdmin, so the
// dangerous path has to be asked for by name at every call site.
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
	"encoding/json"
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

// Permission is what a key may reach.
type Permission string

const (
	// Inference reaches /v1/* and nothing else. It is also what the ABSENCE of
	// a permission means, which is what every key issued before permissions
	// existed has on disk.
	Inference Permission = "inference"
	// Admin reaches everything the admin token does: the control plane, the
	// dashboard, and inference.
	Admin Permission = "admin"
)

// ParsePermission reads a permission as an operator typed or posted it.
//
// Empty means inference, so a caller that has never heard of permissions keeps
// getting the key it always got. An unknown word is an ERROR rather than a
// default: quietly issuing an inference key to someone who asked for "root"
// hides their mistake, and quietly issuing an admin one would be worse.
func ParsePermission(s string) (Permission, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", string(Inference):
		return Inference, nil
	case string(Admin):
		return Admin, nil
	}
	return "", fmt.Errorf("unknown permission %q: use %q or %q", s, Inference, Admin)
}

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

	// Models is an allowlist of model names this key may reach. EMPTY MEANS
	// ALL, which keeps every key issued before scoping existed working exactly
	// as it did - a security feature that silently revokes credentials on
	// upgrade is one nobody deploys.
	//
	// Names are matched the way the gateway resolves them: an alias or a recipe
	// id, case-insensitively.
	Models []string `yaml:"models,omitempty" json:"models,omitempty"`

	// Permission is what this key may reach. EMPTY MEANS INFERENCE, for the
	// same reason an empty Models means all: every key file written before
	// this field existed lacks it, and those keys must come back exactly as
	// they were. Inference keys are still written without it, so a key file is
	// byte-for-byte what it was - and a binary from before this change, reading
	// an admin key's file, ignores the field and treats it as an inference key.
	// A downgrade narrows; it never widens.
	//
	// Read it through Admin and Perm, not directly: only the exact stored word
	// grants anything.
	Permission Permission `yaml:"permission,omitempty" json:"-"`

	CreatedAt time.Time `yaml:"created_at" json:"created_at"`
	// LastUsedAt answers the question that decides whether a key can be
	// revoked safely. Zero means never used, which is the easiest case of all.
	//
	// json:"-" because omitempty does NOT work on a time.Time: an unused key
	// would report "0001-01-01T00:00:00Z", and a client doing date arithmetic
	// on that gets an answer two thousand years wrong rather than an obvious
	// absence. MarshalJSON below emits it only when it is real.
	LastUsedAt time.Time `yaml:"last_used_at,omitempty" json:"-"`
	// Disabled revokes without deleting, so the record of what existed and when
	// it was last used survives the revocation.
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`
}

// Active reports whether this key may still authenticate.
func (k Key) Active() bool { return !k.Disabled }

// Admin reports whether this key reaches the control plane.
//
// AN EXACT MATCH, deliberately not ParsePermission's forgiving one. That
// function reads what a person typed; this reads a file on disk, where a
// hand-edited "Admin" or "root" must fail closed into an inference key rather
// than be interpreted.
func (k Key) Admin() bool { return k.Permission == Admin }

// Perm is the permission to show: never empty, so a listing always says what a
// key may do.
func (k Key) Perm() Permission {
	if k.Admin() {
		return Admin
	}
	return Inference
}

// MayReach reports whether this key may be used on a path.
func (k Key) MayReach(path string) bool { return k.Admin() || Scope(path) }

// Allows reports whether this key may use a given model name.
//
// An empty allowlist means every model - see Models. Matching is
// case-insensitive because the gateway already resolves names that way, and a
// key that works with "Ornith" but not "ornith" would be a puzzle rather than a
// policy.
func (k Key) Allows(model string) bool {
	if len(k.Models) == 0 {
		return true
	}
	for _, m := range k.Models {
		if strings.EqualFold(m, model) {
			return true
		}
	}
	return false
}

// Scoped reports whether this key is limited to particular models.
func (k Key) Scoped() bool { return len(k.Models) > 0 }

// Generate mints a new INFERENCE key, returning the record to store and the
// secret to show the operator once.
func Generate(name string, models ...string) (Key, string, error) {
	return generate(name, Inference, models)
}

// GenerateAdmin mints a key with the admin permission.
//
// NO MODEL LIST, by signature rather than by a check. An allowlist on a key
// that can deploy any model it likes would promise a limit nothing enforces.
func GenerateAdmin(name string) (Key, string, error) {
	return generate(name, Admin, nil)
}

func generate(name string, perm Permission, models []string) (Key, string, error) {
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

	k := Key{
		ID:        hex.EncodeToString(idb),
		Name:      name,
		Models:    cleanModels(models),
		Hint:      tail(secret),
		Hash:      Hash(secret),
		CreatedAt: time.Now().UTC(),
	}
	// Only admin is written down; inference stays the absence of the field.
	if perm == Admin {
		k.Permission = Admin
	}
	return k, secret, nil
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

// MarshalJSON omits a never-used timestamp rather than emitting the zero time,
// and always names the permission.
//
// encoding/json cannot omitempty a struct, so the tag alone would put
// "0001-01-01T00:00:00Z" on the wire for every key that has never been used -
// which reads as a real date to anything that parses it.
func (k Key) MarshalJSON() ([]byte, error) {
	// An alias breaks the method set, so this does not recurse.
	type plain Key
	out := struct {
		plain
		// ALWAYS PRESENT, unlike on disk. Omitting it for inference keys would
		// make a missing field mean two things to a client: an inference key,
		// or a server too old to know the difference.
		Permission Permission `json:"permission"`
		LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	}{plain: plain(k), Permission: k.Perm()}
	if !k.LastUsedAt.IsZero() {
		t := k.LastUsedAt
		out.LastUsedAt = &t
	}
	return json.Marshal(out)
}

// cleanModels normalises an allowlist, dropping blanks and duplicates.
//
// A list containing an empty string would otherwise be non-empty and match
// nothing, producing a key that authenticates and is then refused for every
// model - which reads as a broken key rather than a scoped one.
func cleanModels(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range in {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		k := strings.ToLower(m)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, m)
	}
	return out
}
