package apikey

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codemug/sous/internal/store"
)

func newMgr(t *testing.T) *Manager {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Manager{Store: s}
}

// THE PROPERTY THE WHOLE PACKAGE EXISTS FOR. A key reaches models and nothing
// else. Every other credential in this system can destroy the node; if this
// stops holding, handing a key to a notebook hands it the node.
func TestScopeAllowsInferenceAndNothingElse(t *testing.T) {
	for _, p := range []string{
		"/v1/chat/completions", "/v1/completions", "/v1/models",
		"/v1/embeddings", "/v1/audio/speech", "/v1/audio/transcriptions",
		"/v1/messages", "/v1/responses",
	} {
		if !Scope(p) {
			t.Errorf("%s should be reachable with a key", p)
		}
	}
	for _, p := range []string{
		"/api/deploy/qwen36", "/api/undeploy/qwen36", "/api/recipes",
		"/api/recipes/qwen36", "/api/larder/delete", "/api/status",
		"/", "/model/qwen36", "/deployments", "/login", "/api/sources",
	} {
		if Scope(p) {
			t.Errorf("SCOPE HOLE: %s is reachable with an inference key", p)
		}
	}
}

func TestGeneratedKeyIsUsableAndUnguessable(t *testing.T) {
	k, secret, err := Generate("laptop")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, Prefix) {
		t.Errorf("secret %q lacks the %q prefix", secret, Prefix)
	}
	// 32 bytes of entropy, base64url, plus the prefix.
	if len(secret) < len(Prefix)+40 {
		t.Errorf("secret is only %d chars; too short to be 32 bytes of entropy", len(secret))
	}
	if k.Hash == "" || k.Hash == secret {
		t.Error("the secret was stored in place of its hash")
	}
	if strings.Contains(k.Hash, secret) {
		t.Error("the hash contains the secret")
	}
}

// Two keys minted with the same name must not collide, in id or in secret.
func TestKeysAreDistinct(t *testing.T) {
	a, sa, _ := Generate("same-name")
	b, sb, _ := Generate("same-name")
	if a.ID == b.ID {
		t.Error("two keys share an id")
	}
	if sa == sb {
		t.Fatal("two keys share a secret")
	}
	if a.Hash == b.Hash {
		t.Error("two distinct secrets hashed the same")
	}
}

// The hint must identify a key you already hold without helping anyone who
// does not.
func TestHintRevealsOnlyTheTail(t *testing.T) {
	k, secret, _ := Generate("x")
	if k.Hint == "" {
		t.Fatal("no hint stored; an operator cannot match a key to a row")
	}
	if !strings.HasSuffix(secret, k.Hint) {
		t.Errorf("hint %q is not the tail of the secret", k.Hint)
	}
	if len(k.Hint) > 8 {
		t.Errorf("hint %q is long enough to be worth brute-forcing against", k.Hint)
	}
}

func TestAnUnnamedKeyIsRefused(t *testing.T) {
	for _, name := range []string{"", "   "} {
		if _, _, err := Generate(name); err == nil {
			t.Errorf("accepted a key named %q", name)
		}
	}
}

func TestCreateThenAuthenticate(t *testing.T) {
	m := newMgr(t)
	k, secret, err := m.Create("demo")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := m.Authenticate(secret)
	if !ok {
		t.Fatal("a freshly created key did not authenticate")
	}
	if got.ID != k.ID {
		t.Errorf("authenticated as %q, want %q", got.ID, k.ID)
	}
}

func TestWrongSecretIsRefused(t *testing.T) {
	m := newMgr(t)
	if _, _, err := m.Create("demo"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"", "nonsense", Prefix + "wrong", "sk-openai-something",
	} {
		if _, ok := m.Authenticate(bad); ok {
			t.Errorf("secret %q was accepted", bad)
		}
	}
}

// Revoking must stop the key working while KEEPING the record. Deleting it
// answers "is it valid" and destroys the answer to "was it ever used", which
// is the question actually asked after a leak.
func TestRevokeStopsTheKeyButKeepsTheRecord(t *testing.T) {
	m := newMgr(t)
	k, secret, _ := m.Create("leaked")
	if err := m.Revoke(k.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Authenticate(secret); ok {
		t.Fatal("a revoked key still authenticates")
	}
	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("revoke removed the record; %d keys listed, want 1", len(list))
	}
	if !list[0].Disabled {
		t.Error("the surviving record is not marked disabled")
	}
}

func TestDeleteRemovesItEntirely(t *testing.T) {
	m := newMgr(t)
	k, secret, _ := m.Create("temp")
	if err := m.Delete(k.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Authenticate(secret); ok {
		t.Error("a deleted key still authenticates")
	}
	list, _ := m.List()
	if len(list) != 0 {
		t.Errorf("%d keys survived deletion", len(list))
	}
}

// The stored form must never let anyone reconstruct the secret - including
// whoever runs the process and can read the store directly.
func TestTheStoredRecordCannotYieldTheSecret(t *testing.T) {
	m := newMgr(t)
	_, secret, _ := m.Create("demo")
	list, _ := m.List()
	if len(list) != 1 {
		t.Fatal("expected one key")
	}
	k := list[0]
	for field, v := range map[string]string{"ID": k.ID, "Name": k.Name, "Hash": k.Hash} {
		if strings.Contains(v, secret) {
			t.Errorf("%s contains the plaintext secret", field)
		}
	}
	if k.Hint != "" && len(k.Hint) >= len(secret) {
		t.Error("the hint is the whole secret")
	}
}

func TestLastUsedIsRecordedOnUse(t *testing.T) {
	m := newMgr(t)
	k, secret, _ := m.Create("tracked")

	before, _ := m.List()
	if !before[0].LastUsedAt.IsZero() {
		t.Error("a never-used key already has a last-used time")
	}

	if _, ok := m.Authenticate(secret); !ok {
		t.Fatal("authenticate failed")
	}
	m.FlushLastUsed()

	after, _ := m.List()
	if after[0].LastUsedAt.IsZero() {
		t.Fatal("using a key did not record it; stale keys cannot be spotted")
	}
	if after[0].ID != k.ID {
		t.Errorf("recorded against the wrong key")
	}
}

func TestRevokeRejectsBadID(t *testing.T) {
	m := newMgr(t)
	for _, bad := range []string{"../escape", "a/b", "UPPER", ""} {
		if err := m.Revoke(bad); err == nil {
			t.Errorf("accepted id %q", bad)
		}
	}
}

// A key from another issuer must be rejected before it costs a store read.
func TestNonSousSecretsAreRejectedEarly(t *testing.T) {
	if Looks("sk-proj-abc123") {
		t.Error("an OpenAI-shaped key was mistaken for a Sous key")
	}
	if !Looks(Prefix + "abc") {
		t.Error("a Sous-prefixed key was not recognised")
	}
}

// A never-used key must not report a year-1 timestamp: anything parsing that
// gets an answer two thousand years wrong rather than an obvious absence.
func TestNeverUsedKeyOmitsLastUsedInJSON(t *testing.T) {
	k, _, _ := Generate("fresh")
	b, err := json.Marshal(k)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "0001-01-01") {
		t.Errorf("the zero time reached the wire: %s", b)
	}
	if strings.Contains(string(b), "last_used_at") {
		t.Errorf("last_used_at present for a never-used key: %s", b)
	}
	// And a used one still reports it.
	k.LastUsedAt = time.Now()
	b2, _ := json.Marshal(k)
	if !strings.Contains(string(b2), "last_used_at") {
		t.Errorf("a used key lost its timestamp: %s", b2)
	}
}

// EMPTY MEANS ALL. A security feature that silently revokes every key issued
// before it existed is one nobody deploys.
func TestUnscopedKeyReachesEveryModel(t *testing.T) {
	k, _, _ := Generate("legacy")
	if k.Scoped() {
		t.Error("a key with no allowlist reports as scoped")
	}
	for _, m := range []string{"qwen36", "ornith", "anything-at-all"} {
		if !k.Allows(m) {
			t.Errorf("unscoped key refused %q", m)
		}
	}
}

func TestScopedKeyAllowsOnlyItsModels(t *testing.T) {
	k, _, _ := Generate("voice", "asr", "kokoro")
	if !k.Scoped() {
		t.Fatal("a key with an allowlist does not report as scoped")
	}
	for _, m := range []string{"asr", "kokoro"} {
		if !k.Allows(m) {
			t.Errorf("scoped key refused its own model %q", m)
		}
	}
	for _, m := range []string{"qwen36", "dflash2", ""} {
		if k.Allows(m) {
			t.Errorf("scoped key allowed %q, which is not on its list", m)
		}
	}
}

// The gateway resolves names case-insensitively, so a key that works with
// "Ornith" but not "ornith" would be a puzzle rather than a policy.
func TestScopeMatchingIsCaseInsensitive(t *testing.T) {
	k, _, _ := Generate("mixed", "Ornith")
	for _, m := range []string{"ornith", "ORNITH", "Ornith"} {
		if !k.Allows(m) {
			t.Errorf("scoped key refused %q", m)
		}
	}
}

// A list containing only blanks would be non-empty and match nothing, giving a
// key that authenticates and is then refused for everything - which reads as
// broken rather than as scoped.
func TestBlankModelsAreDroppedNotKept(t *testing.T) {
	k, _, _ := Generate("blanks", "", "  ", "asr", "asr")
	if len(k.Models) != 1 || k.Models[0] != "asr" {
		t.Fatalf("models = %v, want exactly [asr]", k.Models)
	}
}

func TestScopeSurvivesTheStore(t *testing.T) {
	m := newMgr(t)
	if _, _, err := m.Create("voice", "asr", "kokoro"); err != nil {
		t.Fatal(err)
	}
	list, _ := m.List()
	if len(list) != 1 {
		t.Fatal("expected one key")
	}
	if len(list[0].Models) != 2 {
		t.Errorf("models = %v, want two after a round trip", list[0].Models)
	}
}

// ---- permissions ----------------------------------------------------------

var controlPlane = []string{
	"/api/deploy/qwen36/asus-gx10", "/api/undeploy/qwen36/asus-gx10", "/api/recipes",
	"/api/recipes/qwen36", "/api/keys", "/api/nodes", "/api/status", "/", "/keys",
}

// THE DEFAULT HAS TO STAY INFERENCE. Admin keys exist now, and the danger of
// adding them is that the ordinary way of issuing a key starts handing out
// more than it used to. It must not: a key made the way keys were always made
// reaches models and nothing else.
func TestAKeyIsInferenceOnlyUnlessAskedOtherwise(t *testing.T) {
	k, _, err := Generate("laptop")
	if err != nil {
		t.Fatal(err)
	}
	if k.Admin() {
		t.Fatal("an ordinary key came out with the admin permission")
	}
	// Not admin, so Scope is the whole of what it may reach - and Scope must
	// still refuse the control plane.
	if !Scope("/v1/chat/completions") {
		t.Error("an inference key cannot reach inference")
	}
	for _, p := range controlPlane {
		if Scope(p) {
			t.Errorf("SCOPE HOLE: an inference key may reach %s", p)
		}
	}
}

// An admin key is the admin token with a name and a revoke button: it reaches
// everything the token does, inference included.
func TestAnAdminKeyReachesTheControlPlane(t *testing.T) {
	k, secret, err := GenerateAdmin("fleet automation")
	if err != nil {
		t.Fatal(err)
	}
	if !k.Admin() {
		t.Fatal("GenerateAdmin returned a key without the admin permission")
	}
	if !strings.HasPrefix(secret, Prefix) {
		t.Errorf("secret = %q, want the %q prefix", secret, Prefix)
	}
	// It carries no model list: there is nothing an allowlist could mean on a
	// key that can deploy any model it likes.
	if k.Scoped() {
		t.Errorf("an admin key carries a model list: %v", k.Models)
	}
}

// FAIL CLOSED ON A HAND-EDITED FILE. Keys are YAML on disk, and the only value
// that grants admin is the exact word. Anything else - a typo, a different
// case, a guess - is an inference key, not an error and not an admin.
func TestOnlyTheExactWordGrantsAdmin(t *testing.T) {
	for _, p := range []Permission{"", "inference", "Admin", "ADMIN", "admin ", " admin", "root", "true", "all"} {
		if (Key{Permission: p}).Admin() {
			t.Errorf("permission %q was treated as admin", p)
		}
	}
	if !(Key{Permission: Admin}).Admin() {
		t.Error("the admin permission itself was not recognised")
	}
}

// Every key issued before permissions existed has no such field on disk. It
// must come back exactly as it was: usable, and inference-only.
func TestAKeyStoredBeforePermissionsExistedIsInferenceOnly(t *testing.T) {
	m := newMgr(t)
	old, secret, err := Generate("issued in v0.23")
	if err != nil {
		t.Fatal(err)
	}
	// What a v0.23 key file holds: no permission field at all.
	legacy := map[string]any{
		"id": old.ID, "name": old.Name, "hint": old.Hint, "hash": old.Hash,
		"created_at": old.CreatedAt,
	}
	if err := m.Store.WriteYAML(store.KindAPIKey, old.ID, legacy); err != nil {
		t.Fatal(err)
	}
	got, ok := m.Authenticate(secret)
	if !ok {
		t.Fatal("a key from before permissions existed stopped authenticating")
	}
	if got.Admin() {
		t.Fatal("a key from before permissions existed came back as admin")
	}
}

func TestAdminPermissionSurvivesTheStore(t *testing.T) {
	m := newMgr(t)
	k, secret, err := m.CreateAdmin("fleet automation")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := m.Authenticate(secret)
	if !ok {
		t.Fatal("a freshly created admin key did not authenticate")
	}
	if got.ID != k.ID || !got.Admin() {
		t.Errorf("authenticated as %q admin=%v, want %q admin=true", got.ID, got.Admin(), k.ID)
	}
	// And an ordinary key made beside it did not inherit anything.
	_, plain, _ := m.Create("notebook")
	if p, ok := m.Authenticate(plain); !ok || p.Admin() {
		t.Errorf("the ordinary key: ok=%v admin=%v, want ok and not admin", ok, p.Admin())
	}
}

// Revoking is the reason to issue an admin KEY instead of sharing the token:
// it has to actually stop the key.
func TestARevokedAdminKeyIsJustARevokedKey(t *testing.T) {
	m := newMgr(t)
	k, secret, _ := m.CreateAdmin("leaked")
	if err := m.Revoke(k.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Authenticate(secret); ok {
		t.Fatal("a revoked admin key still authenticates")
	}
}

// The API always says what a key may do. Leaving the field out for inference
// keys would make "no permission field" mean two things to a client: an
// inference key, or a server too old to know the difference.
func TestJSONAlwaysNamesThePermission(t *testing.T) {
	plain, _, _ := Generate("notebook")
	admin, _, _ := GenerateAdmin("fleet automation")
	for want, k := range map[string]Key{"inference": plain, "admin": admin} {
		b, err := json.Marshal(k)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), `"permission":"`+want+`"`) {
			t.Errorf("JSON for the %s key does not name its permission: %s", want, b)
		}
	}
}

func TestParsePermission(t *testing.T) {
	for in, want := range map[string]Permission{
		"": Inference, "inference": Inference, "admin": Admin, " Admin ": Admin, "INFERENCE": Inference,
	} {
		got, err := ParsePermission(in)
		if err != nil || got != want {
			t.Errorf("ParsePermission(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	// A word it does not know is an error, never a default: silently issuing an
	// inference key to someone who asked for "root" hides their mistake, and
	// silently issuing an admin one would be worse.
	for _, in := range []string{"root", "all", "true", "admin,inference"} {
		if p, err := ParsePermission(in); err == nil {
			t.Errorf("ParsePermission(%q) = %q, want an error", in, p)
		}
	}
}

// The guard is what auth actually calls, so it has to carry the permission
// through - and must not invent it for a key that lacks it.
func TestTheGuardReportsThePermission(t *testing.T) {
	m := newMgr(t)
	_, adminSecret, _ := m.CreateAdmin("fleet automation")
	_, plainSecret, _ := m.Create("notebook", "asr")
	g := Guard{M: m}

	name, models, admin, ok := g.Authenticate(adminSecret)
	if !ok || !admin || name != "fleet automation" || len(models) != 0 {
		t.Errorf("admin key: name=%q models=%v admin=%v ok=%v", name, models, admin, ok)
	}
	name, models, admin, ok = g.Authenticate(plainSecret)
	if !ok || admin || name != "notebook" || len(models) != 1 {
		t.Errorf("inference key: name=%q models=%v admin=%v ok=%v", name, models, admin, ok)
	}
	if _, _, admin, ok := g.Authenticate("sk-sous-not-a-real-key"); ok || admin {
		t.Errorf("an unknown secret: admin=%v ok=%v, want neither", admin, ok)
	}
}

// REVOKE HAS TO STICK. The last-used flush reads a key file, sets a timestamp
// and writes the whole record back. Without a lock around that, a Revoke
// landing between its read and its write is overwritten by the stale copy -
// the operator sees "revoked", and thirty seconds later the key works again.
// For an admin key that is a leaked root credential nobody knows is live.
//
// A key in active use is exactly the one being revoked after a leak, and
// exactly the one the flush is busy with.
func TestRevokeIsNotUndoneByTheLastUsedFlush(t *testing.T) {
	m := newMgr(t)
	for i := 0; i < 150; i++ {
		k, secret, err := m.CreateAdmin("leaked")
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := m.Authenticate(secret); !ok { // marks it used: the flush now has work
			t.Fatal("a fresh key did not authenticate")
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); m.FlushLastUsed() }()
		go func() { defer wg.Done(); _ = m.Revoke(k.ID) }()
		wg.Wait()
		m.FlushLastUsed() // and whatever the revoked key's own last use queued
		if _, ok := m.Authenticate(secret); ok {
			t.Fatalf("round %d: a revoked key authenticates again after a flush", i)
		}
	}
}

// The same race, for Delete: the flush's write must not bring a deleted key's
// file back.
func TestDeleteIsNotUndoneByTheLastUsedFlush(t *testing.T) {
	m := newMgr(t)
	for i := 0; i < 150; i++ {
		k, secret, _ := m.Create("temp")
		m.Authenticate(secret)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); m.FlushLastUsed() }()
		go func() { defer wg.Done(); _ = m.Delete(k.ID) }()
		wg.Wait()
		m.FlushLastUsed()
		if _, ok := m.Authenticate(secret); ok {
			t.Fatalf("round %d: a deleted key came back after a flush", i)
		}
	}
}
