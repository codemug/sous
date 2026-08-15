package httpapi

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func mustListenTest(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// Adoption is the reason this exists: a service already serving on :8004 with
// clients pointed at it cannot move under Sous if Sous insists on a fresh
// port from its own range.
func TestDeployHonoursAnExplicitPort(t *testing.T) {
	h := newTestServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/deploy/kokoro?port=41999", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	var rec map[string]any
	json.Unmarshal(rr.Body.Bytes(), &rec)
	if got := rec["host_port"]; got != float64(41999) {
		t.Fatalf("want the requested port 41999, got %v", got)
	}
}

func TestDeployWithoutPortStillAutoAllocates(t *testing.T) {
	h := newTestServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/deploy/kokoro", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	var rec map[string]any
	json.Unmarshal(rr.Body.Bytes(), &rec)
	p, _ := rec["host_port"].(float64)
	if p < 41300 || p > 41400 {
		t.Fatalf("expected a port from the configured range, got %v", p)
	}
}

func TestDeployRejectsNonsensePort(t *testing.T) {
	h := newTestServer(t)
	for _, bad := range []string{"0", "-1", "70000", "abc"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/deploy/kokoro?port="+bad, nil))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("port=%q got %d, want 400", bad, rr.Code)
		}
	}
}

// A port already held must still be refused, even when explicitly asked for -
// the bind test is a safety guard, not a preference.
func TestExplicitPortStillCheckedForAvailability(t *testing.T) {
	h := newTestServer(t)
	ln := mustListenTest(t)
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost,
		"/api/deploy/kokoro?port="+strconv.Itoa(port), nil))
	if rr.Code < 400 {
		t.Fatalf("accepted a bound port with status %d", rr.Code)
	}
}
