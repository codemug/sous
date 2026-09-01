package grpcclient

import (
	"context"
	"net"
	"strconv"
	"testing"

	"github.com/codemug/sous/internal/engine"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
	"github.com/codemug/sous/internal/ports"
)

// freePort asks the OS for a port that is free right now and releases it
// again, so a test can name a real, currently-unused port without hardcoding
// one that may be taken on someone else's machine.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestDeployWithNoRequestedPortAllocatesARealOne is the drag-and-drop case:
// the UI sends no port at all, so WantPort is 0. That 0 used to travel
// straight into engine.BuildSpec as the container's HostPort, which Docker
// reads as "pick an ephemeral port" - and nothing recorded what it picked.
// DeployResult.HostPort stayed 0, the next NodeSnapshot's
// DeploymentState.HostPort stayed 0, and portFor returned 0, so souslet's own
// proxy path built http://127.0.0.1:0/... for every request. The headline
// flow produced a model that was running and unreachable.
//
// This asserts the port is real on all four surfaces that matter: the spec
// handed to Docker, the DeployResult, the snapshot, and portFor (the proxy
// path's own lookup) - and that it is genuinely bindable, not just non-zero.
func TestDeployWithNoRequestedPortAllocatesARealOne(t *testing.T) {
	// A range this test owns, starting at a port the OS just confirmed free.
	low := freePort(t)
	rt := &fakeRuntime{}
	h := &Handlers{
		Runtime:  rt,
		ModelDir: t.TempDir(),
		Ports:    ports.Allocator{Low: low, High: low + 20},
		BindHost: "127.0.0.1",
	}

	res := h.HandleDeploy(context.Background(), &pb.DeployCommand{
		RecipeId:   "dflash2",
		RecipeYaml: validRecipeYAML(t, "dflash2"),
		WantPort:   0, // exactly what the drag-and-drop deploy sends
	})
	if res.Error != "" {
		t.Fatalf("HandleDeploy: %s", res.Error)
	}
	if res.HostPort == 0 {
		t.Fatal("DeployResult.HostPort is 0 - the deployed model has no discoverable address")
	}
	if res.HostPort < int32(low) || res.HostPort > int32(low+20) {
		t.Fatalf("HostPort = %d, want a port inside the configured range %d-%d", res.HostPort, low, low+20)
	}

	// The container is actually started on that port, not on 0.
	if len(rt.started) != 1 {
		t.Fatalf("Start called %d times, want 1", len(rt.started))
	}
	if got := rt.started[0].HostPort; got != int(res.HostPort) {
		t.Fatalf("container spec HostPort = %d, want the allocated %d", got, res.HostPort)
	}

	// The proxy path's own lookup resolves to the same real port - this is
	// what forwardToLocalContainer builds its URL from.
	if p, ok := h.portFor("dflash2"); !ok || p != int(res.HostPort) {
		t.Fatalf("portFor = (%d, %v), want (%d, true)", p, ok, res.HostPort)
	}

	// The next snapshot carries it too, so sous-api's catalog view of this
	// node has the real address rather than 0.
	rt.states = map[string]engine.ContainerState{
		engine.ContainerName("dflash2"): {Name: engine.ContainerName("dflash2"), Status: "running"},
	}
	snap := h.Snapshot(context.Background(), "asus-gx10", 121.6, 24)
	var found bool
	for _, d := range snap.Deployments {
		if d.RecipeId == "dflash2" {
			found = true
			if d.HostPort != res.HostPort {
				t.Fatalf("snapshot HostPort = %d, want the allocated %d", d.HostPort, res.HostPort)
			}
		}
	}
	if !found {
		t.Fatal("the deployed recipe is missing from the snapshot")
	}

	// A real port, not just a non-zero number: nothing else holds it, so it
	// can actually be listened on (the fake runtime started no container).
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(res.HostPort))))
	if err != nil {
		t.Fatalf("the allocated port %d is not actually usable: %v", res.HostPort, err)
	}
	ln.Close()
}

// TestDeployAllocationSkipsAPortSomethingElseHolds is the reason allocation
// happens on the NODE rather than on sous-api: availability is decided by
// actually binding, which only answers the right question on the machine the
// container will run on. A port held by a foreign process on this node must
// be skipped.
func TestDeployAllocationSkipsAPortSomethingElseHolds(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer held.Close()
	heldPort := held.Addr().(*net.TCPAddr).Port

	h := &Handlers{
		Runtime:  &fakeRuntime{},
		ModelDir: t.TempDir(),
		Ports:    ports.Allocator{Low: heldPort, High: heldPort + 5},
		BindHost: "127.0.0.1",
	}
	res := h.HandleDeploy(context.Background(), &pb.DeployCommand{
		RecipeId:   "dflash2",
		RecipeYaml: validRecipeYAML(t, "dflash2"),
	})
	if res.Error != "" {
		t.Fatalf("HandleDeploy: %s", res.Error)
	}
	if res.HostPort == int32(heldPort) {
		t.Fatalf("allocated port %d, which another process is holding", heldPort)
	}
}

// TestDeployRejectsAnExplicitlyRequestedPortThatIsTaken mirrors
// deploy.Manager.Deploy's own rule for an explicitly requested port: adoption
// of a specific port is supported, but silently starting a container that
// cannot bind is not.
func TestDeployRejectsAnExplicitlyRequestedPortThatIsTaken(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer held.Close()
	heldPort := held.Addr().(*net.TCPAddr).Port

	rt := &fakeRuntime{}
	h := &Handlers{Runtime: rt, ModelDir: t.TempDir(), BindHost: "127.0.0.1"}
	res := h.HandleDeploy(context.Background(), &pb.DeployCommand{
		RecipeId:   "dflash2",
		RecipeYaml: validRecipeYAML(t, "dflash2"),
		WantPort:   int32(heldPort),
	})
	if res.Error == "" {
		t.Fatal("expected an error deploying onto a port another process holds")
	}
	if len(rt.started) != 0 {
		t.Fatalf("Start called %d times, want 0 - nothing should be started on an unusable port", len(rt.started))
	}
}
