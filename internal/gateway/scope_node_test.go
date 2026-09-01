package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codemug/sous/internal/grpcserver"
	"github.com/codemug/sous/internal/nodecatalog"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
)

// TestScopedKeyIsForbiddenOnTheNodePathToo is
// TestScopedKeyIsForbiddenForAModelItLacks's multi-node counterpart. The
// node-routed path dispatched to proxyOverGRPC BEFORE the local path's scope
// gate and never called auth.FromContext at all, so an API key restricted to
// particular models could reach ANY model on ANY connected node - a silent
// bypass of internal/apikey scoping, which is a shipped feature elsewhere in
// this codebase, not a nicety.
func TestScopedKeyIsForbiddenOnTheNodePathToo(t *testing.T) {
	nodes := nodecatalog.New()
	gsrv := grpcserver.New(nodes, nil)
	// dflash2 is genuinely running on this node (the fake souslet's handshake
	// snapshot reports it), so a 403 here is specifically about the key's
	// scope, not about the model being unreachable.
	stop := dialFakeEchoingSouslet(t, gsrv, "asus-gx10")
	defer stop()

	g := &Gateway{Nodes: nodes, GRPC: gsrv}
	req := scopedCtx(httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"dflash2"}`)), "kokoro", "asr")
	rr := httptest.NewRecorder()
	g.Proxy(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 - a key scoped to other models reached a model on a node: %s",
			rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "model_not_permitted") {
		t.Fatalf("body = %q, want the same model_not_permitted shape the local path returns", rr.Body.String())
	}
}

// TestScopedKeyIsAllowedOnTheNodePathForAModelItCovers is the other half: the
// gate must not refuse the requests it exists to permit.
func TestScopedKeyIsAllowedOnTheNodePathForAModelItCovers(t *testing.T) {
	nodes := nodecatalog.New()
	gsrv := grpcserver.New(nodes, nil)
	stop := dialFakeEchoingSouslet(t, gsrv, "asus-gx10")
	defer stop()

	g := &Gateway{Nodes: nodes, GRPC: gsrv}
	req := scopedCtx(httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"dflash2"}`)), "dflash2")
	rr := httptest.NewRecorder()
	g.Proxy(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a key scoped to exactly this model: %s", rr.Code, rr.Body.String())
	}
}

// TestUnscopedKeyIsUnaffectedOnTheNodePath pins that the new gate applies only
// to SCOPED keys - an unrestricted key (len(Models) == 0) must keep working
// exactly as before, the same rule the local path applies.
func TestUnscopedKeyIsUnaffectedOnTheNodePath(t *testing.T) {
	nodes := nodecatalog.New()
	gsrv := grpcserver.New(nodes, nil)
	stop := dialFakeEchoingSouslet(t, gsrv, "asus-gx10")
	defer stop()

	g := &Gateway{Nodes: nodes, GRPC: gsrv}
	req := scopedCtx(httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"dflash2"}`))) // no models: an unscoped key
	rr := httptest.NewRecorder()
	g.Proxy(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for an unscoped key: %s", rr.Code, rr.Body.String())
	}
}

// TestNodeCatalogIsStillConsultedForRoutingAfterTheScopeGate is a guard
// against fixing the scope bypass by reordering the checks in a way that
// changes what a caller learns: a model NO node runs must still be a 404, not
// a 403, for an unscoped caller.
func TestNodeCatalogIsStillConsultedForRoutingAfterTheScopeGate(t *testing.T) {
	nodes := nodecatalog.New()
	nodes.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{NodeId: "asus-gx10"})
	gsrv := grpcserver.New(nodes, nil)
	g := &Gateway{Nodes: nodes, GRPC: gsrv}

	req := scopedCtx(httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"not-anywhere"}`)), "not-anywhere")
	rr := httptest.NewRecorder()
	g.Proxy(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a model no node runs: %s", rr.Code, rr.Body.String())
	}
}
