package apikey

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codemug/sous/internal/store"
)

// Manager owns the issued keys.
type Manager struct {
	Store *store.Store

	mu sync.Mutex
	// lastUsed is buffered rather than written on every request. A key used in
	// a streaming loop would otherwise rewrite its own file per token, turning
	// a timestamp nobody reads in real time into sustained disk writes on the
	// hot path.
	pending map[string]time.Time
}

// ValidID keeps a key id inside one path segment. The store guards this too;
// checking here means a bad id is refused before it reaches the filesystem.
func ValidID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

func (m *Manager) List() ([]Key, error) {
	names, err := m.Store.List(store.KindAPIKey)
	if err != nil {
		return nil, err
	}
	out := make([]Key, 0, len(names))
	for _, n := range names {
		var k Key
		if err := m.Store.ReadYAML(store.KindAPIKey, n, &k); err != nil {
			continue // a single unreadable key must not hide every other one
		}
		out = append(out, k)
	}
	// Newest first: the key someone just made is the one they are looking for.
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// Create issues a key and returns the secret ONCE. It is never recoverable
// afterwards, by anyone, including whoever runs this process.
func (m *Manager) Create(name string, models ...string) (Key, string, error) {
	k, secret, err := Generate(name, models...)
	if err != nil {
		return Key{}, "", err
	}
	if err := m.Store.WriteYAML(store.KindAPIKey, k.ID, k); err != nil {
		return Key{}, "", err
	}
	return k, secret, nil
}

// Revoke disables a key, keeping the record.
//
// DISABLED, NOT DELETED, by default: the row is the only evidence that the key
// existed, when it was last used, and what it was called. Deleting it answers
// "is this key still valid" and destroys the answer to "was this key ever
// used, and by whom" - which is the question asked after a leak.
func (m *Manager) Revoke(id string) error {
	if !ValidID(id) {
		return fmt.Errorf("apikey: invalid id %q", id)
	}
	var k Key
	if err := m.Store.ReadYAML(store.KindAPIKey, id, &k); err != nil {
		return err
	}
	k.Disabled = true
	return m.Store.WriteYAML(store.KindAPIKey, id, k)
}

// Delete removes a key permanently, for tidying up records nobody needs.
func (m *Manager) Delete(id string) error {
	if !ValidID(id) {
		return fmt.Errorf("apikey: invalid id %q", id)
	}
	return m.Store.Delete(store.KindAPIKey, id)
}

// Authenticate resolves a presented secret to an active key.
func (m *Manager) Authenticate(secret string) (Key, bool) {
	if !Looks(secret) {
		return Key{}, false
	}
	keys, err := m.List()
	if err != nil {
		return Key{}, false
	}
	k, ok := Verify(keys, secret)
	if ok {
		m.touch(k.ID)
	}
	return k, ok
}

// touch records use without writing to disk on the request path.
func (m *Manager) touch(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending == nil {
		m.pending = map[string]time.Time{}
	}
	m.pending[id] = time.Now().UTC()
}

// FlushLastUsed persists buffered use timestamps. Called on a timer, and the
// loss window on an unclean shutdown is bounded by that interval - an
// approximate last-used time is worth far more than an exact one bought with a
// disk write per request.
func (m *Manager) FlushLastUsed() {
	m.mu.Lock()
	pending := m.pending
	m.pending = nil
	m.mu.Unlock()

	for id, t := range pending {
		var k Key
		if err := m.Store.ReadYAML(store.KindAPIKey, id, &k); err != nil {
			continue
		}
		if t.After(k.LastUsedAt) {
			k.LastUsedAt = t
			_ = m.Store.WriteYAML(store.KindAPIKey, id, k)
		}
	}
}

// Scope decides what an API key may reach.
//
// ONE PLACE, ON PURPOSE. The rule is "inference only", and expressing it as a
// path predicate here means a route added later is refused by default rather
// than exposed by an omission. A handler-by-handler check would eventually miss
// one, and the failure mode of missing one is a notebook credential that can
// undeploy a model.
func Scope(path string) bool {
	return path == "/v1/models" || strings.HasPrefix(path, "/v1/")
}

// Guard adapts a Manager to the narrow interface auth needs, so the auth
// package depends on two methods rather than on this whole package.
type Guard struct{ M *Manager }

// Authenticate reports the key's NAME on success, which is what belongs in a
// log line: an id identifies the row, a name identifies who to ask about it.
func (g Guard) Authenticate(secret string) (string, []string, bool) {
	if g.M == nil {
		return "", nil, false
	}
	k, ok := g.M.Authenticate(secret)
	if !ok {
		return "", nil, false
	}
	return k.Name, k.Models, true
}

func (g Guard) Scope(path string) bool { return Scope(path) }
