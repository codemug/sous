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
