// Package nodecatalog holds sous-api's live, in-memory view of every
// connected node: capacity, what's deployed, and which recipes' weights
// are on that node's disk. It is fed exclusively by grpcserver's handling
// of NodeSnapshot messages - level-triggered, full replace, never a merge
// or an event log, so a node's last snapshot is always exactly what that
// node itself reported, not an accumulation this process guessed at.
package nodecatalog

import (
	"sync"
	"time"

	pb "github.com/codemug/sous/internal/pb/souslet/v1"
)

type NodeView struct {
	NodeID            string
	PoolGiB           float64
	ReserveGiB        float64
	Connected         bool
	Deployments       []*pb.DeploymentState
	CachedWeightRepos map[string]bool
	// LastSnapshot is when this node's most recent NodeSnapshot landed.
	// The UI reads it as freshness ("snapshot 6s old") - a node can be
	// Connected yet stale if snapshots stop arriving, which is a distinct
	// and important state from disconnected. MarkDisconnected does NOT
	// advance it, so a greyed-out node's age keeps counting up from its
	// real last snapshot.
	LastSnapshot time.Time
}

type Catalog struct {
	mu    sync.RWMutex
	nodes map[string]*NodeView
}

func New() *Catalog {
	return &Catalog{nodes: make(map[string]*NodeView)}
}

// ReplaceSnapshot overwrites everything known about nodeID with snap. Not a
// merge: a deployment missing from snap is gone from the catalog too, on
// the theory that souslet's own live Docker query is more trustworthy than
// anything this process cached from an earlier snapshot.
func (c *Catalog) ReplaceSnapshot(nodeID string, snap *pb.NodeSnapshot) {
	cached := make(map[string]bool, len(snap.CachedWeightRepos))
	for _, r := range snap.CachedWeightRepos {
		cached[r] = true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nodes[nodeID] = &NodeView{
		NodeID:            nodeID,
		PoolGiB:           snap.PoolGib,
		ReserveGiB:        snap.ReserveGib,
		Connected:         true,
		Deployments:       snap.Deployments,
		CachedWeightRepos: cached,
		LastSnapshot:      time.Now(),
	}
}

// MarkDisconnected flips Connected to false but keeps the node's
// last-known deployments visible (greyed out in the UI) rather than
// deleting the entry - "what was running here before it went quiet"
// stays answerable.
func (c *Catalog) MarkDisconnected(nodeID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n, ok := c.nodes[nodeID]; ok {
		n.Connected = false
	}
}

func (c *Catalog) Node(nodeID string) (NodeView, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	n, ok := c.nodes[nodeID]
	if !ok {
		return NodeView{}, false
	}
	return *n, true
}

func (c *Catalog) All() []NodeView {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]NodeView, 0, len(c.nodes))
	for _, n := range c.nodes {
		out = append(out, *n)
	}
	return out
}

// NodeFor returns the connected node currently running recipeID, if any.
// Disconnected nodes are not returned even if their last snapshot still
// lists the recipe - gateway proxying to a node with no live connection
// cannot succeed, so it should fail fast rather than be offered as a
// candidate.
func (c *Catalog) NodeFor(recipeID string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for id, n := range c.nodes {
		if !n.Connected {
			continue
		}
		for _, d := range n.Deployments {
			if d.RecipeId == recipeID {
				return id, true
			}
		}
	}
	return "", false
}
