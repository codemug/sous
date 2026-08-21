package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The first frame must arrive immediately. A client that connects should not
// wait a full tick to learn anything, and that frame is what proves the stream
// works at all.
func TestStreamSendsAFrameImmediately(t *testing.T) {
	h := newTestServer(t)
	if rr := post(t, h, "/api/deploy/qwen38", "", ""); rr.Code != http.StatusOK {
		t.Fatalf("deploy: %d", rr.Code)
	}

	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	buf := make([]byte, 2048)
	n, err := resp.Body.Read(buf)
	if err != nil || n == 0 {
		t.Fatalf("no first frame: %v", err)
	}
	frame := string(buf[:n])
	if !strings.HasPrefix(frame, "event: status") {
		t.Errorf("frame = %.80q", frame)
	}

	i := strings.Index(frame, "data: ")
	var p eventPayload
	if err := json.Unmarshal([]byte(strings.SplitN(frame[i+6:], "\n", 2)[0]), &p); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if len(p.Models) == 0 {
		t.Error("the frame carries no models")
	}
	if p.Models[0].Phase == "" {
		t.Error("a model in the frame has no phase")
	}
}

// The payload has to carry what the script patches - the phase, the width and
// the stage. Anything else is weight on a wire that ticks every three seconds.
func TestPayloadCarriesWhatTheScriptPatches(t *testing.T) {
	h := newTestServer(t)
	if rr := post(t, h, "/api/deploy/qwen38", "", ""); rr.Code != http.StatusOK {
		t.Fatalf("deploy: %d", rr.Code)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	frame := string(buf[:n])
	i := strings.Index(frame, "data: ")
	if i < 0 {
		t.Fatalf("no data line: %.120q", frame)
	}
	var p eventPayload
	if err := json.Unmarshal([]byte(strings.SplitN(frame[i+6:], "\n", 2)[0]), &p); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if len(p.Models) == 0 {
		t.Fatal("no models in the frame")
	}
	m := p.Models[0]
	if m.ID == "" || m.Phase == "" {
		t.Errorf("model = %+v", m)
	}
	if m.Pct <= 0 {
		t.Errorf("pct = %v; the script cannot set a width from that", m.Pct)
	}
	// The margin is what the header patches, and it must be a real figure.
	if p.MarginGiB == 0 && p.CommittedGiB == 0 {
		t.Error("the frame carries neither a margin nor a committed figure")
	}
}
