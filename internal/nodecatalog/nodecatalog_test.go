package nodecatalog

import (
	"testing"

	pb "github.com/codemug/sous/internal/pb/souslet/v1"
)

func TestReplaceSnapshotIsAFullReplaceNotAMerge(t *testing.T) {
	c := New()
	c.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{
		NodeId: "asus-gx10", PoolGib: 121.6, ReserveGib: 24,
		Deployments: []*pb.DeploymentState{{RecipeId: "old-model", Phase: "ready"}},
	})
	// A later snapshot with a different deployment set must REPLACE, not
	// accumulate - this is the level-triggered reconciliation the design
	// requires: a container that vanished during a disconnect must vanish
	// from the catalog too, not linger from a stale merge.
	c.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{
		NodeId: "asus-gx10", PoolGib: 121.6, ReserveGib: 24,
		Deployments: []*pb.DeploymentState{{RecipeId: "new-model", Phase: "ready"}},
	})
	view, ok := c.Node("asus-gx10")
	if !ok {
		t.Fatal("node not found")
	}
	if len(view.Deployments) != 1 || view.Deployments[0].RecipeId != "new-model" {
		t.Fatalf("expected exactly [new-model], got %+v", view.Deployments)
	}
}

func TestDisconnectKeepsLastKnownDeploymentsButMarksDisconnected(t *testing.T) {
	c := New()
	c.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{
		NodeId: "asus-gx10",
		Deployments: []*pb.DeploymentState{{RecipeId: "dflash2", Phase: "ready"}},
	})
	c.MarkDisconnected("asus-gx10")
	view, ok := c.Node("asus-gx10")
	if !ok {
		t.Fatal("node not found")
	}
	if view.Connected {
		t.Fatal("expected Connected=false after MarkDisconnected")
	}
	if len(view.Deployments) != 1 {
		t.Fatalf("expected last-known deployment to remain visible, got %+v", view.Deployments)
	}
}

func TestNodeForFindsTheConnectedNodeRunningARecipe(t *testing.T) {
	c := New()
	c.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{
		NodeId:      "asus-gx10",
		Deployments: []*pb.DeploymentState{{RecipeId: "dflash2", Phase: "ready"}},
	})
	node, ok := c.NodeFor("dflash2")
	if !ok || node != "asus-gx10" {
		t.Fatalf("NodeFor(dflash2) = %q, %v; want asus-gx10, true", node, ok)
	}
	if _, ok := c.NodeFor("nonexistent"); ok {
		t.Fatal("expected NodeFor to report not-found for an undeployed recipe")
	}
}

// ---- one recipe on more than one node --------------------------------------

func twoNodes(aPhase, bPhase string) *Catalog {
	c := New()
	c.ReplaceSnapshot("node-b", &pb.NodeSnapshot{Deployments: []*pb.DeploymentState{{RecipeId: "qwen", Phase: bPhase}}})
	c.ReplaceSnapshot("node-a", &pb.NodeSnapshot{Deployments: []*pb.DeploymentState{{RecipeId: "qwen", Phase: aPhase}}})
	return c
}

// NodeFor used to return whichever node Go's map iteration reached first, so
// requests for one model were spread over its nodes at random - and nothing
// could say, ahead of a request, where it would go.
func TestNodeForPicksTheSameNodeEveryTime(t *testing.T) {
	c := twoNodes("running", "running")
	for i := 0; i < 200; i++ {
		if got, _ := c.NodeFor("qwen"); got != "node-a" {
			t.Fatalf("call %d went to %q, want node-a (lowest id) every time", i, got)
		}
	}
}

// A node whose container has exited cannot answer. When another node has the
// same recipe running, that one gets the request.
func TestNodeForPrefersANodeWhereTheContainerIsRunning(t *testing.T) {
	c := twoNodes("exited", "running")
	for i := 0; i < 200; i++ {
		if got, _ := c.NodeFor("qwen"); got != "node-b" {
			t.Fatalf("call %d went to %q, want node-b, the one where it is running", i, got)
		}
	}
	// With nowhere running, it is still found: the request is forwarded and the
	// node answers for itself.
	c = twoNodes("exited", "exited")
	if got, ok := c.NodeFor("qwen"); !ok || got != "node-a" {
		t.Fatalf("NodeFor = %q, %v; want node-a, true", got, ok)
	}
}

// Placements is what a listing reads. It must name, for every recipe, the node
// NodeFor routes to - the two answering differently is a listing that lies.
func TestPlacementsAgreeWithNodeFor(t *testing.T) {
	c := twoNodes("exited", "running")
	c.ReplaceSnapshot("node-c", &pb.NodeSnapshot{Deployments: []*pb.DeploymentState{{RecipeId: "asr", Phase: "running"}}})
	c.ReplaceSnapshot("node-gone", &pb.NodeSnapshot{Deployments: []*pb.DeploymentState{{RecipeId: "kokoro", Phase: "running"}}})
	c.MarkDisconnected("node-gone")

	ps := c.Placements()
	if len(ps) != 2 || ps[0].Deployment.RecipeId != "asr" || ps[1].Deployment.RecipeId != "qwen" {
		t.Fatalf("want asr then qwen (each once, sorted, nothing from a disconnected node), got %+v", ps)
	}
	for _, p := range ps {
		if want, _ := c.NodeFor(p.Deployment.RecipeId); p.NodeID != want {
			t.Errorf("%s: placed on %q but routed to %q", p.Deployment.RecipeId, p.NodeID, want)
		}
	}
	if ps[1].Deployment.Phase != "running" {
		t.Errorf("qwen is placed with phase %q, want the phase of the node it is routed to", ps[1].Deployment.Phase)
	}
}
